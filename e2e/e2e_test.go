// Package e2e 用真实的二进制跑完整链路
//
// 这里的测试都以当前用户运行（dev 模式），覆盖命令行、服务端、审批渠道和存储之间的配合
// 独立用户下的权限隔离由 isolation_test.go 在容器里验证
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"cmdgate/internal/store"
	"cmdgate/internal/testutil/faketg"
)

const (
	// 假 Telegram 里审批人和陌生人的用户 ID
	ownerID    = 42
	strangerID = 99
	// 测试用的凭证值
	deployToken = "tok-0123456789abcdef"
)

// bin 是 TestMain 编译出的二进制路径
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cmdgate-bin-")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "cmdgate")
	// 容器里源码目录的属主与当前用户不同，关掉版本信息嵌入以免 git 报错
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/cmdgate")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuffer 是带锁的缓冲区，服务端进程写日志的同时测试可以安全地读
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env 是一套独立的测试环境：数据目录、个人目录、假 Telegram 和服务端进程
type env struct {
	t    *testing.T
	data string
	home string
	tg   *faketg.Server
	srv  *exec.Cmd
	log  *syncBuffer
	// 额外的环境变量
	extra []string
	// 为 true 时不设置数据目录，客户端按登录配置连接远端
	remote bool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	// unix socket 的路径有长度上限，测试默认的临时目录太长，所以放在 /tmp 下
	data, err := os.MkdirTemp("/tmp", "cg-")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, data: data, home: t.TempDir(), tg: faketg.New(), log: &syncBuffer{}}
	t.Cleanup(func() {
		e.stopServer()
		e.tg.Close()
		os.RemoveAll(data)
		if t.Failed() {
			t.Logf("server log:\n%s", e.log.String())
		}
	})
	return e
}

func (e *env) command(args ...string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + e.home,
		"USER=tester",
		"CMDGATE_TELEGRAM_API_BASE=" + e.tg.URL,
	}, e.extra...)
	if !e.remote {
		cmd.Env = append(cmd.Env, store.EnvDataDir+"="+e.data)
	}
	return cmd
}

