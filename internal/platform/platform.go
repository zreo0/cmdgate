// Package platform 封装与操作系统相关的安装步骤：创建服务用户、注册和控制系统服务
//
// 每个平台各有一个实现文件，对外的函数签名相同
package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// BinPath 是安装后的二进制位置，属主为 root，服务从这里启动
const BinPath = "/opt/cmdgate/bin/cmdgate"

// run 执行一个系统命令，失败时把命令的输出带进错误里
func run(name string, args ...string) error {
	// 系统管理工具多在 sbin 下，而调用者的 PATH 不一定包含它，所以先自己找出完整路径
	path := name
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		if _, err := os.Stat(dir + "/" + name); err == nil {
			path = dir + "/" + name
			break
		}
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
