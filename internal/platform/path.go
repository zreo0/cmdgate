package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// EnsureTrustedDir 从根目录逐层检查或创建安装目录，拒绝符号链接和非 root 可写路径
func EnsureTrustedDir(dir string) error {
	if dir != "/" {
		if err := EnsureTrustedDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err = os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("untrusted installation directory %s: must be root-owned and not writable by other users", dir)
	}
	return nil
}

// clientPathLocation 只解析 macOS 的 /etc 系统别名，其他父目录仍须逐层验证
func clientPathLocation() (string, error) {
	etc, err := filepath.EvalSymlinks("/etc")
	if err != nil {
		return "", err
	}
	return filepath.Join(etc, filepath.Base(filepath.Dir(ClientPathFile)), filepath.Base(ClientPathFile)), nil
}

// managedClientPath 判断文件是否仍为安装器生成的 root 所有、只对 root 可写的配置
func managedClientPath(path string, info os.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm() != 0o644 || info.Size() != int64(len(clientPathContent)) {
		return false, nil
	}
	data, err := os.ReadFile(path)
	return string(data) == clientPathContent, err
}

// InstallClientPath 原子安装系统 PATH 片段，拒绝覆盖符号链接或其他内容，重复安装不改写
func InstallClientPath() error {
	path, err := clientPathLocation()
	if err != nil {
		return err
	}
	if err := EnsureTrustedDir(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err == nil {
		managed, err := managedClientPath(path, info)
		if err != nil {
			return err
		}
		if !managed {
			return fmt.Errorf("PATH configuration %s already exists and is not managed by cmdgate; refusing to overwrite", path)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	// 父目录只允许 root 修改，临时文件写完后再发布，登录 shell 不会读取半份配置
	file, err := os.CreateTemp(filepath.Dir(path), ".cmdgate-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.WriteString(clientPathContent); err != nil {
		return err
	}
	if err := file.Chmod(0o644); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// RemoveClientPath 只移除内容和权限均未改变的配置，返回是否实际删除；缺失或自定义文件保留
func RemoveClientPath() (bool, error) {
	path, err := clientPathLocation()
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := EnsureTrustedDir(filepath.Dir(path)); err != nil {
		return false, err
	}
	managed, err := managedClientPath(path, info)
	if err != nil || !managed {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}