// runIn 执行一条命令并返回输出和退出码，stdin 是喂给它的标准输入
func (e *env) runIn(stdin string, args ...string) (string, int) {
	e.t.Helper()
	cmd := e.command(args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			e.t.Fatalf("%v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

func (e *env) run(args ...string) (string, int) {
	e.t.Helper()
	return e.runIn("", args...)
}

// ok 执行一条必须成功的命令并返回输出
func (e *env) ok(args ...string) string {
	e.t.Helper()
	out, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("%v exited %d:\n%s", args, code, out)
	}
	return out
}

func (e *env) startServer(args ...string) {
	e.t.Helper()
	e.srv = e.command(append([]string{"serve"}, args...)...)
	e.srv.Stdout = e.log
	e.srv.Stderr = e.log
	if err := e.srv.Start(); err != nil {
		e.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, code := e.run("whoami"); code == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("server did not come up:\n%s", e.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *env) stopServer() {
	if e.srv == nil || e.srv.Process == nil {
		return
	}
	e.srv.Process.Signal(syscall.SIGTERM)
	e.srv.Wait()
	e.srv = nil
}

var idRe = regexp.MustCompile(`(?m)^id: ([0-9a-f]+)$`)

// submit 发起一个需要审批的请求，返回请求 ID
func (e *env) submit(args ...string) string {
	e.t.Helper()
	out := e.ok(append([]string{"run"}, args...)...)
	if !strings.Contains(out, "state: pending_approval") {
		e.t.Fatalf("expected a pending request:\n%s", out)
	}
	return idRe.FindStringSubmatch(out)[1]
}

// approvalMessage 等待某个请求的审批消息出现在假 Telegram 里
func (e *env) approvalMessage(id string) faketg.Message {
	e.t.Helper()
	msg, found := e.tg.WaitFor(5*time.Second, func(m faketg.Message) bool {
		return len(m.Buttons) == 2 && strings.Contains(m.Text, "Request: "+id)
	})
	if !found {
		e.t.Fatalf("no approval message for %s, messages: %+v", id, e.tg.Messages())
	}
	return msg
}

// waitRejected 等待指定请求和审批人的拒绝审计，保证回调已被处理后再断言没有执行
func (e *env) waitRejected(id, approver string) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines, err := (&store.Store{Root: e.data}).AuditTail(200)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, line := range lines {
			var event store.Event
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				e.t.Fatal(err)
			}
			if event.Event == "decision_rejected" && event.RequestID == id && event.Approver == approver {
				return
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no rejection for request %s by %s", id, approver)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// respondToTelegramSetup 扮演管理员完成 Bot 的私聊和测试按钮确认
func (e *env) respondToTelegramSetup() {
	e.t.Helper()
	go func() {
		// 以服务端收到轮询请求为准，不依赖命令启动速度
		if !e.tg.WaitForMessagePoll(10 * time.Second) {
			return
		}
		e.tg.UserMessage(ownerID, "hello bot")
		msg, found := e.tg.WaitFor(10*time.Second, func(m faketg.Message) bool {
			return len(m.Buttons) == 1 && strings.HasPrefix(m.Buttons[0], "t:")
		})
		if found {
			e.tg.Click(ownerID, msg.Buttons[0])
		}
	}()
}

// setupTelegram 接受默认渠道名并完成 Telegram 配置向导
func (e *env) setupTelegram() {
	e.t.Helper()
	e.respondToTelegramSetup()
	out, code := e.runIn("\n"+faketg.Token+"\ny\nowner\n", "channel", "add", "telegram")
	if code != 0 || !strings.Contains(out, "channel telegram saved") {
		e.t.Fatalf("channel add exited %d:\n%s", code, out)
	}
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("output does not contain %q:\n%s", w, out)
		}
	}
}

// writeAction 在临时目录里生成一个操作目录
func writeAction(t *testing.T, name, yaml, script string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yaml"), []byte("name: "+name+"\n"+yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newReadyEnv 返回一套装好示例操作、凭证和 Telegram 渠道并已启动的环境
func newReadyEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.startServer("--dev")
	e.ok("admin", "sync", "--yes", "../examples/actions")
	if out, code := e.runIn(deployToken+"\n", "secret", "set", "DEPLOY_TOKEN"); code != 0 {
		t.Fatalf("secret set: %s", out)
	}
	e.setupTelegram()
	// 渠道配置在服务端启动时读取
	e.stopServer()
	e.startServer("--dev")
	return e
}

func TestAutoActions(t *testing.T) {
	e := newEnv(t)
	e.startServer("--dev")
	out := e.ok("admin", "sync", "--yes", "../examples/actions")
	mustContain(t, out, "installed: hello", "installed: deploy", "warning: secret DEPLOY_TOKEN is not set")

	mustContain(t, e.ok("list"), "hello", "deploy", "approve*")
	mustContain(t, e.ok("describe", "deploy"), "--env: enum (staging | prod)", "auto when env=staging")
	mustContain(t, e.ok("run", "hello", "--name", "bob"), "state: succeeded", "hello, bob")
	mustContain(t, e.ok("run", "hello"), "hello, world")

	// 不符合参数规则的调用在执行前就被拒绝
	if out, code := e.run("run", "hello", "--name", "x; rm -rf /"); code == 0 || !strings.Contains(out, "does not match") {
		t.Fatalf("bad param accepted (exit %d):\n%s", code, out)
	}
	if out, code := e.run("run", "hello", "--force", "1"); code == 0 || !strings.Contains(out, "unknown param") {
		t.Fatalf("unknown param accepted (exit %d):\n%s", code, out)
	}
	if _, code := e.run("run", "no-such-action"); code == 0 {
		t.Fatal("unknown action accepted")
	}

	// 凭证没设置时操作失败，设置后成功，输出里的凭证值被替换
	if out, code := e.run("run", "deploy", "--env", "staging", "--version", "1.2.3"); code == 0 || !strings.Contains(out, "DEPLOY_TOKEN is not set") {
		t.Fatalf("ran without its secret (exit %d):\n%s", code, out)
	}
	e.runIn(deployToken+"\n", "secret", "set", "DEPLOY_TOKEN")
	out = e.ok("run", "deploy", "--env", "staging", "--version", "1.2.3")
	mustContain(t, out, "deploying 1.2.3 to staging", "[REDACTED:DEPLOY_TOKEN]")
	if strings.Contains(out, deployToken) {
		t.Fatalf("secret leaked to the caller:\n%s", out)
	}
	mustContain(t, e.ok("secret", "list"), "DEPLOY_TOKEN", "deploy")
	if strings.Contains(e.ok("secret", "list"), deployToken) {
		t.Fatal("secret list shows values")
	}

	// 脚本的退出码原样传给调用者
	e.ok("admin", "sync", "--yes", writeAction(t, "fail", "risk: {default: auto}\n", "echo oops\nexit 7"))
	if out, code := e.run("run", "fail"); code != 7 || !strings.Contains(out, "oops") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestApprovalFlow(t *testing.T) {
	e := newReadyEnv(t)

	t.Run("approve", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "1.2.3")
		msg := e.approvalMessage(id)
		mustContain(t, msg.Text, "Action: deploy", "env = prod", "version = 1.2.3", "would deploy 1.2.3 to prod")
		if msg.Chat != ownerID {
			t.Fatalf("message went to chat %d", msg.Chat)
		}

		// 陌生人点击批准无效
		e.tg.Click(strangerID, msg.Buttons[0])
		e.waitRejected(id, "telegram:99")
		mustContain(t, e.ok("status", id), "state: pending_approval")

		e.tg.Click(ownerID, msg.Buttons[0])
		out := e.ok("wait", "--timeout", "20s", id)
		mustContain(t, out, "state: succeeded", "approver: owner", "deploying 1.2.3 to prod")
		if _, found := e.tg.WaitFor(5*time.Second, func(m faketg.Message) bool {
			return m.ReplyTo == msg.ID && strings.Contains(m.Text, "Finished: succeeded")
		}); !found {
			t.Fatal("no result posted back to the approval message")
		}

		// 已用过的按钮再点一次不会再执行
		e.tg.Click(ownerID, msg.Buttons[0])
		e.waitRejected(id, "telegram:42")
		if n := strings.Count(e.ok("admin", "audit", "tail", "-n", "200"), `"event":"request_started"`); n != 1 {
			t.Fatalf("request started %d times", n)
		}
	})

	t.Run("deny", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "2.0.0")
		e.tg.Click(ownerID, e.approvalMessage(id).Buttons[1])
		out, code := e.run("wait", "--timeout", "20s", id)
		if code != 2 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		mustContain(t, out, "state: denied")
	})

	t.Run("cancel", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "2.0.1")
		msg := e.approvalMessage(id)
		mustContain(t, e.ok("cancel", id), "state: canceled")
		// 取消后重放先前收到的按钮回调仍然无效
		e.tg.Click(ownerID, msg.Buttons[0])
		e.waitRejected(id, "telegram:42")
		mustContain(t, e.ok("status", id), "state: canceled")
	})

	t.Run("wait times out while pending", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "2.0.2")
		if out, code := e.run("wait", "--timeout", "1s", id); code != 3 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		e.ok("cancel", id)
	})

	t.Run("pending request survives a restart", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "3.0.0")
		msg := e.approvalMessage(id)
		e.stopServer()
		e.startServer("--dev")
		mustContain(t, e.ok("status", id), "state: pending_approval")
		e.tg.Click(ownerID, msg.Buttons[0])
		mustContain(t, e.ok("wait", "--timeout", "20s", id), "state: succeeded")
	})

	t.Run("emergency approval by the administrator", func(t *testing.T) {
		id := e.submit("deploy", "--env", "prod", "--version", "3.0.1")
		e.ok("admin", "approve", id)
		mustContain(t, e.ok("wait", "--timeout", "20s", id), "state: succeeded", "approver: admin:")
	})

	t.Run("file parameter is shown to the approver in full", func(t *testing.T) {
		dir := writeAction(t, "sql", "params: {script: {type: file}, note: {type: string}}\n", `cat "$CMDGATE_PARAM_SCRIPT"`)
		e.ok("admin", "sync", "--yes", dir)
		sql := filepath.Join(t.TempDir(), "change.sql")
		if err := os.WriteFile(sql, []byte("UPDATE users SET plan = 'pro' WHERE id = 7;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// 很长的参数值不会被截断，而是进附件
		long := strings.Repeat("x", 600)
		id := e.submit("--file", "script="+sql, "sql", "--note", long)
		msg := e.approvalMessage(id)
		mustContain(t, msg.Text, "script:", "sha256", "note: (600 bytes")
		doc, found := e.tg.WaitFor(5*time.Second, func(m faketg.Message) bool { return m.ReplyTo == msg.ID && m.Document != "" })
		if !found {
			t.Fatal("no attachment sent")
		}
		mustContain(t, doc.Document, "UPDATE users SET plan = 'pro' WHERE id = 7;", long)
		e.tg.Click(ownerID, msg.Buttons[0])
		mustContain(t, e.ok("wait", "--timeout", "20s", id), "UPDATE users SET plan = 'pro' WHERE id = 7;")
	})

	t.Run("nothing sensitive in the audit log", func(t *testing.T) {
		audit := e.ok("admin", "audit", "tail", "-n", "500")
		mustContain(t, audit, `"event":"request_created"`, `"event":"request_approved"`, `"event":"request_finished"`, `"approver":"owner"`)
		for _, secret := range []string{deployToken, faketg.Token} {
			if strings.Contains(audit, secret) {
				t.Fatalf("audit log contains a secret value")
			}
		}
		// 服务端日志里也不能出现 bot token
		if strings.Contains(e.log.String(), faketg.Token) {
			t.Fatal("server log contains the bot token")
		}
	})
}

func TestTamperedActionIsRefused(t *testing.T) {
	e := newEnv(t)
	e.startServer("--dev")
	e.ok("admin", "sync", "--yes", "../examples/actions/hello")
	e.ok("run", "hello")

	// 绕过 sync 直接改已安装的脚本
	run := filepath.Join(e.data, "actions", "hello", "run")
	if err := os.WriteFile(run, []byte("#!/bin/sh\necho tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("run", "hello"); code == 0 || strings.Contains(out, "tampered") {
		t.Fatalf("tampered action ran (exit %d):\n%s", code, out)
	}
	if out, code := e.run("admin", "doctor"); code == 0 || !strings.Contains(out, "differs from its installed hash") {
		t.Fatalf("doctor did not notice (exit %d):\n%s", code, out)
	}

	// 重新安装后恢复正常
	mustContain(t, e.ok("admin", "sync", "--yes", "../examples/actions/hello"), "changed: hello")
	mustContain(t, e.ok("run", "hello"), "hello, world")
}

func TestExportImport(t *testing.T) {
	src := newReadyEnv(t)
	file := filepath.Join(t.TempDir(), "backup.cgx")
	src.extra = []string{"CMDGATE_PASSPHRASE=correct horse"}
	src.ok("admin", "export", file)
	if data, _ := os.ReadFile(file); bytes.Contains(data, []byte(deployToken)) {
		t.Fatal("export file contains a secret in clear text")
	}

	dst := newEnv(t)
	dst.startServer("--dev")
	dst.stopServer()
	dst.extra = []string{"CMDGATE_PASSPHRASE=wrong"}
	if out, code := dst.run("admin", "import", file); code == 0 {
		t.Fatalf("import accepted a wrong passphrase:\n%s", out)
	}
	dst.extra = []string{"CMDGATE_PASSPHRASE=correct horse"}
	dst.ok("admin", "import", file)
	mustContain(t, dst.ok("secret", "list"), "DEPLOY_TOKEN")
	mustContain(t, dst.ok("channel", "list"), "telegram", "owner=42")

	// 导入后装上同一份操作就能直接使用
	dst.tg.Close()
	dst.tg = src.tg
	dst.startServer("--dev")
	dst.ok("admin", "sync", "--yes", "../examples/actions")
	mustContain(t, dst.ok("run", "deploy", "--env", "staging", "--version", "1.2.3"), "state: succeeded")
}

func TestServerMode(t *testing.T) {
	cert, key := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := newEnv(t)
	st := &store.Store{Root: srv.data}
	if err := st.Init(&store.Config{Mode: store.ModeServer, Listen: addr, TLSCert: cert, TLSKey: key}); err != nil {
		t.Fatal(err)
	}
	srv.startServer()
	srv.ok("admin", "sync", "--yes", "../examples/actions/hello")
	srv.ok("admin", "sync", "--yes", writeAction(t, "bobonly", "risk: {default: auto}\nrequesters: [bob]\n", "echo for bob"))
	token := strings.TrimSpace(lastLine(srv.ok("user", "add", "alice")))

	// 远端用户只有客户端和令牌，没有数据目录
	alice := &env{t: t, home: t.TempDir(), tg: srv.tg, remote: true}
	url := "https://" + addr
	if out, code := alice.runIn("cg_wrong\n", "login", "--ca", cert, url); code == 0 {
		t.Fatalf("login accepted a bad token:\n%s", out)
	}
	if out, code := alice.runIn(token+"\n", "login", url); code == 0 {
		t.Fatalf("login trusted an unknown certificate:\n%s", out)
	}
	out, code := alice.runIn(token+"\n", "login", "--ca", cert, url)
	if code != 0 {
		t.Fatalf("login failed:\n%s", out)
	}
	mustContain(t, alice.ok("whoami"), "name: alice", "admin: false", "mode: server")
	mustContain(t, alice.ok("run", "hello", "--name", "remote"), "hello, remote")

	// 操作可以限定发起人，名单外的用户既看不到也调不了
	if strings.Contains(alice.ok("list"), "bobonly") {
		t.Fatal("alice can see an action restricted to bob")
	}
	if _, code := alice.run("run", "bobonly"); code == 0 {
		t.Fatal("alice ran an action restricted to bob")
	}

	// 令牌不能用来调用管理接口
	req, _ := http.NewRequest("POST", url+"/v1/admin/requests/abc/decision", strings.NewReader(`{"approve":true}`))
	req.Header.Set("Authorization", "Bearer "+token)
	raw := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	t.Cleanup(raw.CloseIdleConnections)
	resp, err := raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin endpoint over the network: status %d", resp.StatusCode)
	}

	// 吊销后令牌立即失效，不需要重启服务端
	srv.ok("user", "remove", "alice")
	if out, code := alice.run("list"); code == 0 {
		t.Fatalf("revoked token still works:\n%s", out)
	}
}

func TestServerModeRefusesPlainHTTPUnlessAskedTo(t *testing.T) {
	e := newEnv(t)
	st := &store.Store{Root: e.data}
	if err := st.Init(&store.Config{Mode: store.ModeServer, Listen: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	out, code := e.run("serve")
	if code == 0 || !strings.Contains(out, "insecure: true") {
		t.Fatalf("server started without TLS (exit %d):\n%s", code, out)
	}
}

func TestDevRun(t *testing.T) {
	e := newEnv(t)
	e.extra = []string{"DEPLOY_TOKEN=local-test-value"}
	out := e.ok("dev", "run", "../examples/actions/deploy", "--env", "prod", "--version", "1.2.3")
	mustContain(t, out, "risk level for these params: approve", "would deploy 1.2.3 to prod", "deploying 1.2.3 to prod")
	if _, code := e.run("dev", "run", "../examples/actions/deploy", "--env", "moon", "--version", "1.2.3"); code == 0 {
		t.Fatal("dev run accepted an invalid param")
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// selfSignedCert 生成一张只用于 127.0.0.1 的自签证书，返回证书和私钥文件的路径
func selfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cmdgate test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
