package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/channel"
	"cmdgate/internal/client"
	"cmdgate/internal/platform"
	"cmdgate/internal/store"
)

// adminStore 打开数据目录并确认当前用户有权管理它
//
// 已安装的系统上数据目录归 root 和服务用户，所以管理命令必须用 sudo；
// dev 模式的数据目录归当前用户，不需要 sudo
func adminStore() (*store.Store, *store.Config, error) {
	st := store.Open()
	cfg, err := st.LoadConfig()
	switch {
	case errors.Is(err, fs.ErrPermission):
		return nil, nil, fmt.Errorf("this command needs sudo")
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil, fmt.Errorf("%s is not initialised, run '%s install' or '%s serve --dev' first", st.Root, store.Name, store.Name)
	case err != nil:
		return nil, nil, err
	}
	if cfg.ServiceUser != "" && os.Geteuid() != 0 {
		return nil, nil, fmt.Errorf("this command needs sudo")
	}
	return st, cfg, nil
}

// adminName 返回执行管理命令的人，用于审计
func adminName() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	return os.Getenv("USER")
}

// cmdAdmin 按子命令分发管理员操作并返回执行错误
func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s admin <sync|action|request|approve|deny|audit|prune|export|import|doctor> ...", store.Name)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "sync":
		return adminSync(rest)
	case "action":
		return adminAction(rest)
	case "request":
		return adminRequest(rest)
	case "approve", "deny":
		return adminDecide(sub == "approve", rest)
	case "audit":
		return adminAudit(rest)
	case "prune":
		return adminPrune(rest)
	case "export":
		return adminExport(rest)
	case "import":
		return adminImport(rest)
	case "doctor":
		return adminDoctor(rest)
	}
	return usageErr("unknown admin command %q", sub)
}

// adminSync 把一个目录里的操作安装到数据目录
//
// 参数可以是单个操作目录，也可以是包含多个操作目录的上级目录
// 全部操作先校验，再显示与已安装版本的差异，确认后才写入并更新 actions.lock
func adminSync(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"yes": false})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErr("usage: %s admin sync [--yes] <dir>", store.Name)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	var dirs []string
	if _, err := os.Stat(filepath.Join(rest[0], "action.yaml")); err == nil {
		dirs = []string{rest[0]}
	} else {
		entries, err := os.ReadDir(rest[0])
		if err != nil {
			return err
		}
		for _, e := range entries {
			dir := filepath.Join(rest[0], e.Name())
			if _, err := os.Stat(filepath.Join(dir, "action.yaml")); err == nil {
				dirs = append(dirs, dir)
			}
		}
	}
	if len(dirs) == 0 {
		return fmt.Errorf("no action.yaml found under %s", rest[0])
	}
	lock, err := st.LoadLock()
	if err != nil {
		return err
	}
	var changed []*action.Action
	seen := map[string]string{}
	for _, dir := range dirs {
		act, err := action.Load(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		if prev, dup := seen[act.Name]; dup {
			return fmt.Errorf("action %q is defined in both %s and %s", act.Name, prev, dir)
		}
		seen[act.Name] = dir
		installed := st.Path("actions", act.Name)
		current, _ := action.Hash(installed)
		if lock[act.Name].Hash == act.Hash && current == act.Hash {
			fmt.Printf("unchanged: %s\n", act.Name)
			continue
		}
		if _, err := os.Stat(installed); err == nil {
			fmt.Printf("changed: %s\n", act.Name)
			// diff 在有差异时退出码为 1，这不是错误
			out, _ := exec.Command("diff", "-ruN", installed, act.Dir).CombinedOutput()
			fmt.Print(string(out))
		} else {
			fmt.Printf("new: %s\n", act.Name)
			filepath.WalkDir(act.Dir, func(path string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(act.Dir, path)
					fmt.Printf("    + %s\n", rel)
				}
				return nil
			})
		}
		fmt.Printf("    hash: %s\n", act.Hash)
		changed = append(changed, act)
	}
	if len(changed) == 0 {
		return nil
	}
	if !f.has("yes") && !confirm(fmt.Sprintf("Install %d action(s)?", len(changed))) {
		return fmt.Errorf("aborted")
	}
	secrets := secretSet(st)
	for _, act := range changed {
		// 先复制到临时目录再替换，避免服务端读到写了一半的操作
		dst := st.Path("actions", act.Name)
		tmp := st.Path("actions", ".tmp-"+act.Name)
		os.RemoveAll(tmp)
		if err := copyTree(act.Dir, tmp); err != nil {
			return err
		}
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		lock[act.Name] = store.LockEntry{Hash: act.Hash, InstalledAt: time.Now()}
		if err := st.SaveLock(lock); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "action_installed", Action: act.Name, Hash: act.Hash, Requester: adminName()})
		fmt.Printf("installed: %s\n", act.Name)
		for _, s := range act.Secrets {
			if !secrets[s] {
				fmt.Printf("    warning: secret %s is not set, run: %s secret set %s\n", s, store.Name, s)
			}
		}
	}
	return st.FixPerms()
}

