package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ClientPathFile 是安装器管理的系统 PATH 片段位置
const ClientPathFile = "/etc/paths.d/cmdgate"

// clientPathContent 仅向客户端登录环境注册受保护的二进制目录
const clientPathContent = "/opt/cmdgate/bin\n"

const (
	serviceLabel = "com.cmdgate.server"
	plistPath    = "/Library/LaunchDaemons/" + serviceLabel + ".plist"
	// 系统账号使用 500 以下的 ID，从这个范围里找一个没用过的
	minSystemID = 300
	maxSystemID = 499
)

// EnsureUser 创建隐藏的服务用户和同名用户组，已存在时什么都不做
//
// 用户没有登录 shell 和个人目录，不会出现在登录界面
func EnsureUser(name string) error {
	if exec.Command("dscl", ".", "-read", "/Users/"+name).Run() == nil {
		return nil
	}
	id, err := freeID()
	if err != nil {
		return err
	}
	sid := strconv.Itoa(id)
	steps := [][]string{
		{"-create", "/Groups/" + name},
		{"-create", "/Groups/" + name, "PrimaryGroupID", sid},
		{"-create", "/Groups/" + name, "RealName", "cmdgate service"},
		{"-create", "/Groups/" + name, "Password", "*"},
		{"-create", "/Users/" + name},
		{"-create", "/Users/" + name, "UniqueID", sid},
		{"-create", "/Users/" + name, "PrimaryGroupID", sid},
		{"-create", "/Users/" + name, "UserShell", "/usr/bin/false"},
		{"-create", "/Users/" + name, "NFSHomeDirectory", "/var/empty"},
		{"-create", "/Users/" + name, "RealName", "cmdgate service"},
		{"-create", "/Users/" + name, "Password", "*"},
		{"-create", "/Users/" + name, "IsHidden", "1"},
	}
	for _, s := range steps {
		if err := run("dscl", append([]string{"."}, s...)...); err != nil {
			return err
		}
	}
	return nil
}

// freeID 找一个既没被用户也没被用户组占用的系统 ID
func freeID() (int, error) {
	used := map[int]bool{}
	for _, q := range [][]string{{"/Users", "UniqueID"}, {"/Groups", "PrimaryGroupID"}} {
		out, err := exec.Command("dscl", ".", "-list", q[0], q[1]).Output()
		if err != nil {
			return 0, fmt.Errorf("dscl list %s: %w", q[0], err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if id, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
				used[id] = true
			}
		}
	}
	for id := minSystemID; id <= maxSystemID; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("no free system id between %d and %d", minSystemID, maxSystemID)
}

// RemoveUser 删除服务用户和用户组，不存在时不报错
func RemoveUser(name string) error {
	exec.Command("dscl", ".", "-delete", "/Users/"+name).Run()
	exec.Command("dscl", ".", "-delete", "/Groups/"+name).Run()
	return nil
}

// InstallService 写入 launchd 配置并启动服务
//
// 参数 dataDir 是数据根目录，user 是运行服务的用户
func InstallService(dataDir, user string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>serve</string>
    </array>
    <key>UserName</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>CMDGATE_DATA_DIR</key>
        <string>%s</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s/log/server.log</string>
    <key>StandardErrorPath</key>
    <string>%s/log/server.log</string>
</dict>
</plist>
`, serviceLabel, BinPath, user, dataDir, dataDir, dataDir)
	// 重复安装时先卸掉旧的
	exec.Command("launchctl", "bootout", "system/"+serviceLabel).Run()
	if err := os.WriteFile(plistPath, []byte(plist), 0o644); err != nil {
		return err
	}
	return run("launchctl", "bootstrap", "system", plistPath)
}

// RemoveService 停止服务并删除 launchd 配置
func RemoveService() error {
	exec.Command("launchctl", "bootout", "system/"+serviceLabel).Run()
	err := os.Remove(plistPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ServiceInstalled 返回系统服务是否已注册
func ServiceInstalled() bool {
	_, err := os.Stat(plistPath)
	return err == nil
}

// RestartService 重启服务，让新的配置生效
func RestartService() error {
	return run("launchctl", "kickstart", "-k", "system/"+serviceLabel)
}
