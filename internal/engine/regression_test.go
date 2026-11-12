package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/channel"
	"cmdgate/internal/store"
)

// TestQueuedActionReplacement 拒绝排队期间换掉的脚本，批准只对应原版本
func TestQueuedActionReplacement(t *testing.T) {
	f := newFixture(t)
	f.install("queued", "concurrency: 1\n", "echo approved-version")
	act, err := action.Load(f.st.Path("actions", "queued"))
	if err != nil {
		t.Fatal(err)
	}
	sem := f.eng.sem(act)
	sem <- struct{}{}
	req := f.submit("queued", nil)
	if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: ownerID}); err != nil {
		t.Fatal(err)
	}
	// 并发槽位保持占用，更新后才允许执行；快照绑定另由 action 包确定性验证
	f.install("queued", "concurrency: 1\n", "echo NEW-UNAPPROVED-VERSION")
	<-sem
	got := f.wait(req.ID)
	if got.State != store.StateFailed || strings.Contains(f.eng.Output(req.ID), "NEW-UNAPPROVED") {
		t.Fatalf("updated script executed: state=%s output=%q", got.State, f.eng.Output(req.ID))
	}
}

// TestStateSaveFailuresBlockExecution 覆盖批准及开始执行时的写入失败，不允许产生脚本副作用
func TestStateSaveFailuresBlockExecution(t *testing.T) {
	for _, stage := range []string{"approval", "execution"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			marker, script := f.marker()
			f.install("persist", "concurrency: 1\n", script)
			req := f.submit("persist", nil)
			act, err := action.Load(f.st.Path("actions", "persist"))
			if err != nil {
				t.Fatal(err)
			}
			sem := f.eng.sem(act)
			if stage == "execution" {
				sem <- struct{}{}
				if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: ownerID}); err != nil {
					t.Fatal(err)
				}
			}
			record := filepath.Join(f.st.RequestDir(req.ID), "request.json")
			if err := os.Remove(record); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(record, 0o700); err != nil {
				t.Fatal(err)
			}
			if stage == "approval" {
				if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: ownerID}); err == nil {
					t.Fatal("accepted an unpersisted approval")
				}
				got, _ := f.eng.Get(req.ID, user)
				if got.State != store.StatePending {
					t.Fatalf("state=%s", got.State)
				}
			} else {
				<-sem
				// 终态保存也会失败，使用内存里的 done 等待执行协程退出
				f.eng.mu.Lock()
				ent := f.eng.active[req.ID]
				f.eng.mu.Unlock()
				if ent != nil {
					select {
					case <-ent.done:
					case <-time.After(3 * time.Second):
						t.Fatal("execution did not stop")
					}
				}
			}
			if exists(marker) {
				t.Fatal("script ran despite persistence failure")
			}
		})
	}
}

