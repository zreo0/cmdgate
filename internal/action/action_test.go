package action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeAction 在临时目录里生成一个操作，返回它的路径
func writeAction(t *testing.T, yaml string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "act")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "action.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

const deployYAML = `
name: deploy
params:
    env: {type: enum, values: [staging, prod]}
    version: {type: string, pattern: '\d+\.\d+\.\d+'}
    replicas: {type: int, min: 1, max: 10, default: "2"}
    note: {type: string, optional: true}
    script: {type: file, max_size: 16, optional: true}
risk:
    default: approve
    rules:
        - when: {env: staging}
          level: auto
secrets: [DEPLOY_TOKEN]
`

func TestValidate(t *testing.T) {
	act, err := Load(writeAction(t, deployYAML))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		params  map[string]string
		files   map[string][]byte
		wantErr string
	}{
		{name: "ok", params: map[string]string{"env": "prod", "version": "1.2.3"}},
		{name: "unknown param", params: map[string]string{"env": "prod", "version": "1.2.3", "force": "1"}, wantErr: "unknown param"},
		{name: "missing param", params: map[string]string{"env": "prod"}, wantErr: "missing param"},
		{name: "bad enum", params: map[string]string{"env": "dev", "version": "1.2.3"}, wantErr: "must be one of"},
		// 正则必须完整匹配，不能靠前缀混进额外内容
		{name: "pattern is anchored", params: map[string]string{"env": "prod", "version": "1.2.3; rm -rf /"}, wantErr: "does not match"},
		{name: "int out of range", params: map[string]string{"env": "prod", "version": "1.2.3", "replicas": "99"}, wantErr: "above 10"},
		{name: "int not a number", params: map[string]string{"env": "prod", "version": "1.2.3", "replicas": "two"}, wantErr: "not an integer"},
		{name: "file too large", params: map[string]string{"env": "prod", "version": "1.2.3"}, files: map[string][]byte{"script": make([]byte, 17)}, wantErr: "exceeds"},
		{name: "file passed as plain param", params: map[string]string{"env": "prod", "version": "1.2.3", "script": "x"}, wantErr: "is a file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := act.Validate(c.params, c.files)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got["replicas"] != "2" {
					t.Errorf("default not applied: %v", got)
				}
				if _, ok := got["note"]; ok {
					t.Errorf("optional param should be absent: %v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestLevel(t *testing.T) {
	act, err := Load(writeAction(t, deployYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := act.Level(map[string]string{"env": "staging"}); got != LevelAuto {
		t.Errorf("staging: got %s", got)
	}
	if got := act.Level(map[string]string{"env": "prod"}); got != LevelApprove {
		t.Errorf("prod: got %s", got)
	}
}

func TestLoadRejectsBadDefinitions(t *testing.T) {
	cases := map[string]string{
		"unknown field":         "name: a\nrisk: {default: auto}\ntypo_field: 1\n",
		"bad risk level":        "name: a\nrisk: {default: maybe}\n",
		"reserved secret":       "name: a\nsecrets: [CMDGATE_TELEGRAM_TOKEN]\n",
		"rule on unknown param": "name: a\nrisk: {rules: [{when: {x: y}, level: auto}]}\n",
		"bad name":              "name: ../etc\n",
	}
	for name, yaml := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeAction(t, yaml)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadDefaultsToApprove(t *testing.T) {
	// 没写 risk 的操作必须需要审批，而不是自动通过
	act, err := Load(writeAction(t, "name: a\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := act.Level(nil); got != LevelApprove {
		t.Errorf("got %s", got)
	}
}

func TestHash(t *testing.T) {
	dir := writeAction(t, "name: a\n")
	base, err := Hash(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := Hash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed == base {
		t.Error("content change did not change the hash")
	}

	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if restored, err := Hash(dir); err != nil || restored != base {
		t.Error("hash is not stable for identical content")
	}

	if err := os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if added, err := Hash(dir); err != nil || added == base {
		t.Error("added file did not change the hash")
	}
	if err := os.Remove(filepath.Join(dir, "extra")); err != nil {
		t.Fatal(err)
	}

	// 符号链接可能指向目录之外，必须拒绝
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Hash(dir); err == nil {
		t.Error("symlink should be rejected")
	}
}

// TestSnapshotBindsAllFiles 安装目录变化不能改变执行副本，复制前发生变化则拒绝
func TestSnapshotBindsAllFiles(t *testing.T) {
	dir := writeAction(t, "name: snapshot\n")
	helper := filepath.Join(dir, "helper")
	if err := os.WriteFile(helper, []byte("approved helper"), 0o644); err != nil {
		t.Fatal(err)
	}
	act, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Snapshot(act, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("new helper"), 0o644); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(snapshot.Dir, "helper"))
	if err != nil || string(copied) != "approved helper" {
		t.Fatalf("copied helper=%q err=%v", copied, err)
	}
	if _, err := Snapshot(act, t.TempDir()); err == nil {
		t.Fatal("accepted changed source")
	}
	if err := os.Remove(helper); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", helper); err != nil {
		t.Fatal(err)
	}
	if _, err := Snapshot(act, t.TempDir()); err == nil {
		t.Fatal("accepted symlink source")
	}
}