// copyTree 复制目录，保留文件的可执行位
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// secretSet 返回已设置的凭证名集合
func secretSet(st *store.Store) map[string]bool {
	set := map[string]bool{}
	list, _ := st.ListSecrets()
	for _, s := range list {
		set[s.Name] = true
	}
	return set
}

// installedActions 加载全部已安装的操作，加载失败的以错误形式一并返回
func installedActions(st *store.Store) ([]*action.Action, []error) {
	entries, _ := os.ReadDir(st.Path("actions"))
	var acts []*action.Action
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		act, err := action.Load(st.Path("actions", e.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("action %s: %w", e.Name(), err))
			continue
		}
		acts = append(acts, act)
	}
	return acts, errs
}

// adminAction 列出、查看或移除已安装操作
func adminAction(args []string) error {
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return usageErr("usage: %s admin action list|show <name>|remove <name>", store.Name)
	}
	switch {
	case args[0] == "list":
		acts, errs := installedActions(st)
		lock, _ := st.LoadLock()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tRISK\tHASH\tINSTALLED\tSECRETS")
		for _, a := range acts {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Name, riskSummary(a.Risk), a.Hash[:12],
				lock[a.Name].InstalledAt.Local().Format("2006-01-02 15:04"), strings.Join(a.Secrets, ","))
		}
		w.Flush()
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "broken:", e)
		}
		return nil
	case args[0] == "show" && len(args) == 2 && action.ValidName(args[1]):
		dir := st.Path("actions", args[1])
		data, err := os.ReadFile(filepath.Join(dir, "action.yaml"))
		if err != nil {
			return err
		}
		hash, _ := action.Hash(dir)
		fmt.Printf("# %s\n# hash: %s\n%s", dir, hash, data)
		return nil
	case args[0] == "remove" && len(args) == 2 && action.ValidName(args[1]):
		if err := os.RemoveAll(st.Path("actions", args[1])); err != nil {
			return err
		}
		lock, err := st.LoadLock()
		if err != nil {
			return err
		}
		delete(lock, args[1])
		if err := st.SaveLock(lock); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "action_removed", Action: args[1], Requester: adminName()})
		fmt.Printf("removed: %s\n", args[1])
		return st.FixPerms()
	}
	return usageErr("usage: %s admin action list|show <name>|remove <name>", store.Name)
}

// adminRequest 列出或查看持久化的请求记录
func adminRequest(args []string) error {
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	switch {
	case len(args) == 1 && args[0] == "list":
		reqs, err := st.ListRequests()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tCREATED\tACTION\tREQUESTER\tSTATE\tAPPROVER")
		for _, r := range reqs {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.CreatedAt.Local().Format("2006-01-02 15:04:05"), r.Action, r.Requester, r.State, r.Approver)
		}
		return w.Flush()
	case len(args) == 2 && args[0] == "show":
		r, err := st.LoadRequest(args[1])
		if err != nil {
			return fmt.Errorf("request %s not found", args[1])
		}
		// 随机数是审批的凭据，不显示
		r.Nonce = ""
		data, _ := json.MarshalIndent(r, "", "    ")
		fmt.Println(string(data))
		if out, err := os.ReadFile(filepath.Join(st.RequestDir(r.ID), "output.log")); err == nil {
			fmt.Printf("--- output ---\n%s", out)
		}
		return nil
	}
	return usageErr("usage: %s admin request list|show <id>", store.Name)
}

