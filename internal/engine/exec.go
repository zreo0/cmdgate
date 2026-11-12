package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"
)

const (
	// 单次执行保留的输出上限，超出部分丢弃
	maxOutputBytes = 4 << 20
	// 返回给调用者的输出上限，保留末尾
	maxViewBytes = 256 << 10
	// preflight 的固定超时
	preflightTimeout = 60 * time.Second
	// 进程收到终止信号后的宽限时间
	killGrace = 5 * time.Second
	// 输出截断标记在脱敏完成后追加
	truncatedMarker = "\n[output truncated]\n"

	defaultPath = "/usr/bin:/bin:/usr/sbin:/sbin"
)

// limitedWriter 写入文件直到达到上限，之后的内容丢弃并记下发生过截断
type limitedWriter struct {
	f         *os.File
	left      int
	truncated bool
}

// Write 写入输出上限内的内容，超出部分丢弃并记录截断状态
func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		w.truncated = true
		if w.left > 0 {
			w.f.Write(p[:w.left])
			w.left = 0
		}
		// 向上报告全部写入，避免脚本因为管道写失败而异常退出
		return len(p), nil
	}
	w.left -= len(p)
	n, err := w.f.Write(p)
	if n != len(p) || err != nil {
		w.truncated = true
	}
	return n, err
}

// runScript 直接启动脚本并等待结束，不拼接 shell 命令字符串
//
// 脚本自身仍由 shebang 指定的解释器执行
//
// 参数 path 是脚本路径，dir 是工作目录，env 是完整的环境变量，outPath 是输出文件
// 返回退出码、输出是否截断以及错误；无法启动或被信号终止时退出码为 -1
func runScript(ctx context.Context, path, dir string, env []string, outPath string) (int, bool, error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return -1, false, err
	}
	defer f.Close()
	out := &limitedWriter{f: f, left: maxOutputBytes}

	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	// 让脚本和它的子进程在同一个进程组里，超时或取消时可以整组终止
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = killGrace

	err = cmd.Run()
	if cmd.Process != nil {
		// 宽限期后仍有残留子进程时强制清理
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode(), out.truncated, nil
	}
	if err != nil {
		return -1, out.truncated, err
	}
	return 0, out.truncated, nil
}

// redact 把输出里出现的凭证值替换成占位符
//
// 参数 secrets 是凭证名到值的映射
// 先替换较长的值，避免一个凭证是另一个的子串时只替换掉一部分
func redact(data []byte, secrets map[string]string, truncated bool) []byte {
	names := make([]string, 0, len(secrets))
	for name, value := range secrets {
		if len(value) > 0 {
			names = append(names, name)
		}
	}
	// 原始输出的截断点可能落在凭证中间，连同残留前缀一起隐藏
	cut := len(data)
	if truncated {
		for _, name := range names {
			value := secrets[name]
			for n := min(len(data), len(value)-1); n > 0; n-- {
				if bytes.Equal(data[len(data)-n:], []byte(value[:n])) {
					cut = min(cut, len(data)-n)
					break
				}
			}
		}
	}
	// 截断前缀可能与另一个完整凭证重叠，向前合并这些区间，不能裁断完整匹配
	for {
		previous := cut
		for _, name := range names {
			value := []byte(secrets[name])
			start := max(0, cut-len(value)+1)
			end := min(len(data), cut+len(value)-1)
			if start < end {
				if offset := bytes.Index(data[start:end], value); offset >= 0 {
					cut = min(cut, start+offset)
				}
			}
		}
		if cut == previous {
			break
		}
	}
	if cut < len(data) {
		data = append(data[:cut:cut], []byte("[REDACTED:TRUNCATED]")...)
	}
	sort.Slice(names, func(i, j int) bool { return len(secrets[names[i]]) > len(secrets[names[j]]) })
	for _, name := range names {
		data = bytes.ReplaceAll(data, []byte(secrets[name]), []byte("[REDACTED:"+name+"]"))
	}
	if truncated {
		data = append(data, truncatedMarker...)
	}
	return data
}

// tail 返回内容末尾最多 n 个字节，发生截断时在开头注明
func tail(data []byte, n int) string {
	if len(data) <= n {
		return string(data)
	}
	return "[earlier output truncated]\n" + string(data[len(data)-n:])
}
