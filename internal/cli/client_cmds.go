package cli

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/api"
	"cmdgate/internal/client"
	"cmdgate/internal/server"
	"cmdgate/internal/store"
)

const (
	// run 等待 auto 操作结束的默认时间，要短于常见 Agent 环境的命令超时
	defaultRunTimeout = 90 * time.Second
	// wait 的默认等待时间
	defaultWaitTimeout = 5 * time.Minute
	// 单次长轮询的时长
	pollInterval = 30 * time.Second

	// 请求被拒绝、过期、取消或结果未知时的退出码
	exitNotExecuted = 2
	// 等待超时、请求仍未结束时的退出码
	exitNotFinished = 3
)

// cmdList 列出当前身份可请求的操作
func cmdList(args []string) error {
	c, err := client.New()
	if err != nil {
		return err
	}
	actions, err := c.List()
	if err != nil {
		return err
	}
	if len(actions) == 0 {
		fmt.Println("no actions available")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tRISK\tDESCRIPTION")
	for _, a := range actions {
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.Name, riskSummary(a.Risk), a.Description)
	}
	return w.Flush()
}

// riskSummary 用一个词概括风险策略，带按参数细分的规则时加上星号
func riskSummary(r action.Risk) string {
	if len(r.Rules) > 0 {
		return r.Default + "*"
	}
	return r.Default
}

// cmdDescribe 显示指定操作的参数和审批策略
func cmdDescribe(args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s describe <action>", store.Name)
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	a, err := c.Describe(args[0])
	if err != nil {
		return err
	}
	printAction(a)
	return nil
}

// printAction 把操作说明格式化到标准输出
func printAction(a *api.ActionView) {
	fmt.Printf("name: %s\n", a.Name)
	if a.Description != "" {
		fmt.Printf("description: %s\n", a.Description)
	}
	fmt.Printf("risk: %s\n", a.Risk.Default)
	for _, r := range a.Risk.Rules {
		var conds []string
		for k, v := range r.When {
			conds = append(conds, k+"="+v)
		}
		sort.Strings(conds)
		fmt.Printf("    %s when %s\n", r.Level, strings.Join(conds, ", "))
	}
	if len(a.Params) == 0 {
		fmt.Println("params: none")
		return
	}
	fmt.Println("params:")
	names := make([]string, 0, len(a.Params))
	for n := range a.Params {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := a.Params[n]
		detail := p.Type
		switch p.Type {
		case "enum":
			detail += " (" + strings.Join(p.Values, " | ") + ")"
		case "string":
			if p.Pattern != "" {
				detail += " matching " + p.Pattern
			}
		case "file":
			detail += fmt.Sprintf(" up to %d bytes, pass with --file %s=<path>", p.MaxSize, n)
		}
		if p.Default != nil {
			detail += fmt.Sprintf(", default %q", *p.Default)
		} else if p.Optional {
			detail += ", optional"
		}
		if p.Description != "" {
			detail += " - " + p.Description
		}
		fmt.Printf("    --%s: %s\n", n, detail)
	}
}

// cmdRun 解析参数和文件并提交操作请求，按选项等待结果
func cmdRun(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"timeout": true, "no-wait": false, "file": true})
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usageErr("usage: %s run [flags] <action> [--key value ...]", store.Name)
	}
	timeout, err := time.ParseDuration(f.get("timeout", defaultRunTimeout.String()))
	if err != nil {
		return usageErr("bad --timeout: %v", err)
	}
	params, err := parseParams(rest[1:])
	if err != nil {
		return err
	}
	files, err := parseFileFlags(f.all("file"))
	if err != nil {
		return err
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	v, err := c.Submit(api.SubmitRequest{Action: rest[0], Params: params, Files: files})
	if err != nil {
		return err
	}
	// 需要审批的请求不在这里等，人可能要过很久才处理
	if v.State != store.StatePending && !f.has("no-wait") {
		v, err = waitFor(c, v.ID, timeout)
		if err != nil {
			return err
		}
	}
	return report(v, false)
}

// cmdStatus 查询并显示指定请求的状态
func cmdStatus(args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s status <id>", store.Name)
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	v, err := c.Get(args[0], 0)
	if err != nil {
		return err
	}
	// status 只是查询，查到了就返回 0，请求本身的结果由 run 和 wait 的退出码表示
	printRequest(v)
	return nil
}