// adminDecide 是渠道不可用时的应急入口，直接向本机服务端提交决定
//
// 服务端只接受来自 root 或服务端自身用户的调用
func adminDecide(approve bool, args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s admin approve|deny <id>", store.Name)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	v, err := client.NewLocal(st.Socket()).AdminDecide(args[0], approve)
	if err != nil {
		return err
	}
	printRequest(v)
	return nil
}

// adminAudit 按参数读取并打印最近的审计日志
func adminAudit(args []string) error {
	if len(args) == 0 || args[0] != "tail" {
		return usageErr("usage: %s admin audit tail [-n <lines>]", store.Name)
	}
	f, _, err := parseFlags(args[1:], map[string]bool{"n": true})
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(f.get("n", "20"))
	if err != nil || n <= 0 {
		return usageErr("bad -n value")
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	lines, err := st.AuditTail(n)
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// parseAge 解析时长，在标准写法之外支持以 d 结尾的天数，如 30d
func parseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// adminPrune 清理早于指定时长的终态请求，保留审计日志
func adminPrune(args []string) error {
	f, _, err := parseFlags(args, map[string]bool{"older": true})
	if err != nil {
		return err
	}
	if !f.has("older") {
		return usageErr("usage: %s admin prune --older <duration>", store.Name)
	}
	age, err := parseAge(f.get("older", ""))
	if err != nil {
		return usageErr("%v", err)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	reqs, err := st.ListRequests()
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, r := range reqs {
		// 只清理已结束的请求，审计日志不受影响
		if store.Terminal(r.State) && r.FinishedAt != nil && r.FinishedAt.Before(cutoff) {
			if err := os.RemoveAll(st.RequestDir(r.ID)); err != nil {
				return err
			}
			removed++
		}
	}
	fmt.Printf("removed %d request(s)\n", removed)
	return nil
}

// adminDoctor 检查安装是否健康：目录权限、操作与安装记录是否一致、凭证是否齐全、
// 渠道是否连通、服务端是否在运行
func adminDoctor(args []string) error {
	st, cfg, err := adminStore()
	if err != nil {
		return err
	}
	problems := 0
	check := func(name string, issues []string) {
		if len(issues) == 0 {
			fmt.Printf("ok       %s\n", name)
			return
		}
		problems += len(issues)
		fmt.Printf("PROBLEM  %s\n", name)
		for _, i := range issues {
			fmt.Printf("         - %s\n", i)
		}
	}
	asIssues := func(err error) []string {
		if err == nil {
			return nil
		}
		return []string{err.Error()}
	}

	check("data layout version", asIssues(st.CheckVersion()))

	perms, err := st.CheckPerms()
	check("ownership and permissions", append(perms, asIssues(err)...))

	acts, errs := installedActions(st)
	lock, _ := st.LoadLock()
	secrets := secretSet(st)
	var actionIssues, secretIssues []string
	for _, e := range errs {
		actionIssues = append(actionIssues, e.Error())
	}
	for _, a := range acts {
		if lock[a.Name].Hash != a.Hash {
			actionIssues = append(actionIssues, fmt.Sprintf("action %s differs from its installed hash", a.Name))
		}
		for _, s := range a.Secrets {
			if !secrets[s] {
				secretIssues = append(secretIssues, fmt.Sprintf("action %s needs secret %s, which is not set", a.Name, s))
			}
		}
	}
	check(fmt.Sprintf("actions (%d installed)", len(acts)), actionIssues)
	check("secrets referenced by actions", secretIssues)

	var channelIssues []string
	for name, c := range cfg.Channels {
		driver, err := channel.Lookup(c.Type)
		if err != nil {
			channelIssues = append(channelIssues, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		ch, err := driver.Open(name, c, st.GetSecret)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = ch.Ping(ctx)
			cancel()
		}
		if err != nil {
			channelIssues = append(channelIssues, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if len(cfg.Channels) == 0 {
		channelIssues = append(channelIssues, fmt.Sprintf("no approval channel configured, run: %s channel add telegram", store.Name))
	}
	check(fmt.Sprintf("approval channels (%d configured)", len(cfg.Channels)), channelIssues)

	_, err = client.NewLocal(st.Socket()).WhoAmI()
	check("server reachable on "+st.Socket(), asIssues(err))

	if problems > 0 {
		return &exitError{code: 1, msg: fmt.Sprintf("%d problem(s) found", problems)}
	}
	return nil
}

// cmdSecret 管理凭证，列表只展示元信息
func cmdSecret(args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s secret set <NAME>|list|rm <NAME>", store.Name)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "set" && len(args) == 2:
		value, err := readSecretInput("Value for " + args[1] + ": ")
		if err != nil {
			return err
		}
		if value == "" {
			return fmt.Errorf("empty value, nothing saved")
		}
		if err := st.SetSecret(args[1], value); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "secret_set", Detail: args[1], Requester: adminName()})
		fmt.Printf("saved: %s\n", args[1])
		return st.FixPerms()
	case args[0] == "list" && len(args) == 1:
		list, err := st.ListSecrets()
		if err != nil {
			return err
		}
		usedBy := map[string][]string{}
		acts, _ := installedActions(st)
		for _, a := range acts {
			for _, s := range a.Secrets {
				usedBy[s] = append(usedBy[s], a.Name)
			}
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tUPDATED\tUSED BY")
		for _, s := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, s.UpdatedAt.Local().Format("2006-01-02 15:04"), strings.Join(usedBy[s.Name], ","))
		}
		return w.Flush()
	case args[0] == "rm" && len(args) == 2:
		if err := st.DeleteSecret(args[1]); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "secret_removed", Detail: args[1], Requester: adminName()})
		fmt.Printf("removed: %s\n", args[1])
		return st.FixPerms()
	}
	return usageErr("usage: %s secret set <NAME>|list|rm <NAME>", store.Name)
}

// stdioPrompter 通过终端与管理员交互，供渠道配置向导使用
type stdioPrompter struct {
	in *bufio.Reader
}

// Say 向标准错误输出格式化的向导消息
func (p *stdioPrompter) Say(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// Ask 读取一行回答，空回答使用 def，读取失败时返回错误
func (p *stdioPrompter) Ask(prompt, def string) (string, error) {
	if def != "" {
		prompt += " [" + def + "]"
	}
	fmt.Fprint(os.Stderr, prompt+": ")
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return def, nil
	}
	return line, nil
}

// AskSecret 读取敏感回答，交互式终端中关闭回显
func (p *stdioPrompter) AskSecret(prompt string) (string, error) {
	// 标准输入不是终端时按行读取，这样向导后面的问题还能继续从同一个输入里读
	if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice == 0 {
		line, err := p.in.ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	return readSecretInput(prompt)
}

// cmdChannel 管理审批渠道并按需应用配置
func cmdChannel(args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s channel add <type> [--name <name>]|list|remove <name>", store.Name)
	}
	st, cfg, err := adminStore()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "add" && len(args) >= 2:
		typ := args[1]
		f, _, err := parseFlags(args[2:], map[string]bool{"name": true})
		if err != nil {
			return err
		}
		driver, err := channel.Lookup(typ)
		if err != nil {
			return err
		}
		// 名称、覆盖确认和渠道向导共用输入缓冲，避免管道输入被提前读走
		p := &stdioPrompter{in: bufio.NewReader(os.Stdin)}
		name, err := p.Ask("Channel name", f.get("name", typ))
		if err != nil {
			return err
		}
		if existing, ok := cfg.Channels[name]; ok {
			answer, err := p.Ask(fmt.Sprintf("Channel %q already exists (type %s). Replace its configuration and credentials? y/N", name, existing.Type), "n")
			if err != nil {
				return err
			}
			if answer = strings.ToLower(answer); answer != "y" && answer != "yes" {
				return fmt.Errorf("channel %s unchanged: replacement canceled", name)
			}
		}
		result, err := driver.Setup(context.Background(), name, p)
		if err != nil {
			return err
		}
		for k, v := range result.Secrets {
			if err := st.SetSecret(k, v); err != nil {
				return err
			}
		}
		if cfg.Channels == nil {
			cfg.Channels = map[string]store.ChannelConfig{}
		}
		cfg.Channels[name] = result.Config
		if err := st.SaveConfig(cfg); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "channel_added", Detail: name, Requester: adminName()})
		if err := st.FixPerms(); err != nil {
			return err
		}
		fmt.Printf("channel %s saved\n", name)
		return applyConfig()
	case args[0] == "list" && len(args) == 1:
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tTYPE\tAPPROVERS")
		for name, c := range cfg.Channels {
			var approvers []string
			for alias, id := range c.Approvers {
				approvers = append(approvers, alias+"="+id)
			}
			sort.Strings(approvers)
			fmt.Fprintf(w, "%s\t%s\t%s\n", name, c.Type, strings.Join(approvers, ","))
		}
		return w.Flush()
	case args[0] == "remove" && len(args) == 2:
		c, ok := cfg.Channels[args[1]]
		if !ok {
			return fmt.Errorf("channel %s not found", args[1])
		}
		if s := c.Settings["token_secret"]; s != "" {
			st.DeleteSecret(s)
		}
		delete(cfg.Channels, args[1])
		if err := st.SaveConfig(cfg); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "channel_removed", Detail: args[1], Requester: adminName()})
		if err := st.FixPerms(); err != nil {
			return err
		}
		fmt.Printf("channel %s removed\n", args[1])
		return applyConfig()
	}
	return usageErr("usage: %s channel add <type> [--name <name>]|list|remove <name>", store.Name)
}

