package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// LockServer 独占当前数据目录，调用者关闭返回的文件时释放锁
//
// 锁文件不能删除，否则其他进程可能锁住不同的 inode
func (s *Store) LockServer() (*os.File, error) {
	f, err := os.OpenFile(s.Path("run", "server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("data directory already in use: %w", err)
	}
	return f, nil
}
