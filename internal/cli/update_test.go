package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// releaseArchive 创建内存发布归档，检查所有准备步骤的写入错误
func releaseArchive(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg && header.Size > 0 {
			if _, err := tw.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUpdateVerifiesBeforeReplacing 覆盖发布下载、校验、原子替换和重启的成功与失败边界
func TestUpdateVerifiesBeforeReplacing(t *testing.T) {
	const version = "v1.2.3"
	archiveName := fmt.Sprintf("cmdgate_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	binary := &tar.Header{Name: "cmdgate", Typeflag: tar.TypeReg, Mode: 0o7777, Size: 3}
	for _, name := range []string{
		"latest", "explicit", "no service", "restart failure", "concurrent update",
		"checksum mismatch", "missing checksum", "duplicate checksum", "invalid checksum",
		"manifest unavailable", "archive unavailable", "truncated download", "bad gzip",
		"missing binary", "empty binary", "symlink binary", "hardlink binary", "duplicate binary",
		"bad gzip trailer", "latest prerelease", "latest wrong origin", "HTTP redirect", "canceled",
	} {
		t.Run(name, func(t *testing.T) {
			archive := releaseArchive(t, binary, &tar.Header{Name: "../../outside", Typeflag: tar.TypeReg, Size: 1})
			switch name {
			case "bad gzip":
				archive = []byte("not a gzip stream")
			case "missing binary":
				archive = releaseArchive(t, &tar.Header{Name: "other", Typeflag: tar.TypeReg, Size: 1})
			case "empty binary":
				archive = releaseArchive(t, &tar.Header{Name: "cmdgate", Typeflag: tar.TypeReg})
			case "symlink binary":
				archive = releaseArchive(t, &tar.Header{Name: "cmdgate", Typeflag: tar.TypeSymlink, Linkname: "outside"})
			case "hardlink binary":
				archive = releaseArchive(t, &tar.Header{Name: "cmdgate", Typeflag: tar.TypeLink, Linkname: "outside"})
			case "duplicate binary":
				archive = releaseArchive(t, binary, binary)
			case "bad gzip trailer":
				archive[len(archive)-8] ^= 0xff
			}
			sums := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), archiveName)
			switch name {
			case "checksum mismatch":
				sums = strings.Repeat("0", 64) + "  " + archiveName
			case "missing checksum":
				sums = strings.ReplaceAll(sums, archiveName, "another-platform.tar.gz")
			case "duplicate checksum":
				sums += sums
			case "invalid checksum":
				sums = "invalid  " + archiveName
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					tag := "/releases/tag/" + version
					switch name {
					case "latest prerelease":
						tag += "-rc.1"
					case "latest wrong origin":
						tag = "/foreign/tag/" + version
					case "HTTP redirect":
						tag = "http://" + r.Host + "/releases/tag/" + version
					}
					http.Redirect(w, r, tag, http.StatusFound)
				case "/releases/download/" + version + "/SHA256SUMS":
					if name == "manifest unavailable" {
						http.Error(w, "unavailable", http.StatusNotFound)
						return
					}
					fmt.Fprint(w, sums)
				case "/releases/download/" + version + "/" + archiveName:
					if name == "archive unavailable" {
						http.Error(w, "unavailable", http.StatusNotFound)
						return
					}
					if name == "truncated download" {
						w.Header().Set("Content-Length", fmt.Sprint(len(archive)+10))
					}
					w.Write(archive)
				default:
					if !strings.Contains(r.URL.Path, "/tag/") {
						http.NotFound(w, r)
					}
				}
			}))
			t.Cleanup(server.Close)
			client := server.Client()
			client.CheckRedirect = releaseRedirect
			root := t.TempDir()
			dir := filepath.Join(root, "bin")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "cmdgate")
			if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			old, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			// 更新不能重写配置、凭证或请求，也不能按归档里的路径写出额外文件
			for _, file := range []string{"config.yaml", "secrets", "requests", "service", "outside"} {
				if err := os.WriteFile(filepath.Join(root, file), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			restarts := 0
			restartErr := errors.New("restart unavailable")
			restart := func() error {
				restarts++
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "xxx" {
					t.Fatalf("restart happened before replacement: %q, %v", data, err)
				}
				if name == "concurrent update" {
					if _, err := updateBinary(ctx, client, server.URL+"/releases", version, target, nil); err == nil || !strings.Contains(err.Error(), "lock installation") {
						t.Fatalf("concurrent update was not rejected: %v", err)
					}
				}
				if name == "restart failure" {
					return restartErr
				}
				return nil
			}
			if name == "no service" {
				restart = nil
			}
			requested := "latest"
			if name == "explicit" {
				requested = version
			}
			got, err := updateBinary(ctx, client, server.URL+"/releases", requested, target, restart)
			replaced := name == "latest" || name == "explicit" || name == "no service" || name == "restart failure" || name == "concurrent update"
			if replaced {
				if got != version {
					t.Fatalf("version = %q; error = %v", got, err)
				}
				if name == "restart failure" {
					if !errors.Is(err, restartErr) || !strings.Contains(err.Error(), "installed "+version) {
						t.Fatalf("missing partial update error: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid release")
			}
			want, wantRestarts := "old", 0
			if replaced {
				want = "xxx"
				if restart != nil {
					wantRestarts = 1
				}
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != want || restarts != wantRestarts {
				t.Fatalf("binary = %q, restarts = %d, error = %v", data, restarts, err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode() != 0o755 {
				t.Fatalf("incorrect binary permissions: %v, %v", info, err)
			}
			data, err = io.ReadAll(old)
			if err != nil || string(data) != "old" {
				t.Fatalf("overwrote the running binary: %q, %v", data, err)
			}
			for _, file := range []string{"config.yaml", "secrets", "requests", "service", "outside"} {
				data, err := os.ReadFile(filepath.Join(root, file))
				if err != nil || string(data) != "keep" {
					t.Fatalf("changed %s: %q, %v", file, data, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "cmdgate" {
				t.Fatalf("temporary files remain: %v, %v", entries, err)
			}
		})
	}
}

// TestUpdateRejectsInvalidDestination 不允许更新尚未安装的路径、目录或符号链接
func TestUpdateRejectsInvalidDestination(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "cmdgate")
			if kind == "directory" {
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink("missing", target); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := updateBinary(context.Background(), nil, "", "v1.2.3", target, nil); err == nil {
				t.Fatal("accepted invalid destination")
			}
		})
	}
}

// TestUpdateRejectsInvalidArguments 无效参数在访问系统目录和网络之前被拒绝
func TestUpdateRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"v1.2.3"}, {"--version"}, {"--version", ""}, {"--version", "../bad"},
		{"--version", "v01.2.3"}, {"--version", "v1.2.3-rc.1"}, {"--version", "v1.2.3", "extra"}, {"--repo", "other/repo"},
	} {
		var usage *exitError
		if err := cmdUpdate(args); !errors.As(err, &usage) || usage.code != 2 {
			t.Fatalf("args %v: expected usage error, got %v", args, err)
		}
	}
	if os.Geteuid() != 0 {
		if err := cmdUpdate(nil); err == nil || !strings.Contains(err.Error(), "needs sudo") {
			t.Fatalf("update did not require root: %v", err)
		}
	}
}

// TestReleaseDownloadLimits 拒绝超限下载和非 HTTPS 地址，不访问外部网络
func TestReleaseDownloadLimits(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "12345")
	}))
	t.Cleanup(server.Close)
	if err := downloadRelease(context.Background(), server.Client(), server.URL, io.Discard, 4); err == nil {
		t.Fatal("accepted oversized download")
	}
	if _, err := releaseRequest(context.Background(), server.Client(), "http://example.invalid"); err == nil {
		t.Fatal("accepted insecure release URL")
	}
}
