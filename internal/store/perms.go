package store

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// owner 表示一类路径应有的属主
type owner int

const (
	// 属主是 root，服务用户通过属组只读
	ownRoot owner = iota
	// 属主是服务用户
	ownService
)

// rule 描述数据目录下一个顶层条目的权限要求
type rule struct {
	// 相对数据根目录的路径
	path string
	own  owner
	// 目录权限
	dirMode os.FileMode
	// 普通文件权限，0 表示保留原有权限只改属主
	fileMode os.FileMode
	// 文件带可执行位时使用的权限，0 表示与 fileMode 相同
	execMode os.FileMode
}

// layout 是整个数据目录的权限表，FixPerms 和 CheckPerms 都以它为准
//
// 定义类内容归 root，服务用户只读，这样即使某个脚本有漏洞也改不了操作定义和配置
// 运行数据和凭证归服务用户，其他用户不可读
var layout = []rule{
	{path: "VERSION", own: ownRoot, fileMode: 0o640},
	{path: "config.yaml", own: ownRoot, fileMode: 0o640},
	{path: "users.yaml", own: ownRoot, fileMode: 0o640},
	{path: "actions.lock", own: ownRoot, fileMode: 0o640},
	{path: "actions", own: ownRoot, dirMode: 0o750, fileMode: 0o640, execMode: 0o750},
	{path: "secrets", own: ownService, dirMode: 0o700, fileMode: 0o600},
	{path: "requests", own: ownService, dirMode: 0o700},
	{path: "audit", own: ownService, dirMode: 0o700, fileMode: 0o600},
	{path: "home", own: ownService, dirMode: 0o700},
	{path: "log", own: ownService, dirMode: 0o700},
	{path: "run", own: ownService, dirMode: 0o755},
}

// ids 保存权限表里两类属主对应的 uid 和 gid
type ids struct {
	rootUID, serviceUID, serviceGID int
	// 是否以 root 运行，只有这时才能改属主
	chown bool
}

// resolveIDs 根据配置算出属主
//
// 以 root 运行且配置了服务用户时使用真实的 root 和服务用户，
// 否则（dev 模式）全部归当前用户，只整理权限位
func (s *Store) resolveIDs() (ids, error) {
	self := ids{rootUID: os.Geteuid(), serviceUID: os.Geteuid(), serviceGID: os.Getegid()}
	cfg, err := s.LoadConfig()
	if err != nil {
		return self, err
	}
	if os.Geteuid() != 0 || cfg.ServiceUser == "" {
		return self, nil
	}
	u, err := user.Lookup(cfg.ServiceUser)
	if err != nil {
		return self, fmt.Errorf("service user %s: %w", cfg.ServiceUser, err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return ids{rootUID: 0, serviceUID: uid, serviceGID: gid, chown: true}, nil
}

// want 返回一个路径按规则应有的 uid 和权限位，mode 为 0 表示不要求
func (r rule) want(id ids, info fs.FileInfo) (uid int, mode os.FileMode) {
	uid = id.serviceUID
	if r.own == ownRoot {
		uid = id.rootUID
	}
	switch {
	case info.IsDir():
		mode = r.dirMode
	case info.Mode().Perm()&0o100 != 0 && r.execMode != 0:
		mode = r.execMode
	default:
		mode = r.fileMode
	}
	return uid, mode
}

// walk 对权限表覆盖的每个现存路径调用 fn
func (s *Store) walk(fn func(path string, info fs.FileInfo, r rule) error) error {
	for _, r := range layout {
		top := s.Path(r.path)
		err := filepath.Walk(top, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			// socket 等特殊文件由服务端自己管理
			if !info.IsDir() && !info.Mode().IsRegular() {
				return nil
			}
			return fn(path, info, r)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// FixPerms 把数据目录的属主和权限整理成权限表要求的样子
//
// 每个管理命令结束时都会调用，所以管理命令以 root 写入的文件会被交还给正确的属主
func (s *Store) FixPerms() error {
	id, err := s.resolveIDs()
	if err != nil {
		return err
	}
	if id.chown {
		if err := os.Chown(s.Root, 0, 0); err != nil {
			return err
		}
	}
	if err := os.Chmod(s.Root, 0o755); err != nil {
		return err
	}
	return s.walk(func(path string, info fs.FileInfo, r rule) error {
		uid, mode := r.want(id, info)
		if id.chown {
			if err := os.Chown(path, uid, id.serviceGID); err != nil {
				return err
			}
		}
		if mode != 0 && info.Mode().Perm() != mode {
			return os.Chmod(path, mode)
		}
		return nil
	})
}

// CheckPerms 返回数据目录里不符合权限表的条目，供 doctor 使用
func (s *Store) CheckPerms() ([]string, error) {
	id, err := s.resolveIDs()
	if err != nil {
		return nil, err
	}
	var problems []string
	err = s.walk(func(path string, info fs.FileInfo, r rule) error {
		uid, mode := r.want(id, info)
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
			problems = append(problems, fmt.Sprintf("%s: owner uid %d, want %d", path, st.Uid, uid))
		}
		if mode != 0 && info.Mode().Perm() != mode {
			problems = append(problems, fmt.Sprintf("%s: mode %o, want %o", path, info.Mode().Perm(), mode))
		}
		return nil
	})
	return problems, err
}
