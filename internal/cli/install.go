package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cmdgate/internal/platform"
	"cmdgate/internal/store"
)

// cmdInstall 把 cmdgate 安装成系统服务
//
// 依次完成：把二进制复制到 root 所有的位置、创建服务用户、初始化数据目录、注册并启动服务
// 重复执行是安全的，已有的数据目录会保留
func cmdInstall(args []string) error {
	f, _, err := parseFlags(args, map[string]bool{
		"mode": true, "listen": true, "tls-cert": true, "tls-key": true, "insecure": false, "no-service": false,
	})
	if err != nil {
		return err
	}
	mode := f.get("mode", "")
	if mode != store.ModeLocal && mode != store.ModeServer {
		return usageErr("usage: %s install --mode local|server [--listen addr] [--tls-cert file --tls-key file | --insecure] [--no-service]", store.Name)
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("install needs sudo")
	}
	cfg := &store.Config{Mode: mode, ServiceUser: store.DefaultServiceUser}
	if mode == store.ModeServer {
		cfg.Listen = f.get("listen", "")
		cfg.TLSCert = f.get("tls-cert", "")
		cfg.TLSKey = f.get("tls-key", "")
		cfg.Insecure = f.has("insecure")
		if cfg.Listen == "" {
			return usageErr("server mode needs --listen, for example --listen 0.0.0.0:8443")
		}
		if (cfg.TLSCert == "" || cfg.TLSKey == "") && !cfg.Insecure {
			return usageErr("server mode needs --tls-cert and --tls-key, or --insecure to serve plain HTTP")
		}
	}

	// 服务必须从 root 所有的路径启动，否则普通用户可以替换二进制来拿到服务用户的权限
	if err := installBinary(); err != nil {
		return err
	}
	fmt.Printf("binary: %s\n", platform.BinPath)
	if err := platform.InstallClientPath(); err != nil {
		return err
	}
	fmt.Printf("PATH configuration: %s\n", platform.ClientPathFile)

	if err := platform.EnsureUser(cfg.ServiceUser); err != nil {
		return err
	}
	fmt.Printf("service user: %s\n", cfg.ServiceUser)

	st := store.Open()
	if _, err := os.Stat(st.Path("config.yaml")); err == nil {
		fmt.Printf("data directory: %s (existing, kept)\n", st.Root)
		if err := st.FixPerms(); err != nil {
			return err
		}
	} else {
		if err := st.Init(cfg); err != nil {
			return err
		}
		fmt.Printf("data directory: %s\n", st.Root)
	}
	st.Audit(store.Event{Event: "installed", Detail: mode, Requester: adminName()})
	if err := st.FixPerms(); err != nil {
		return err
	}

	if f.has("no-service") {
		fmt.Printf("service: not registered, start it yourself as %s: %s serve\n", cfg.ServiceUser, platform.BinPath)
	} else {
		if err := platform.InstallService(st.Root, cfg.ServiceUser); err != nil {
			return err
		}
		fmt.Println("service: registered and started")
	}
	fmt.Printf(`
next steps:
    sudo %[1]s channel add telegram     set up the approval channel
    sudo %[1]s admin sync <dir>         install your actions
    sudo %[1]s secret set <NAME>        store the secrets they need
    sudo %[1]s admin doctor             check that everything is in place
`, platform.BinPath)
	fmt.Println("client commands: open a new login shell, then use cmdgate; restart agents from that shell")
	return nil
}

// installBinary 把当前运行的程序复制到固定的安装位置
func installBinary() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	if err := platform.EnsureTrustedDir(filepath.Dir(platform.BinPath)); err != nil {
		return err
	}
	if self == platform.BinPath {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	// 先写到临时文件再改名，升级时不会破坏正在运行的旧二进制
	dst, err := os.CreateTemp(filepath.Dir(platform.BinPath), ".cmdgate-*")
	if err != nil {
		return err
	}
	tmp := dst.Name()
	defer os.Remove(tmp)
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Chmod(0o755); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, platform.BinPath)
}

// cmdUninstall 移除系统服务、服务用户和二进制，默认保留数据目录
func cmdUninstall(args []string) error {
	f, _, err := parseFlags(args, map[string]bool{"purge": false})
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("uninstall needs sudo")
	}
	removed, err := platform.RemoveClientPath()
	if err != nil {
		return err
	}
	if removed {
		fmt.Printf("PATH configuration: %s removed\n", platform.ClientPathFile)
	} else {
		fmt.Printf("PATH configuration: %s absent or customized, left unchanged\n", platform.ClientPathFile)
	}
	if err := platform.RemoveService(); err != nil {
		return err
	}
	fmt.Println("service: removed")
	if err := platform.RemoveUser(store.DefaultServiceUser); err != nil {
		return err
	}
	fmt.Printf("service user: %s removed\n", store.DefaultServiceUser)
	if err := os.Remove(platform.BinPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("binary: %s removed\n", platform.BinPath)
	st := store.Open()
	if f.has("purge") {
		if err := os.RemoveAll(st.Root); err != nil {
			return err
		}
		fmt.Printf("data directory: %s removed\n", st.Root)
	} else {
		fmt.Printf("data directory: %s kept, it still holds your secrets; pass --purge to delete it\n", st.Root)
	}
	return nil
}
