//go:build isolation

package e2e

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"cmdgate/internal/platform"
	"cmdgate/internal/store"
)

// as 以指定的 OS 用户执行一条命令，返回输出和退出码
func as(t *testing.T, username string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "USER=" + username}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential(t, username)}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s %v as %s: %v", name, args, username, err)
		}
		code = exit.ExitCode()
	}
	return string(out), code
}

func credential(t *testing.T, username string) *syscall.Credential {
	t.Helper()
	u, err := user.Lookup(username)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
}

// TestIsolation 在真实的安装上验证设计的核心不变量：
//
// 与 Agent 同权限的普通用户读不到凭证、改不了操作、批准不了请求，
// 最多只能发起请求；需要凭证的操作仍然能通过 cmdgate 正常执行
//
// 这个测试必须以 root 在一次性的 Linux 容器里运行，见 scripts/test-isolation.sh
func TestIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root in a disposable container")
	}
	const agent = "agent"
	root, svc := "root", store.DefaultServiceUser
	if out, err := exec.Command("useradd", "--create-home", agent).CombinedOutput(); err != nil {
		t.Fatalf("useradd: %v %s", err, out)
	}

	// 安装：不注册系统服务，由测试自己以服务用户启动
	out, code := as(t, root, bin, "install", "--mode", "local", "--no-service")
	if code != 0 {
		t.Fatalf("install exited %d:\n%s", code, out)
	}
	cg := platform.BinPath

	t.Run("client PATH is installed and survives reinstall", func(t *testing.T) {
		const config = platform.ClientPathFile
		before, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if out, code := as(t, agent, "/bin/bash", "-lc", "command -v cmdgate && cmdgate version"); code != 0 || !strings.Contains(out, cg) {
			t.Fatalf("client not available in login shell: %s", out)
		}
		if out, code := as(t, root, cg, "install", "--mode", "local", "--no-service"); code != 0 {
			t.Fatal(out)
		}
		after, err := os.ReadFile(config)
		if err != nil || string(after) != string(before) {
			t.Fatalf("PATH configuration changed on reinstall: %v", err)
		}
		if out, code := as(t, agent, "/bin/sh", "-c", "echo unsafe >> "+config); code == 0 {
			t.Fatalf("agent modified system PATH: %s", out)
		}
		if out, code := as(t, agent, "/bin/sh", "-c", "PATH=/usr/bin:/bin; . "+config+"; . "+config+"; printf '%s' \"$PATH\""); code != 0 || out != "/opt/cmdgate/bin:/usr/bin:/bin" {
			t.Fatalf("PATH setup is not idempotent: %q", out)
		}
	})

	t.Run("PATH configuration preserves foreign files and rejects unsafe parents", func(t *testing.T) {
		const config = platform.ClientPathFile
		original, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(t.TempDir(), "unrelated")
		if err := os.WriteFile(victim, []byte("keep this file"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"modified", "symlink", "writable", "foreign owner"} {
			t.Run(kind, func(t *testing.T) {
				if err := os.Remove(config); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(config); err != nil {
						t.Error(err)
					}
					if err := platform.InstallClientPath(); err != nil {
						t.Error(err)
					}
				})
				if kind == "symlink" {
					if err := os.Symlink(victim, config); err != nil {
						t.Fatal(err)
					}
				} else {
					data := original
					if kind == "modified" {
						data = []byte(strings.ReplaceAll(string(original), "cmdgate", "changed"))
					}
					if err := os.WriteFile(config, data, 0o644); err != nil {
						t.Fatal(err)
					}
					if kind == "writable" {
						if err := os.Chmod(config, 0o666); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "foreign owner" {
						cred := credential(t, agent)
						if err := os.Chown(config, int(cred.Uid), int(cred.Gid)); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, err := os.Lstat(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := platform.InstallClientPath(); err == nil {
					t.Fatal("overwrote foreign PATH configuration")
				}
				if removed, err := platform.RemoveClientPath(); err != nil || removed {
					t.Fatalf("removed foreign PATH configuration: %v", err)
				}
				after, err := os.Lstat(config)
				if err != nil || !os.SameFile(before, after) {
					t.Fatalf("foreign PATH entry changed: %v", err)
				}
				data, err := os.ReadFile(victim)
				if err != nil || string(data) != "keep this file" {
					t.Fatalf("symlink target changed: %v", err)
				}
			})
		}
		parent := filepath.Dir(config)
		info, err := os.Stat(parent)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0o777); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(parent, info.Mode().Perm())
		if err := platform.InstallClientPath(); err == nil {
			t.Fatal("accepted a user-writable PATH configuration directory")
		}
		if _, err := platform.RemoveClientPath(); err == nil {
			t.Fatal("removed configuration through an untrusted parent")
		}
	})

	t.Run("binary survives a user-writable client directory", func(t *testing.T) {
		// 模拟 macOS 上由用户管理的 /usr/local/bin，不能据此替换服务程序
		cred := credential(t, agent)
		if err := os.Chown("/usr/local/bin", int(cred.Uid), int(cred.Gid)); err != nil {
			t.Fatal(err)
		}
		defer os.Chown("/usr/local/bin", 0, 0)
		backup := cg + ".replaced"
		if out, code := as(t, agent, "mv", cg, backup); code == 0 {
			os.Rename(backup, cg)
			t.Fatalf("agent replaced service binary: %s", out)
		}
		// 即使从已安装位置再次执行，也必须检查父目录，不能走提前返回的捷径
		parent := filepath.Dir(cg)
		if err := os.Chown(parent, int(cred.Uid), int(cred.Gid)); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(parent, 0, 0)
		if out, code := as(t, root, cg, "install", "--mode", "local", "--no-service"); code == 0 || !strings.Contains(out, "untrusted installation directory") {
			t.Fatalf("accepted unsafe installation directory: %s", out)
		}
	})
	t.Cleanup(func() {
		out, code := as(t, root, cg, "uninstall", "--purge")
		if code != 0 {
			t.Errorf("uninstall exited %d:\n%s", code, out)
		}
		if _, err := os.Stat(store.DefaultRoot); err == nil {
			t.Errorf("%s still exists after uninstall --purge", store.DefaultRoot)
		}
		if _, err := user.Lookup(svc); err == nil {
			t.Errorf("service user still exists after uninstall")
		}
		if _, err := os.Lstat(platform.ClientPathFile); !os.IsNotExist(err) {
			t.Errorf("PATH configuration still exists after uninstall: %v", err)
		}
	})

	// 服务端拒绝以 root 运行
	if out, code := as(t, root, cg, "serve"); code == 0 || !strings.Contains(out, "refusing to serve as root") {
		t.Fatalf("server started as root (exit %d):\n%s", code, out)
	}

	srv := exec.Command(cg, "serve")
	srv.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	srv.SysProcAttr = &syscall.SysProcAttr{Credential: credential(t, svc)}
	logFile, err := os.CreateTemp(t.TempDir(), "server-log-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	srv.Stdout, srv.Stderr = logFile, logFile
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Process.Signal(syscall.SIGTERM)
		srv.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logFile.Name())
			t.Logf("server log:\n%s", data)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, code := as(t, agent, cg, "whoami"); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 管理员安装操作和凭证
	if out, code := as(t, root, cg, "admin", "sync", "--yes", "../examples/actions"); code != 0 {
		t.Fatalf("sync exited %d:\n%s", code, out)
	}
	set := exec.Command(cg, "secret", "set", "DEPLOY_TOKEN")
	set.Stdin = strings.NewReader(deployToken + "\n")
	if out, err := set.CombinedOutput(); err != nil {
		t.Fatalf("secret set: %v %s", err, out)
	}

	t.Run("preflight cannot load caller-controlled tools", func(t *testing.T) {
		fake := "/usr/local/bin/uname"
		if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat /opt/cmdgate/secrets/DEPLOY_TOKEN\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(fake)
		cred := credential(t, agent)
		if err := os.Chown(fake, int(cred.Uid), int(cred.Gid)); err != nil {
			t.Fatal(err)
		}
		dir := writeAction(t, "pathcheck", "", "true")
		if err := os.WriteFile(filepath.Join(dir, "preflight"), []byte("#!/bin/sh\nuname -s\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, code := as(t, root, cg, "admin", "sync", "--yes", dir); code != 0 {
			t.Fatal(out)
		}
		defer as(t, root, cg, "admin", "action", "remove", "pathcheck")
		out, code := as(t, agent, cg, "run", "pathcheck")
		match := idRe.FindStringSubmatch(out)
		if code != 0 || len(match) != 2 {
			t.Fatal(out)
		}
		id := match[1]
		out, code = as(t, agent, "curl", "-s", "--unix-socket", store.DefaultRoot+"/run/cmdgate.sock", "http://cmdgate/v1/requests/"+id)
		if code != 0 || strings.Contains(out, deployToken) || !strings.Contains(out, "Linux") {
			t.Fatalf("unsafe preflight: %s", out)
		}
		as(t, agent, cg, "cancel", id)
	})

	t.Run("agent is identified by its OS user and is not an admin", func(t *testing.T) {
		out, _ := as(t, agent, cg, "whoami")
		mustContain(t, out, "name: agent", "admin: false", "mode: local")
	})

	t.Run("agent can run an action that needs a secret", func(t *testing.T) {
		out, code := as(t, agent, cg, "run", "deploy", "--env", "staging", "--version", "1.2.3")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		mustContain(t, out, "state: succeeded", "deploying 1.2.3 to staging", "[REDACTED:DEPLOY_TOKEN]")
		if strings.Contains(out, deployToken) {
			t.Fatal("secret leaked to the agent")
		}
	})

	t.Run("agent cannot read anything inside the data directory", func(t *testing.T) {
		for _, path := range []string{
			"secrets/DEPLOY_TOKEN", "secrets", "config.yaml", "actions.lock",
			"actions", "actions/deploy/run", "requests", "audit", "home",
		} {
			full := store.DefaultRoot + "/" + path
			// 文件用 cat 读内容，目录用带斜杠的 ls 列内容，两者都失败才算读不到
			out, code := as(t, agent, "sh", "-c", "cat "+full+" 2>&1 || ls "+full+"/ 2>&1")
			if code == 0 {
				t.Errorf("agent can read %s:\n%s", full, out)
			}
			if strings.Contains(out, deployToken) {
				t.Errorf("agent saw the secret through %s", full)
			}
		}
	})

	t.Run("agent cannot change actions, secrets or configuration", func(t *testing.T) {
		writes := []string{
			"echo 'echo pwned' >> " + store.DefaultRoot + "/actions/deploy/run",
			"mkdir " + store.DefaultRoot + "/actions/evil",
			"echo x > " + store.DefaultRoot + "/secrets/NEW",
			"echo 'mode: dev' > " + store.DefaultRoot + "/config.yaml",
			"rm -f " + store.DefaultRoot + "/actions.lock",
			"echo x > " + cg,
		}
		for _, w := range writes {
			if out, code := as(t, agent, "sh", "-c", w); code == 0 {
				t.Errorf("agent could run %q:\n%s", w, out)
			}
		}
		adminCmds := [][]string{
			{"admin", "sync", "--yes", "../examples/actions"},
			{"secret", "set", "X"},
			{"secret", "list"},
			{"channel", "list"},
			{"admin", "request", "list"},
			{"admin", "audit", "tail"},
			{"install", "--mode", "local"},
		}
		for _, c := range adminCmds {
			if out, code := as(t, agent, cg, c...); code == 0 || !strings.Contains(out, "needs sudo") {
				t.Errorf("agent ran %v (exit %d):\n%s", c, code, out)
			}
		}
	})

	t.Run("agent cannot approve, only an administrator can", func(t *testing.T) {
		out, code := as(t, agent, cg, "run", "deploy", "--env", "prod", "--version", "1.2.3")
		if code != 0 || !strings.Contains(out, "state: pending_approval") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		id := idRe.FindStringSubmatch(out)[1]

		if out, code := as(t, agent, cg, "admin", "approve", id); code == 0 {
			t.Fatalf("agent approved its own request:\n%s", out)
		}
		// 绕过客户端直接调管理接口也不行
		status, _ := as(t, agent, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
			"--unix-socket", store.DefaultRoot+"/run/cmdgate.sock",
			"-X", "POST", "-d", `{"approve":true}`, "http://cmdgate/v1/admin/requests/"+id+"/decision")
		if status != "403" {
			t.Fatalf("admin endpoint answered %s to the agent", status)
		}
		out, _ = as(t, agent, cg, "status", id)
		mustContain(t, out, "state: pending_approval")

		if out, code := as(t, root, cg, "admin", "approve", id); code != 0 {
			t.Fatalf("admin approve exited %d:\n%s", code, out)
		}
		out, code = as(t, agent, cg, "wait", "--timeout", "20s", id)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		mustContain(t, out, "state: succeeded", "approver: admin:root")
	})

	t.Run("service user cannot change actions or configuration", func(t *testing.T) {
		// 脚本以服务用户运行，即使某个脚本有漏洞也改不了定义类内容
		for _, w := range []string{
			"echo 'echo pwned' >> " + store.DefaultRoot + "/actions/deploy/run",
			"echo 'mode: dev' > " + store.DefaultRoot + "/config.yaml",
			"echo x > " + store.DefaultRoot + "/actions.lock",
		} {
			if out, code := as(t, svc, "sh", "-c", w); code == 0 {
				t.Errorf("service user could run %q:\n%s", w, out)
			}
		}
	})

	t.Run("doctor finds the permissions in order", func(t *testing.T) {
		out, _ := as(t, root, cg, "admin", "doctor")
		mustContain(t, out, "ok       ownership and permissions", "ok       actions (2 installed)", "ok       server reachable")
	})
}
