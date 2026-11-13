package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// launchctlFailure 构造带退出码的进程错误，测试不执行真实服务管理命令
func launchctlFailure(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != code {
		t.Fatalf("prepare exit status %d: %v", code, err)
	}
	return fmt.Errorf("launchctl: %w", err)
}

// TestBootoutWaitsForRemoval 重现 bootout 先返回、旧服务稍后才移除的竞态及错误边界
func TestBootoutWaitsForRemoval(t *testing.T) {
	missing := launchctlFailure(t, 113)
	absent := launchctlFailure(t, 3)
	inProgress := launchctlFailure(t, 36)
	denied := launchctlFailure(t, 1)
	for _, tt := range []struct {
		name   string
		verbs  []string
		errors []error
		cancel bool
		want   error
	}{
		{"wait until removed", []string{"bootout", "print", "print", "print"}, []error{nil, nil, nil, missing}, false, nil},
		{"first installation", []string{"bootout", "print"}, []error{absent, missing}, false, nil},
		{"removal in progress", []string{"bootout", "print", "print"}, []error{inProgress, nil, missing}, false, nil},
		{"bootout denied", []string{"bootout"}, []error{denied}, false, denied},
		{"cannot query state", []string{"bootout", "print"}, []error{nil, denied}, false, denied},
		{"removal canceled", []string{"bootout", "print"}, []error{nil, nil}, true, context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			calls := 0
			err := bootoutLaunchdService(ctx, func(args ...string) error {
				if calls >= len(tt.verbs) {
					t.Fatalf("unexpected launchctl call: %v", args)
				}
				want := []string{tt.verbs[calls], "system/" + serviceLabel}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("launchctl args = %v, want %v", args, want)
				}
				result := tt.errors[calls]
				calls++
				if tt.cancel && calls == len(tt.verbs) {
					cancel()
				}
				return result
			})
			if !errors.Is(err, tt.want) || calls != len(tt.verbs) {
				t.Fatalf("error = %v, want %v; calls = %d, want %d", err, tt.want, calls, len(tt.verbs))
			}
		})
	}
}

// TestBootoutStopsAtDeadline 旧服务一直存在时等待必须受截止时间约束
func TestBootoutStopsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	err := bootoutLaunchdService(ctx, func(...string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

// TestRunContextPreservesExitStatus 查询失败的退出码必须能被服务状态等待逻辑识别
func TestRunContextPreservesExitStatus(t *testing.T) {
	err := runContext(context.Background(), "sh", "-c", "exit 113")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != launchctlServiceNotFound {
		t.Fatalf("lost command exit status: %v", err)
	}
}
