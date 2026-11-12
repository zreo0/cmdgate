package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"cmdgate/internal/store"

	"gopkg.in/yaml.v3"
)

const (
	// 导出包开头的标识，也用来区分格式版本
	exportMagic = "CMDGATE1"
	// 从口令派生密钥的迭代次数
	kdfIterations = 600_000
	saltSize      = 16
	// 读取口令的环境变量，用于非交互场景
	envPassphrase = "CMDGATE_PASSPHRASE"
)

// 导出包默认包含的内容：配置、用户和凭证
// 操作脚本不在其中，它们的源头是用户自己的仓库，在新机器上重新 sync 即可
var exportPaths = []string{"config.yaml", "users.yaml", "secrets"}

// 加上 --with-history 时额外包含的内容
var historyPaths = []string{"requests", "audit"}

// adminExport 把配置、用户和凭证打包成一个用口令加密的文件
func adminExport(args []string) error {
	f, rest, err := parseFlags(args, map[string]bool{"with-history": false})
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageErr("usage: %s admin export [--with-history] <file>", store.Name)
	}
	st, _, err := adminStore()
	if err != nil {
		return err
	}
	paths := exportPaths
	if f.has("with-history") {
		paths = append(paths, historyPaths...)
	}
	archive, err := packTree(st.Root, paths)
	if err != nil {
		return err
	}
	pass, err := readPassphrase(true)
	if err != nil {
		return err
	}
	sealed, err := seal(archive, pass)
	if err != nil {
		return err
	}
	if err := os.WriteFile(rest[0], sealed, 0o600); err != nil {
		return err
	}
	st.Audit(store.Event{Event: "exported", Requester: adminName()})
	fmt.Printf("exported to %s (%d bytes)\n", rest[0], len(sealed))
	return st.FixPerms()
}

// adminImport 把导出包里的内容写入当前数据目录
//
// 当前机器的部署方式（模式、服务用户、监听地址、TLS）保持不变，只接收渠道、限流、用户和凭证，
// 这样本地导出的包可以直接导入到 server 模式的机器上
func adminImport(args []string) error {
	if len(args) != 1 {
		return usageErr("usage: %s admin import <file>", store.Name)
	}
	st, cfg, err := adminStore()
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	pass, err := readPassphrase(false)
	if err != nil {
		return err
	}
	archive, err := unseal(sealed, pass)
	if err != nil {
		return err
	}
	var imported store.Config
	err = unpackTree(archive, func(name string, data []byte, mode fs.FileMode) error {
		switch {
		case name == "config.yaml":
			return yaml.Unmarshal(data, &imported)
		case name == "users.yaml" || allowedPrefix(name):
			target := st.Path(filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return os.WriteFile(target, data, mode.Perm())
		}
		return fmt.Errorf("unexpected entry %q in the export file", name)
	})
	if err != nil {
		return err
	}
	cfg.Channels = imported.Channels
	cfg.RateLimitPerMinute = imported.RateLimitPerMinute
	if err := st.SaveConfig(cfg); err != nil {
		return err
	}
	st.Audit(store.Event{Event: "imported", Requester: adminName()})
	if err := st.FixPerms(); err != nil {
		return err
	}
	fmt.Println("imported configuration, users and secrets")
	fmt.Printf("actions are not part of an export, install them with: %s admin sync <dir>\n", store.Name)
	return applyConfig()
}

// allowedPrefix 返回导出包里的路径是否落在允许写入的目录下
func allowedPrefix(name string) bool {
	for _, p := range append([]string{"secrets"}, historyPaths...) {
		if strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// readPassphrase 读取导出包的口令
//
// 设置了环境变量时直接使用，否则在终端上询问；导出时要求输入两次以免打错
func readPassphrase(confirmTwice bool) (string, error) {
	if p := os.Getenv(envPassphrase); p != "" {
		return p, nil
	}
	pass, err := readSecretInput("Passphrase: ")
	if err != nil {
		return "", err
	}
	if pass == "" {
		return "", errors.New("empty passphrase")
	}
	if confirmTwice {
		again, err := readSecretInput("Repeat passphrase: ")
		if err != nil {
			return "", err
		}
		if again != pass {
			return "", errors.New("passphrases do not match")
		}
	}
	return pass, nil
}

// packTree 把根目录下的若干路径打成 tar.gz，不存在的路径跳过
func packTree(root string, paths []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		err := filepath.WalkDir(filepath.Join(root, p), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), Size: int64(len(data))}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// unpackTree 逐个读出 tar.gz 里的文件并交给 fn 处理
//
// 带有 .. 或绝对路径的条目会被拒绝，防止写到数据目录之外
func unpackTree(archive []byte, fn func(name string, data []byte, mode fs.FileMode) error) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := hdr.Name
		if hdr.Typeflag != tar.TypeReg || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("unsafe entry %q in the export file", name)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := fn(name, data, fs.FileMode(hdr.Mode)); err != nil {
			return err
		}
	}
}

// seal 用口令加密数据
//
// 格式为：标识 + 盐 + 随机数 + AES-256-GCM 密文，密钥由 PBKDF2-SHA256 从口令派生
func seal(plain []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := append([]byte(exportMagic), salt...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plain, []byte(exportMagic)), nil
}

// unseal 解密 seal 生成的数据，口令错误或内容被改动时返回错误
func unseal(sealed []byte, passphrase string) ([]byte, error) {
	if len(sealed) < len(exportMagic)+saltSize || string(sealed[:len(exportMagic)]) != exportMagic {
		return nil, errors.New("not a cmdgate export file")
	}
	rest := sealed[len(exportMagic):]
	salt, rest := rest[:saltSize], rest[saltSize:]
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	if len(rest) < gcm.NonceSize() {
		return nil, errors.New("export file is truncated")
	}
	nonce, ciphertext := rest[:gcm.NonceSize()], rest[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, []byte(exportMagic))
	if err != nil {
		return nil, errors.New("wrong passphrase or damaged export file")
	}
	return plain, nil
}

// newGCM 从口令和盐派生密钥并创建 AES-GCM 实例
func newGCM(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, kdfIterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
