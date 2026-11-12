package cli

import (
	"path/filepath"
	"testing"

	"cmdgate/internal/platform"
)

// TestInstallRejectsUntrustedParents 不修改共享目录，拒绝具有用户可写祖先的安装目录
func TestInstallRejectsUntrustedParents(t *testing.T) {
	dir := t.TempDir()
	if err := platform.EnsureTrustedDir(filepath.Join(dir, "bin")); err == nil {
		t.Fatal("accepted a user-writable ancestor")
	}
	if err := platform.EnsureTrustedDir("/"); err != nil {
		t.Fatalf("rejected trusted system path: %v", err)
	}
}