// cmdWait 等待指定请求结束，超时返回对应的退出状态
func cmdWait(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"timeout": true})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErr("usage: %s wait [--timeout <duration>] <id>", store.Name)
	}
	timeout, err := time.ParseDuration(f.get("timeout", defaultWaitTimeout.String()))
	if err != nil {
		return usageErr("bad --timeout: %v", err)
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	v, err := waitFor(c, rest[0], timeout)
	if err != nil {
		return err
	}
	return report(v, true)
}

// cmdCancel 取消当前身份有权取消的请求
func cmdCancel(args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s cancel <id>", store.Name)
	}
	c, err := client.New()
	if err != nil {
		return err
	}
	v, err := c.Cancel(args[0])
	if err != nil {
		return err
	}
	printRequest(v)
	return nil
}

// waitFor 用长轮询等待请求结束，最多等 timeout，返回最后看到的状态
func waitFor(c *client.Client, id string, timeout time.Duration) (*api.RequestView, error) {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		v, err := c.Get(id, max(min(left, pollInterval), 0))
		if err != nil {
			return nil, err
		}
		if store.Terminal(v.State) || time.Until(deadline) <= 0 {
			return v, nil
		}
	}
}

// report 打印请求状态，并按状态决定退出码
//
// 参数 waited 表示调用者明确要求等到结束，此时未结束算作超时
// 成功返回 0，脚本失败返回脚本的退出码，未执行返回 2，等待超时返回 3
func report(v *api.RequestView, waited bool) error {
	printRequest(v)
	switch v.State {
	case store.StateSucceeded:
		return nil
	case store.StateFailed:
		code := 1
		if v.ExitCode != nil && *v.ExitCode > 0 && *v.ExitCode < 256 {
			code = *v.ExitCode
		}
		return &exitError{code: code}
	case store.StatePending, store.StateApproved, store.StateRunning:
		if waited {
			return &exitError{code: exitNotFinished}
		}
		return nil
	}
	return &exitError{code: exitNotExecuted}
}

// printRequest 打印请求状态、审批信息和可见输出
func printRequest(v *api.RequestView) {
	fmt.Printf("id: %s\n", v.ID)
	fmt.Printf("action: %s\n", v.Action)
	fmt.Printf("state: %s\n", v.State)
	fmt.Printf("risk: %s\n", v.Risk)
	if v.Approver != "" {
		fmt.Printf("approver: %s\n", v.Approver)
	}
	if v.ExitCode != nil {
		fmt.Printf("exit_code: %d\n", *v.ExitCode)
	}
	if v.Reason != "" {
		fmt.Printf("reason: %s\n", v.Reason)
	}
	switch v.State {
	case store.StatePending:
		fmt.Printf("expires_at: %s\n", v.ExpiresAt.Local().Format(time.RFC3339))
		fmt.Printf("next: waiting for a human to approve, check with: %s wait %s\n", store.Name, v.ID)
	case store.StateApproved, store.StateRunning:
		fmt.Printf("next: still in progress, check with: %s wait %s\n", store.Name, v.ID)
	}
	if v.Output != "" {
		fmt.Println("--- output ---")
		fmt.Print(v.Output)
		if !strings.HasSuffix(v.Output, "\n") {
			fmt.Println()
		}
	}
}

// cmdLogin 验证远端服务与令牌后保存客户端连接配置
func cmdLogin(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"ca": true})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErr("usage: %s login [--ca <file>] <url>", store.Name)
	}
	token, err := readSecretInput("Token: ")
	if err != nil {
		return err
	}
	cfg := &client.Config{Server: rest[0], Token: strings.TrimSpace(token)}
	if ca := f.get("ca", ""); ca != "" {
		if cfg.CAFile, err = filepath.Abs(ca); err != nil {
			return err
		}
	}
	c, err := client.NewRemote(cfg)
	if err != nil {
		return err
	}
	// 先确认令牌有效再保存
	who, err := c.WhoAmI()
	if err != nil {
		return err
	}
	if err := client.SaveConfig(cfg); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as %s\n", cfg.Server, who.Name)
	return nil
}