// TestPreflightIgnoresCallerPath 验证普通工具调用不从调用者控制的 PATH 加载程序
func TestPreflightIgnoresCallerPath(t *testing.T) {
	f := newFixture(t)
	if err := f.st.SetSecret("API_KEY", "review-secret-123456"); err != nil {
		t.Fatal(err)
	}
	helpers := t.TempDir()
	t.Setenv("PATH", helpers+":/usr/bin:/bin")
	if err := os.WriteFile(filepath.Join(helpers, "uname"), []byte("#!/bin/sh\n/bin/cat '"+f.st.Path("secrets", "API_KEY")+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.install("check", "", "true")
	if err := os.WriteFile(f.st.Path("actions", "check", "preflight"), []byte("#!/bin/sh\nuname -s\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	act, err := action.Load(f.st.Path("actions", "check"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SaveLock(map[string]store.LockEntry{"check": {Hash: act.Hash}}); err != nil {
		t.Fatal(err)
	}
	req := f.submit("check", nil)
	if req.State != store.StatePending || strings.Contains(req.Preflight, "review-secret") || strings.TrimSpace(req.Preflight) == "" {
		t.Fatalf("unsafe or failed preflight: state=%s output=%q", req.State, req.Preflight)
	}
}

// TestTruncatedSecretDoesNotLeak 覆盖完整执行链路中输出上限切开凭证的情况
func TestTruncatedSecretDoesNotLeak(t *testing.T) {
	for _, c := range []struct {
		name, secret string
		extraPadding int
	}{
		{"partial credential", "review-token-abcdefghijklmnopqrstuvwxyz", 1},
		{"complete overlapping credential", "abab", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			if err := f.st.SetSecret("API_KEY", c.secret); err != nil {
				t.Fatal(err)
			}
			f.install("print", "risk: {default: auto}\nsecrets: [API_KEY]\n", fmt.Sprintf("head -c %d /dev/zero\nprintf '%%s' \"$API_KEY\"\nprintf extra", maxOutputBytes-len(c.secret)+c.extraPadding))
			req := f.wait(f.submit("print", nil).ID)
			out := f.eng.Output(req.ID)
			if req.State != store.StateSucceeded || strings.Contains(out, c.secret[:min(4, len(c.secret))]) || !strings.Contains(out, "[output truncated]") {
				t.Fatalf("unsafe output: state=%s suffix=%q", req.State, tail([]byte(out), 150))
			}
		})
	}
}

// TestRedactionBoundaries 覆盖分块写入、完整凭证和截断后只剩一个字符的边界
func TestRedactionBoundaries(t *testing.T) {
	for _, prefix := range []string{"s", "secret-valu", "secret-value"} {
		file, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		writer := limitedWriter{f: file, left: len(prefix) + 2}
		writer.Write([]byte("okse"))
		writer.Write([]byte("cret-value-extra"))
		file.Close()
		data, err := os.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		out := string(redact(data, map[string]string{"TOKEN": "secret-value"}, writer.truncated))
		if !strings.HasPrefix(out, "ok[REDACTED:") || !strings.Contains(out, "[output truncated]") {
			t.Fatalf("prefix=%q out=%q", prefix, out)
		}
	}
	if got := string(redact([]byte("ordinary output"), map[string]string{"TOKEN": "secret-value"}, false)); got != "ordinary output" {
		t.Fatal(got)
	}
	for _, c := range []struct {
		data    string
		secrets map[string]string
	}{
		{"abab", map[string]string{"TOKEN": "abab"}},
		{"sensitive", map[string]string{"A": "sensitive", "B": "iveX"}},
	} {
		if out := string(redact([]byte(c.data), c.secrets, true)); out != "[REDACTED:TRUNCATED]"+truncatedMarker {
			t.Fatalf("overlapping credentials leaked: %q", out)
		}
	}
}

// failedDelivery 模拟已发送摘要但必需附件发送失败的渠道
type failedDelivery struct{ *fakeChannel }

// Send 记录请求后报告附件交付失败
func (c *failedDelivery) Send(ctx context.Context, req channel.Request) ([]store.Handle, error) {
	handles, _ := c.fakeChannel.Send(ctx, req)
	return handles, errors.New("required attachment failed")
}

// TestFailedDeliveryBlocksApproval 验证交付失败后即使收到合法按钮回调也不能执行
func TestFailedDeliveryBlocksApproval(t *testing.T) {
	f := newFixture(t)
	f.eng.channels["fake"] = &failedDelivery{f.ch}
	marker, script := f.marker()
	f.install("delivery", "", script)
	req := f.submit("delivery", nil)
	if err := f.eng.Decide(req.ID, f.ch.last().Nonce, true, Approver{Channel: "fake", ID: ownerID}); err == nil {
		t.Fatal("accepted incomplete delivery")
	}
	if exists(marker) {
		t.Fatal("script executed")
	}
	// 应急管理员入口仍可在带外核实后处理请求
	if err := f.eng.Decide(req.ID, "", true, Approver{Admin: true, Name: "root"}); err != nil {
		t.Fatal(err)
	}
	if f.wait(req.ID).State != store.StateSucceeded {
		t.Fatal("admin fallback failed")
	}
}

// TestConcurrentCancelAndApprove 验证终态不会被重新改成 running，也不会重复关闭通知通道
func TestConcurrentCancelAndApprove(t *testing.T) {
	f := newFixture(t)
	f.cfg.RateLimitPerMinute = 1000
	f.install("race", "", "true")
	for i := 0; i < 100; i++ {
		req := f.submit("race", nil)
		nonce := f.ch.last().Nonce
		start := make(chan struct{})
		done := make(chan struct{}, 16)
		for j := 0; j < 16; j++ {
			go func(approve bool) {
				<-start
				if approve {
					f.eng.Decide(req.ID, nonce, true, Approver{Channel: "fake", ID: ownerID})
				} else {
					f.eng.Cancel(req.ID, user)
				}
				done <- struct{}{}
			}(j%2 == 0)
		}
		close(start)
		for j := 0; j < 16; j++ {
			<-done
		}
		if got := f.wait(req.ID); got.State != store.StateCanceled && got.State != store.StateSucceeded {
			t.Fatalf("unexpected state %s", got.State)
		}
	}
}