// applyConfig 让运行中的服务端读到新的配置
//
// 服务端只在启动时读取渠道配置，所以装成系统服务时直接重启，否则提示手动重启
func applyConfig() error {
	if platform.ServiceInstalled() && os.Geteuid() == 0 {
		if err := platform.RestartService(); err != nil {
			return fmt.Errorf("saved, but the service could not be restarted: %w", err)
		}
		fmt.Println("service restarted")
		return nil
	}
	fmt.Println("restart the server to apply the change")
	return nil
}

// cmdUser 管理远端用户及访问令牌
func cmdUser(args []string) error {
	if len(args) == 0 {
		return usageErr("usage: %s user add <name>|remove <name>|list", store.Name)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	users, err := st.LoadUsers()
	if err != nil {
		return err
	}
	switch {
	case args[0] == "add" && len(args) == 2:
		name := args[1]
		for _, u := range users {
			if u.Name == name {
				return fmt.Errorf("user %s already exists, remove it first to issue a new token", name)
			}
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := "cg_" + base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256([]byte(token))
		users = append(users, store.User{Name: name, TokenHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now()})
		if err := st.SaveUsers(users); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "user_added", Detail: name, Requester: adminName()})
		fmt.Fprintf(os.Stderr, "token for %s, shown once, give it to them for '%s login':\n", name, store.Name)
		fmt.Println(token)
		return st.FixPerms()
	case args[0] == "remove" && len(args) == 2:
		kept := users[:0]
		for _, u := range users {
			if u.Name != args[1] {
				kept = append(kept, u)
			}
		}
		if len(kept) == len(users) {
			return fmt.Errorf("user %s not found", args[1])
		}
		if err := st.SaveUsers(kept); err != nil {
			return err
		}
		st.Audit(store.Event{Event: "user_removed", Detail: args[1], Requester: adminName()})
		fmt.Printf("removed: %s\n", args[1])
		return st.FixPerms()
	case args[0] == "list" && len(args) == 1:
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tCREATED")
		for _, u := range users {
			fmt.Fprintf(w, "%s\t%s\n", u.Name, u.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		return w.Flush()
	}
	return usageErr("usage: %s user add <name>|remove <name>|list", store.Name)
}
