package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

// ClientPathFile 是安装器管理的系统 PATH 片段位置
const ClientPathFile = "/etc/profile.d/cmdgate.sh"

// clientPathContent 仅向客户端登录环境注册受保护的二进制目录
const clientPathContent = `# Managed by cmdgate; removed on uninstall if unchanged
case ":${PATH-}:" in
    *:/opt/cmdgate/bin:*) ;;
    *) PATH="/opt/cmdgate/bin${PATH:+:$PATH}" ;;
esac
export PATH
`

const (
	serviceName = "cmdgate"
	unitPath    = "/etc/systemd/system/" + serviceName + ".service"
)

// EnsureUser 创建不能登录的系统用户和同名用户组，已存在时什么都不做
func EnsureUser(name string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	return run("useradd", "--system", "--user-group", "--no-create-home", "--shell", "/usr/sbin/nologin", name)
}

// RemoveUser 删除服务用户，不存在时不报错
func RemoveUser(name string) error {
	if _, err := user.Lookup(name); err != nil {
		return nil
	}
	return run("userdel", name)
}

// hasSystemd 返回当前系统是否由 systemd 管理
func hasSystemd() bool {
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

// InstallService 写入 systemd 单元并启动服务
//
// 参数 dataDir 是数据根目录，user 是运行服务的用户
func InstallService(dataDir, user string) error {
	if !hasSystemd() {
		return errors.New("systemd not found, install with --no-service and start 'cmdgate serve' as the service user yourself")
	}
	unit := fmt.Sprintf(`[Unit]
Description=cmdgate server
After=network.target

[Service]
User=%s
ExecStart=%s serve
Environment=CMDGATE_DATA_DIR=%s
Restart=on-failure

[Install]
WantedBy=multi-user.target
`, user, BinPath, dataDir)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "enable", "--now", serviceName)
}

// RemoveService 停止服务并删除 systemd 单元
func RemoveService() error {
	if !ServiceInstalled() {
		return nil
	}
	exec.Command("systemctl", "disable", "--now", serviceName).Run()
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	return nil
}

// ServiceInstalled 返回系统服务是否已注册
func ServiceInstalled() bool {
	_, err := os.Stat(unitPath)
	return err == nil
}

// RestartService 重启服务，让新的配置生效
func RestartService() error {
	return run("systemctl", "restart", serviceName)
}
