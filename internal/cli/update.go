package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"cmdgate/internal/platform"

	"golang.org/x/sys/unix"
)

// stableVersion 与发布脚本使用相同的稳定版本格式，不接受预发布版本或路径
var stableVersion = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

const (
	// releaseBase 固定官方发布源，避免提权后使用调用方指定的下载地址
	releaseBase = "https://github.com/zreo0/cmdgate/releases"
	// maxReleaseSize 限制下载和解压大小，防止异常发布包耗尽磁盘
	maxReleaseSize = 128 << 20
)

// cmdUpdate 校验并更新已安装的系统二进制，保留配置和数据，重启已注册的服务
func cmdUpdate(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"version": true})
	if err != nil {
		return err
	}
	version := f.get("version", "latest")
	if len(rest) != 0 || (version != "latest" && !stableVersion.MatchString(version)) {
		return usageErr("usage: cmdgate update [--version vX.Y.Z]")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("update needs sudo")
	}
	if err := platform.EnsureTrustedDir(filepath.Dir(platform.BinPath)); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: releaseRedirect}
	var restart func() error
	if platform.ServiceInstalled() {
		restart = platform.RestartService
	}
	fmt.Println("Downloading and verifying release...")
	installed, err := updateBinary(ctx, client, releaseBase, version, platform.BinPath, restart)
	if err != nil {
		return err
	}
	fmt.Printf("updated: %s (%s)\n", platform.BinPath, installed)
	if restart == nil {
		fmt.Println("service: not registered; restart any manually managed server yourself")
	} else {
		fmt.Println("service: restarted")
	}
	return nil
}

// releaseRedirect 只允许 HTTPS 跳转并限制次数，保留 GitHub 发布资源的 CDN 跳转
func releaseRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("release redirect must use HTTPS")
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many release redirects")
	}
	return nil
}

// releaseRequest 发起 HTTPS 下载，返回由调用方关闭的成功响应
func releaseRequest(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("release URL must use HTTPS")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return resp, nil
}

// downloadRelease 将有限大小的响应写入 dst，下载失败或超限时返回错误
func downloadRelease(ctx context.Context, client *http.Client, url string, dst io.Writer, limit int64) error {
	resp, err := releaseRequest(ctx, client, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > limit {
		return fmt.Errorf("download %s exceeds size limit", url)
	}
	return nil
}

// updateBinary 在受信任的安装目录中暂存发布包，校验后原子替换，再调用 restart
//
// 调用方负责验证目录权限；restart 为 nil 时不管理服务，返回实际安装的版本
// 目录锁覆盖下载至重启，防止两个 update 交错替换；关闭文件自动释放锁
func updateBinary(ctx context.Context, client *http.Client, base, version, target string, restart func() error) (string, error) {
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", fmt.Errorf("lock installation directory (another update may be running): %w", err)
	}
	info, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("inspect installed binary (run install first): %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("installed binary must be a regular file: %s", target)
	}
	if version == "latest" {
		resp, err := releaseRequest(ctx, client, base+"/latest")
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		url := resp.Request.URL.String()
		if !strings.HasPrefix(url, base+"/tag/") {
			return "", fmt.Errorf("cannot resolve latest stable release: %s", url)
		}
		version = strings.TrimPrefix(url, base+"/tag/")
	}
	if !stableVersion.MatchString(version) {
		return "", fmt.Errorf("invalid stable version: %s", version)
	}
	archive := fmt.Sprintf("cmdgate_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	base += "/download/" + version + "/"
	var sums bytes.Buffer
	if err := downloadRelease(ctx, client, base+"SHA256SUMS", &sums, 1<<20); err != nil {
		return "", err
	}
	expected, err := releaseChecksum(sums.String(), archive)
	if err != nil {
		return "", err
	}
	// 暂存目录位于 root 管理的安装目录内，且只允许 root 访问，校验后的内容不会被普通用户替换
	work, err := os.MkdirTemp(filepath.Dir(target), ".cmdgate-update-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	file, err := os.Create(filepath.Join(work, "release.tar.gz"))
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if err := downloadRelease(ctx, client, base+archive, io.MultiWriter(file, hash), maxReleaseSize); err != nil {
		return "", err
	}
	if !bytes.Equal(hash.Sum(nil), expected) {
		return "", fmt.Errorf("checksum mismatch for %s; nothing was installed", archive)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	binary := filepath.Join(work, "cmdgate")
	if err := extractRelease(file, binary); err != nil {
		return "", fmt.Errorf("extract release: %w", err)
	}
	// 取消发生在替换前时保留旧程序；替换后必须继续尝试重启，避免运行版本与磁盘版本不一致
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(binary, target); err != nil {
		return "", fmt.Errorf("replace installed binary: %w", err)
	}
	if restart != nil {
		if err := restart(); err != nil {
			return version, fmt.Errorf("installed %s, but service restart failed; fix the service and restart it manually: %w", version, err)
		}
	}
	return version, nil
}

// releaseChecksum 提取唯一匹配的 SHA256SUMS 记录，拒绝缺失、重复和格式不正确的值
func releaseChecksum(sums, archive string) ([]byte, error) {
	var checksum []byte
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != archive {
			continue
		}
		value, err := hex.DecodeString(fields[0])
		if checksum != nil || len(fields) != 2 || err != nil || len(value) != sha256.Size {
			return nil, fmt.Errorf("invalid or duplicate checksum for %s", archive)
		}
		checksum = value
	}
	if checksum == nil {
		return nil, fmt.Errorf("missing checksum for %s", archive)
	}
	return checksum, nil
}

// extractRelease 仅写出唯一的普通 cmdgate 文件，忽略归档内其他路径且限制总解压大小
func extractRelease(src io.Reader, target string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	limited := &io.LimitedReader{R: gz, N: maxReleaseSize + 1}
	tr := tar.NewReader(limited)
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer dst.Close()
	found := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name != "cmdgate" {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxReleaseSize {
			return fmt.Errorf("release must contain one nonempty regular cmdgate binary")
		}
		found = true
		if _, err := io.Copy(dst, tr); err != nil {
			return err
		}
	}
	// 读到 gzip 尾部才能验证压缩流校验和，tar 的结束标记可能早于压缩流结束
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if !found || limited.N == 0 {
		return fmt.Errorf("release is missing cmdgate or exceeds size limit")
	}
	if err := dst.Chmod(0o755); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	return dst.Close()
}
