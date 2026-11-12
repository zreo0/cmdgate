package engine

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/channel"
	"cmdgate/internal/store"
)

const (
	ownerID    = "1001"
	strangerID = "9999"
)

var user = Identity{Name: "alice"}

// fakeChannel 是内存里的审批渠道，记录发出的请求，并让测试直接注入决定
type fakeChannel struct {
	mu        sync.Mutex
	sent      []channel.Request
	statuses  []string
	decisions chan channel.Decision
}

func (c *fakeChannel) Send(ctx context.Context, req channel.Request) ([]store.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, req)
	return []store.Handle{{Chat: ownerID, Message: "1"}}, nil
}

func (c *fakeChannel) Decisions(ctx context.Context) (<-chan channel.Decision, error) {
	out := make(chan channel.Decision)
	go func() {
		defer close(out)
		for {
			select {
			case d := <-c.decisions:
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (c *fakeChannel) Update(ctx context.Context, h store.Handle, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses = append(c.statuses, status)
	return nil
}

func (c *fakeChannel) Ping(ctx context.Context) error { return nil }

func (c *fakeChannel) last() channel.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent[len(c.sent)-1]
}

// fixture 是一套测试环境：临时数据目录、假渠道和运行中的引擎
type fixture struct {
	t   *testing.T
	st  *store.Store
	cfg *store.Config
	ch  *fakeChannel
	eng *Engine
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := &store.Store{Root: t.TempDir()}
	cfg := &store.Config{
		Mode: store.ModeDev,
		Channels: map[string]store.ChannelConfig{
			"fake": {Type: "fake", Approvers: map[string]string{"owner": ownerID, "alice": "1002"}},
		},
	}
	if err := st.Init(cfg); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, cfg: cfg, ch: &fakeChannel{decisions: make(chan channel.Decision)}}
	f.start()
	t.Cleanup(func() { f.eng.Stop() })
	return f
}

// start 创建并启动一个新的引擎，用来模拟服务端启动
func (f *fixture) start() {
	f.eng = New(f.st, f.cfg, map[string]channel.Channel{"fake": f.ch}, log.New(io.Discard, "", 0))
	if err := f.eng.Start(); err != nil {
		f.t.Fatal(err)
	}
}

// install 把一个操作写进数据目录并登记到 actions.lock
//
// 参数 yaml 是 action.yaml 里 name 之后的内容，script 是 run 脚本
func (f *fixture) install(name, yaml, script string) {
	f.t.Helper()
	dir := f.st.Path("actions", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yaml"), []byte("name: "+name+"\n"+yaml), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	act, err := action.Load(dir)
	if err != nil {
		f.t.Fatal(err)
	}
	lock, err := f.st.LoadLock()
	if err != nil {
		f.t.Fatal(err)
	}
	lock[name] = store.LockEntry{Hash: act.Hash, InstalledAt: time.Now()}
	if err := f.st.SaveLock(lock); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) submit(name string, params map[string]string) *store.Request {
	f.t.Helper()
	req, err := f.eng.Submit(user, name, params, nil)
	if err != nil {
		f.t.Fatalf("submit: %v", err)
	}
	return req
}

// wait 等待请求结束并返回终态
func (f *fixture) wait(id string) *store.Request {
	f.t.Helper()
	req, err := f.eng.Wait(context.Background(), id, user, 10*time.Second)
	if err != nil {
		f.t.Fatal(err)
	}
	if !store.Terminal(req.State) {
		f.t.Fatalf("request %s did not finish, state %s", id, req.State)
	}
	return req
}

// waitState 等待指定的可观察状态，超时或提前结束时报告最后的状态
func (f *fixture) waitState(id, state string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := f.eng.Get(id, user)
		if err != nil {
			f.t.Fatal(err)
		}
		if req.State == state {
			return
		}
		if store.Terminal(req.State) || time.Now().After(deadline) {
			f.t.Fatalf("request %s: want %s, got %s (%s)", id, state, req.State, req.Reason)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// marker 返回一个标记文件的路径，以及在 run 脚本里创建它的命令，用来判断脚本是否真的执行过
func (f *fixture) marker() (path, script string) {
	path = filepath.Join(f.t.TempDir(), "executed")
	return path, "touch " + path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestAutoRunsImmediately(t *testing.T) {
	f := newFixture(t)
	f.install("hello", "params: {name: {type: string}}\nrisk: {default: auto}\n", `echo "hello $CMDGATE_PARAM_NAME"`)
	req := f.wait(f.submit("hello", map[string]string{"name": "bob"}).ID)
	if req.State != store.StateSucceeded {
		t.Fatalf("state %s: %s", req.State, req.Reason)
	}
	if out := f.eng.Output(req.ID); out != "hello bob\n" {
		t.Errorf("output %q", out)
	}
}

func TestApprovalIsBoundToTheRequest(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	if req.State != store.StatePending {
		t.Fatalf("state %s", req.State)
	}
	nonce := f.ch.last().Nonce
	owner := Approver{Channel: "fake", ID: ownerID}

	// 以下每一种决定都必须被拒绝，且脚本不能执行
	rejected := []struct {
		name  string
		nonce string
		by    Approver
	}{
		{"wrong nonce", "0000", owner},
		{"empty nonce", "", owner},
		{"unknown approver", nonce, Approver{Channel: "fake", ID: strangerID}},
		{"wrong channel", nonce, Approver{Channel: "other", ID: ownerID}},
	}
	for _, c := range rejected {
		if err := f.eng.Decide(req.ID, c.nonce, true, c.by); err == nil {
			t.Fatalf("%s: decision was accepted", c.name)
		}
	}
	if got, _ := f.eng.Get(req.ID, user); got.State != store.StatePending {
		t.Fatalf("state changed to %s after rejected decisions", got.State)
	}
	if exists(path) {
		t.Fatal("script ran without a valid approval")
	}

	if err := f.eng.Decide(req.ID, nonce, true, owner); err != nil {
		t.Fatal(err)
	}
	done := f.wait(req.ID)
	if done.State != store.StateSucceeded || done.Approver != "owner" {
		t.Fatalf("state %s approver %q", done.State, done.Approver)
	}
	if !exists(path) {
		t.Fatal("script did not run after approval")
	}
	// 同一个批准不能用第二次
	if err := f.eng.Decide(req.ID, nonce, true, owner); err == nil {
		t.Fatal("approval was accepted twice")
	}
}

func TestDecisionsArriveThroughTheChannel(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	f.ch.decisions <- channel.Decision{RequestID: req.ID, Nonce: f.ch.last().Nonce, Approve: true, ApproverID: ownerID}
	if done := f.wait(req.ID); done.State != store.StateSucceeded || !exists(path) {
		t.Fatalf("state %s", done.State)
	}
}

func TestDenyDoesNotExecute(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	if err := f.eng.Decide(req.ID, f.ch.last().Nonce, false, Approver{Channel: "fake", ID: ownerID}); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(req.ID); done.State != store.StateDenied || exists(path) {
		t.Fatalf("state %s, executed %v", done.State, exists(path))
	}
}

func TestExpiryDoesNotExecute(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "approval: {timeout: 150ms}\n", script)
	req := f.submit("deploy", nil)
	nonce := f.ch.last().Nonce
	if done := f.wait(req.ID); done.State != store.StateExpired {
		t.Fatalf("state %s", done.State)
	}
	if err := f.eng.Decide(req.ID, nonce, true, Approver{Channel: "fake", ID: ownerID}); err == nil {
		t.Fatal("approval accepted after expiry")
	}
	if exists(path) {
		t.Fatal("script ran after expiry")
	}
}

func TestScriptChangedAfterRequestIsNotExecuted(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	// 在请求创建之后、批准之前换掉脚本
	if err := os.WriteFile(f.st.Path("actions", "deploy", "run"), []byte("#!/bin/sh\ntouch "+path+"\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: ownerID}); err != nil {
		t.Fatal(err)
	}
	done := f.wait(req.ID)
	if done.State != store.StateFailed || !strings.Contains(done.Reason, "changed") {
		t.Fatalf("state %s reason %q", done.State, done.Reason)
	}
	if exists(path) {
		t.Fatal("modified script was executed")
	}
	// 之后的新请求也必须被拒绝，直到重新安装
	if _, err := f.eng.Submit(user, "deploy", nil, nil); err == nil {
		t.Fatal("submit accepted for an action that differs from its installed hash")
	}
}

func TestSelfApprovalCanBeForbidden(t *testing.T) {
	f := newFixture(t)
	f.install("deploy", "approval: {allow_self: false}\n", "true")
	req := f.submit("deploy", nil)
	nonce := f.ch.last().Nonce
	// 请求者 alice 在渠道里也是审批人，但不能批准自己的请求
	if err := f.eng.Decide(req.ID, nonce, true, Approver{Channel: "fake", ID: "1002"}); err == nil {
		t.Fatal("self approval was accepted")
	}
	if err := f.eng.Decide(req.ID, nonce, true, Approver{Channel: "fake", ID: ownerID}); err != nil {
		t.Fatal(err)
	}
}

func TestApproverListIsEnforced(t *testing.T) {
	f := newFixture(t)
	f.install("deploy", "approval: {approvers: [owner]}\n", "true")
	req := f.submit("deploy", nil)
	if got := f.ch.last().Recipients; len(got) != 1 || got[0] != ownerID {
		t.Fatalf("recipients %v", got)
	}
	if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: "1002"}); err == nil {
		t.Fatal("approver outside the action's list was accepted")
	}
}

func TestSecretsAreInjectedAndRedacted(t *testing.T) {
	f := newFixture(t)
	if err := f.st.SetSecret("API_KEY", "s3cr3t-value"); err != nil {
		t.Fatal(err)
	}
	// 服务端自己的环境变量不能泄漏给脚本
	t.Setenv("SERVER_ONLY", "leak")
	f.install("show", "risk: {default: auto}\nsecrets: [API_KEY]\n", `echo "key=$API_KEY server=${SERVER_ONLY:-unset}"`)
	req := f.wait(f.submit("show", nil).ID)
	out := f.eng.Output(req.ID)
	if out != "key=[REDACTED:API_KEY] server=unset\n" {
		t.Fatalf("output %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(f.st.RequestDir(req.ID), "output.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cr3t-value") {
		t.Fatal("secret value was written to disk in the output log")
	}
}

func TestMissingSecretFailsBeforeRunning(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("show", "risk: {default: auto}\nsecrets: [NOT_SET]\n", script)
	req := f.wait(f.submit("show", nil).ID)
	if req.State != store.StateFailed || exists(path) {
		t.Fatalf("state %s, executed %v", req.State, exists(path))
	}
}

func TestPreflightFailureStopsTheRequest(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	pre := f.st.Path("actions", "deploy", "preflight")
	if err := os.WriteFile(pre, []byte("#!/bin/sh\necho ci is red\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 加了文件后需要更新安装记录
	act, err := action.Load(f.st.Path("actions", "deploy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveLock(map[string]store.LockEntry{"deploy": {Hash: act.Hash}}); err != nil {
		t.Fatal(err)
	}

	req := f.submit("deploy", nil)
	if req.State != store.StateFailed || !strings.Contains(req.Preflight, "ci is red") {
		t.Fatalf("state %s preflight %q", req.State, req.Preflight)
	}
	if len(f.ch.sent) != 0 || exists(path) {
		t.Fatal("request went on after a failed preflight")
	}
}

func TestFailureExitCodeAndTimeout(t *testing.T) {
	f := newFixture(t)
	f.install("fail", "risk: {default: auto}\n", "echo oops; exit 7")
	req := f.wait(f.submit("fail", nil).ID)
	if req.State != store.StateFailed || req.ExitCode == nil || *req.ExitCode != 7 {
		t.Fatalf("state %s exit %v", req.State, req.ExitCode)
	}

	f.install("slow", "risk: {default: auto}\nexec: {timeout: 200ms}\n", "sleep 30")
	req = f.wait(f.submit("slow", nil).ID)
	if req.State != store.StateFailed || !strings.Contains(req.Reason, "timed out") {
		t.Fatalf("state %s reason %q", req.State, req.Reason)
	}
}

func TestCancel(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	if err := f.eng.Cancel(req.ID, Identity{Name: "mallory"}); err == nil {
		t.Fatal("another user could cancel the request")
	}
	if err := f.eng.Cancel(req.ID, user); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(req.ID); done.State != store.StateCanceled || exists(path) {
		t.Fatalf("state %s", done.State)
	}

	f.install("slow", "risk: {default: auto}\n", "sleep 30")
	running := f.submit("slow", nil)
	f.waitState(running.ID, store.StateRunning)
	if err := f.eng.Cancel(running.ID, user); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(running.ID); done.State != store.StateCanceled {
		t.Fatalf("state %s", done.State)
	}
}

func TestRequestsAreOnlyVisibleToTheirRequester(t *testing.T) {
	f := newFixture(t)
	f.install("hello", "risk: {default: auto}\n", "echo hi")
	req := f.wait(f.submit("hello", nil).ID)
	if _, err := f.eng.Get(req.ID, Identity{Name: "mallory"}); err == nil {
		t.Fatal("another user could read the request")
	}
	if _, err := f.eng.Get(req.ID, Identity{Name: "root", Admin: true}); err != nil {
		t.Fatal("admin could not read the request")
	}
}

func TestRequestersRestrictWhoCanSubmit(t *testing.T) {
	f := newFixture(t)
	f.install("hello", "risk: {default: auto}\nrequesters: [bob]\n", "echo hi")
	if _, err := f.eng.Submit(user, "hello", nil, nil); err == nil {
		t.Fatal("a user outside requesters could submit")
	}
	if len(f.eng.List(user)) != 0 || len(f.eng.List(Identity{Name: "bob"})) != 1 {
		t.Fatal("list does not follow requesters")
	}
}

func TestRateLimit(t *testing.T) {
	f := newFixture(t)
	f.cfg.RateLimitPerMinute = 2
	f.install("deploy", "", "true")
	f.submit("deploy", nil)
	f.submit("deploy", nil)
	if _, err := f.eng.Submit(user, "deploy", nil, nil); err == nil {
		t.Fatal("third approval request within a minute was accepted")
	}
}

func TestConcurrencyLimit(t *testing.T) {
	f := newFixture(t)
	release := filepath.Join(t.TempDir(), "release")
	f.install("job", "risk: {default: auto}\nconcurrency: 1\n", "while [ ! -f '"+release+"' ]; do sleep 0.01; done")
	first := f.submit("job", nil)
	f.waitState(first.ID, store.StateRunning)
	second := f.submit("job", nil)
	f.waitState(second.ID, store.StateApproved)
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if done := f.wait(id); done.State != store.StateSucceeded {
			t.Fatalf("request %s ended as %s", id, done.State)
		}
	}
}

func TestRecoveryAfterRestart(t *testing.T) {
	f := newFixture(t)
	path, script := f.marker()
	f.install("deploy", "", script)
	f.install("slow", "risk: {default: auto}\n", "sleep 30")
	pending := f.submit("deploy", nil)
	nonce := f.ch.last().Nonce
	running := f.submit("slow", nil)
	f.waitState(running.ID, store.StateRunning)

	f.eng.Stop()
	f.start()

	// 执行中被打断的请求结果未知，不会自动重试
	if got, _ := f.eng.Get(running.ID, user); got.State != store.StateUnknown {
		t.Fatalf("interrupted request is %s", got.State)
	}
	// 待审批的请求在重启后仍然可以用原来的随机数批准
	if got, _ := f.eng.Get(pending.ID, user); got.State != store.StatePending {
		t.Fatalf("pending request is %s after restart", got.State)
	}
	if err := f.eng.Decide(pending.ID, nonce, true, Approver{Channel: "fake", ID: ownerID}); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(pending.ID); done.State != store.StateSucceeded || !exists(path) {
		t.Fatalf("state %s", done.State)
	}
}

func TestAdminDecisionWorksWithoutChannel(t *testing.T) {
	f := newFixture(t)
	f.eng.Stop()
	// 没有配置任何渠道时请求仍然创建，但只能由管理员处理
	f.cfg.Channels = nil
	f.eng = New(f.st, f.cfg, nil, log.New(io.Discard, "", 0))
	if err := f.eng.Start(); err != nil {
		t.Fatal(err)
	}
	path, script := f.marker()
	f.install("deploy", "", script)
	req := f.submit("deploy", nil)
	if req.State != store.StatePending || !strings.Contains(req.Reason, "admin approve") {
		t.Fatalf("state %s reason %q", req.State, req.Reason)
	}
	if err := f.eng.Decide(req.ID, "", true, Approver{Admin: true, Name: "root"}); err != nil {
		t.Fatal(err)
	}
	if done := f.wait(req.ID); done.State != store.StateSucceeded || done.Approver != "admin:root" || !exists(path) {
		t.Fatalf("state %s approver %q", done.State, done.Approver)
	}
}
