// Package cli 实现命令行入口，把子命令分发到客户端、服务端和管理功能
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"cmdgate/internal/store"

	"golang.org/x/term"
)

// Version 是程序版本，发布时通过 -ldflags 覆盖
var Version = "dev"

const usage = `cmdgate - a gate between AI agents and privileged commands

Usage: cmdgate <command> [arguments]

Client commands (what an agent uses):
  list                               List the actions you may request
  describe <action>                  Show an action's parameters and risk level
  run [flags] <action> [--key value] Request an action
      --timeout <duration>             How long to wait for an auto action (default 90s)
      --no-wait                        Return right after submitting
      --file <param>=<path>            Upload a local file as a file parameter
  status <id>                        Show a request
  wait [--timeout <duration>] <id>   Wait for a request to finish (default 5m)
  cancel <id>                        Cancel a request
  login [--ca <file>] <url>          Use a remote server (reads the token from stdin)
  logout                             Forget the remote server
  whoami                             Show how the server identifies you

Developing actions:
  dev run [--file p=path] <dir> [--key value]
                                     Run an action directory directly, as you, without secrets
  serve --dev                        Run a server as yourself (no isolation, for trying things out)

Administration (needs sudo on an installed system):
  install --mode local|server [--listen addr] [--no-service]
  uninstall [--purge]
  admin sync [--yes] <dir>           Install or update actions from a directory
  admin action list|show <name>|remove <name>
  admin request list|show <id>
  admin approve <id> | admin deny <id>   Emergency decision when the channel is down
  admin audit tail [-n <lines>]
  admin prune --older <duration>     Delete finished requests older than e.g. 30d
  admin export <file> [--with-history] | admin import <file>
  admin doctor                       Check permissions, actions, secrets and channels
  secret set <NAME> | list | rm <NAME>
  channel add <type> [--name <name>] | list | remove <name>
  user add <name> | remove <name> | list     (server mode)

Other:
  serve                              Run the server (started by the system service)
  version

Environment:
  CMDGATE_DATA_DIR                   Data directory (default /opt/cmdgate)
`

// exitError 让命令以指定的退出码结束，msg 为空时不打印任何内容
type exitError struct {
	code int
	msg  string
}

// Error 返回面向调用者的错误说明
func (e *exitError) Error() string { return e.msg }

// Main 是命令行入口
//
// 参数 args 不含程序名，返回进程的退出码
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	err := dispatch(args[0], args[1:])
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, store.Name+": "+ee.msg)
		}
		return ee.code
	}
	fmt.Fprintln(os.Stderr, store.Name+": "+err.Error())
	return 1
}

// dispatch 按命令名调用对应处理函数，未知命令返回用法错误
func dispatch(cmd string, args []string) error {
	switch cmd {
	case "list":
		return cmdList(args)
	case "describe":
		return cmdDescribe(args)
	case "run":
		return cmdRun(args)
	case "status":
		return cmdStatus(args)
	case "wait":
		return cmdWait(args)
	case "cancel":
		return cmdCancel(args)
	case "login":
		return cmdLogin(args)
	case "logout":
		return cmdLogout(args)
	case "whoami":
		return cmdWhoAmI(args)
	case "dev":
		return cmdDev(args)
	case "serve":
		return cmdServe(args)
	case "install":
		return cmdInstall(args)
	case "uninstall":
		return cmdUninstall(args)
	case "admin":
		return cmdAdmin(args)
	case "secret":
		return cmdSecret(args)
	case "channel":
		return cmdChannel(args)
	case "user":
		return cmdUser(args)
	case "version", "--version":
		fmt.Println(store.Name, Version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	return usageErr("unknown command %q, run '%s help'", cmd, store.Name)
}

// usageErr 构造退出码为 2 的命令用法错误
func usageErr(format string, args ...any) error {
	return &exitError{code: 2, msg: fmt.Sprintf(format, args...)}
}

// flags 是一组已解析的命令行选项
type flags struct {
	values map[string][]string
}

// has 返回指定选项是否出现
func (f flags) has(name string) bool { return len(f.values[name]) > 0 }

// get 返回指定选项的值，缺省时使用 def
func (f flags) get(name, def string) string {
	if v := f.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return def
}

// all 返回指定选项的全部值
func (f flags) all(name string) []string { return f.values[name] }

// parseFlags 解析参数开头的选项，遇到第一个非选项参数就停下
//
// 参数 spec 是选项名到是否带值的映射，选项名不含前导的横线
// 返回解析出的选项和剩余参数
// 这里不用标准库 flag，是因为 run 的操作名之后还有任意的 --key value，必须在操作名处停止解析
func parseFlags(args []string, spec map[string]bool) (flags, []string, error) {
	f := flags{values: map[string][]string{}}
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			return f, args[1:], nil
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		name := strings.TrimLeft(arg, "-")
		value, hasValue := "", false
		if i := strings.Index(name, "="); i >= 0 {
			name, value, hasValue = name[:i], name[i+1:], true
		}
		takes, known := spec[name]
		if !known {
			return f, nil, usageErr("unknown option %s", arg)
		}
		args = args[1:]
		switch {
		case !takes:
			value = "true"
		case hasValue:
		case len(args) == 0:
			return f, nil, usageErr("option --%s needs a value", name)
		default:
			value, args = args[0], args[1:]
		}
		f.values[name] = append(f.values[name], value)
	}
	return f, args, nil
}

// parseParams 把操作名之后的 --key value 解析成参数表
//
// 接受 --key value 和 --key=value 两种写法，键里的横线会转成下划线
func parseParams(args []string) (map[string]string, error) {
	params := map[string]string{}
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			return nil, usageErr("unexpected argument %q, parameters are passed as --key value", arg)
		}
		key, value := arg[2:], ""
		if i := strings.Index(key, "="); i >= 0 {
			key, value = key[:i], key[i+1:]
			args = args[1:]
		} else {
			if len(args) < 2 {
				return nil, usageErr("parameter --%s needs a value", key)
			}
			value, args = args[1], args[2:]
		}
		params[strings.ReplaceAll(key, "-", "_")] = value
	}
	return params, nil
}

// parseFileFlags 读取 --file param=path 指定的本地文件
//
// 返回参数名到文件内容的映射
func parseFileFlags(specs []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, spec := range specs {
		name, path, ok := strings.Cut(spec, "=")
		if !ok || name == "" || path == "" {
			return nil, usageErr("--file expects <param>=<path>, got %q", spec)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		files[strings.ReplaceAll(name, "-", "_")] = content
	}
	return files, nil
}

// confirm 在终端上询问是否继续，只有回答 y 或 yes 才返回 true
func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// readSecretInput 读取一个不应回显的值
//
// 标准输入是终端时隐藏输入，否则读取整个标准输入，两种情况都去掉结尾的换行
func readSecretInput(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		data, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return string(data), err
	}
	data, err := io.ReadAll(os.Stdin)
	return strings.TrimRight(string(data), "\r\n"), err
}