// cmdLogout 移除本地保存的远端连接配置
func cmdLogout(args []string) error {
	err := os.Remove(client.ConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("logged out")
	return nil
}

// cmdWhoAmI 显示服务端识别出的当前调用者身份
func cmdWhoAmI(args []string) error {
	c, err := client.New()
	if err != nil {
		return err
	}
	who, err := c.WhoAmI()
	if err != nil {
		return err
	}
	fmt.Printf("name: %s\nadmin: %t\nmode: %s\n", who.Name, who.Admin, who.Mode)
	return nil
}

// cmdDev 分发无需安装的操作开发命令
func cmdDev(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return usageErr("usage: %s dev run [--file p=path] <dir> [--key value ...]", store.Name)
	}
	f, rest, err := parseFlags(args[1:], map[string]bool{"file": true})
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return usageErr("usage: %s dev run [--file p=path] <dir> [--key value ...]", store.Name)
	}
	act, err := action.Load(rest[0])
	if err != nil {
		return err
	}
	given, err := parseParams(rest[1:])
	if err != nil {
		return err
	}
	files, err := parseFileFlags(f.all("file"))
	if err != nil {
		return err
	}
	params, err := act.Validate(given, files)
	if err != nil {
		return err
	}
	return devRun(act, params, files)
}

// devRun 以当前用户直接运行一个操作目录，用来在安装前调试脚本
//
// 不经过服务端，也不从凭证库注入凭证；操作声明的凭证如果已经在当前环境变量里，会原样传给脚本，
// 方便用测试值调试
func devRun(act *action.Action, params map[string]string, files map[string][]byte) error {
	work, err := os.MkdirTemp("", store.Name+"-dev-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + work,
		"CMDGATE_REQUEST_ID=dev",
		"CMDGATE_ACTION=" + act.Name,
		"CMDGATE_REQUESTER=" + os.Getenv("USER"),
	}
	for k, v := range params {
		env = append(env, "CMDGATE_PARAM_"+strings.ToUpper(k)+"="+v)
	}
	for name, content := range files {
		path := filepath.Join(work, "input-"+name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		env = append(env, "CMDGATE_PARAM_"+strings.ToUpper(name)+"="+path)
	}
	fmt.Fprintf(os.Stderr, "risk level for these params: %s\n", act.Level(params))
	run := func(script string, extra []string) error {
		cmd := exec.Command(filepath.Join(act.Dir, script))
		cmd.Dir = work
		cmd.Env = append(env, extra...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	if act.HasPreflight() {
		fmt.Fprintln(os.Stderr, "--- preflight ---")
		if err := run("preflight", nil); err != nil {
			return fmt.Errorf("preflight failed: %w", err)
		}
	}
	var secrets []string
	for _, name := range act.Secrets {
		if v, ok := os.LookupEnv(name); ok {
			secrets = append(secrets, name+"="+v)
		} else {
			fmt.Fprintf(os.Stderr, "note: secret %s is not set in your environment\n", name)
		}
	}
	fmt.Fprintln(os.Stderr, "--- run ---")
	return run("run", secrets)
}

// cmdServe 初始化运行配置并启动服务，收到终止信号时退出
func cmdServe(args []string) error {
	f, _, err := parseFlags(args, map[string]bool{"dev": false})
	if err != nil {
		return err
	}
	st := store.Open()
	if f.has("dev") {
		if os.Getenv(store.EnvDataDir) == "" {
			home, _ := os.UserHomeDir()
			st = &store.Store{Root: filepath.Join(home, "."+store.Name+"-dev")}
		}
		if _, err := os.Stat(st.Path("config.yaml")); os.IsNotExist(err) {
			if err := st.Init(&store.Config{Mode: store.ModeDev}); err != nil {
				return err
			}
		}
		cfg, err := st.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Mode != store.ModeDev {
			return fmt.Errorf("%s is an installed data directory, not a dev one", st.Root)
		}
		fmt.Fprintf(os.Stderr, "dev data directory: %s\nto use it from another shell: export %s=%s\n", st.Root, store.EnvDataDir, st.Root)
	} else if os.Geteuid() == 0 {
		// 以 root 运行会让所有脚本都拿到 root 权限
		return fmt.Errorf("refusing to serve as root, the server must run as the service user")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx, st, log.New(os.Stderr, "", log.LstdFlags))
}
