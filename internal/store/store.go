// Package store 管理数据目录：布局、配置、请求记录、审计日志、凭证和权限
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// Name 是项目名，服务用户、目录和服务标识都由它派生
	Name = "cmdgate"
	// DefaultRoot 是默认数据根目录
	DefaultRoot = "/opt/" + Name
	// DefaultServiceUser 是默认的服务用户名
	DefaultServiceUser = "_" + Name
	// LayoutVersion 是当前数据布局的版本
	LayoutVersion = 1
	// EnvDataDir 是覆盖数据根目录的环境变量
	EnvDataDir = "CMDGATE_DATA_DIR"

	// ModeDev 表示同用户运行，没有隔离
	ModeDev = "dev"
	// ModeLocal 表示独立用户，只监听本机 socket
	ModeLocal = "local"
	// ModeServer 表示额外监听网络
	ModeServer = "server"
)

// 请求的各个状态
const (
	StatePending   = "pending_approval"
	StateApproved  = "approved"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateDenied    = "denied"
	StateExpired   = "expired"
	StateCanceled  = "canceled"
	StateUnknown   = "unknown"
)

var secretNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// Terminal 返回状态是否为终态
func Terminal(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateDenied, StateExpired, StateCanceled, StateUnknown:
		return true
	}
	return false
}

// Config 是 config.yaml 的内容，不含任何凭证值
type Config struct {
	Mode string `yaml:"mode"`
	// 运行服务端的 OS 用户，dev 模式为空
	ServiceUser string `yaml:"service_user,omitempty"`
	// server 模式的监听地址，如 0.0.0.0:8443
	Listen  string `yaml:"listen,omitempty"`
	TLSCert string `yaml:"tls_cert,omitempty"`
	TLSKey  string `yaml:"tls_key,omitempty"`
	// 允许不带 TLS 监听网络
	Insecure bool `yaml:"insecure,omitempty"`
	// 每个请求者每分钟可发起的审批请求数，0 使用默认值
	RateLimitPerMinute int `yaml:"rate_limit_per_minute,omitempty"`
	// 渠道名到渠道配置
	Channels map[string]ChannelConfig `yaml:"channels,omitempty"`
}

// ChannelConfig 是一个审批渠道的配置
type ChannelConfig struct {
	// 渠道类型，如 telegram
	Type string `yaml:"type"`
	// 渠道自己的非敏感设置
	Settings map[string]string `yaml:"settings,omitempty"`
	// 审批人别名到平台用户 ID
	Approvers map[string]string `yaml:"approvers"`
}

// FileInfo 记录一个文件参数的快照信息
type FileInfo struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Handle 指向渠道里已发出的一条审批消息
type Handle struct {
	Chat    string `json:"chat"`
	Message string `json:"message"`
}

// Request 是一个请求的完整记录，对应 requests/<id>/request.json
type Request struct {
	ID        string            `json:"id"`
	Action    string            `json:"action"`
	Params    map[string]string `json:"params"`
	Files     []FileInfo        `json:"files,omitempty"`
	Requester string            `json:"requester"`
	// 请求创建时操作目录的 hash，执行前会再次比对
	Hash  string `json:"hash"`
	Risk  string `json:"risk"`
	State string `json:"state"`
	// 审批回调必须带回的一次性随机数
	Nonce string `json:"nonce,omitempty"`
	// 审批使用的渠道名
	Channel string `json:"channel,omitempty"`
	// 允许的审批人别名，空表示渠道里的所有审批人
	Approvers []string `json:"approvers,omitempty"`
	AllowSelf bool     `json:"allow_self,omitempty"`
	// 完整审批内容已经交付，旧记录默认关闭渠道审批，管理员仍可处理
	ApprovalDelivered bool     `json:"approval_delivered,omitempty"`
	Handles           []Handle `json:"handles,omitempty"`
	Approver          string   `json:"approver,omitempty"`
	// 失败、拒绝等情况的说明
	Reason    string `json:"reason,omitempty"`
	Preflight string `json:"preflight,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	DecidedAt  *time.Time `json:"decided_at,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Event 是审计日志里的一行
type Event struct {
	Time      time.Time         `json:"time"`
	Event     string            `json:"event"`
	RequestID string            `json:"request_id,omitempty"`
	Action    string            `json:"action,omitempty"`
	Requester string            `json:"requester,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	Files     []FileInfo        `json:"files,omitempty"`
	Hash      string            `json:"hash,omitempty"`
	Approver  string            `json:"approver,omitempty"`
	State     string            `json:"state,omitempty"`
	ExitCode  *int              `json:"exit_code,omitempty"`
	Detail    string            `json:"detail,omitempty"`
}

// LockEntry 记录一个已安装操作的 hash
type LockEntry struct {
	Hash        string    `yaml:"hash"`
	InstalledAt time.Time `yaml:"installed_at"`
}

// User 是 server 模式的一个用户
type User struct {
	Name string `yaml:"name"`
	// 令牌的 sha256，服务端不保存令牌本身
	TokenHash string    `yaml:"token_hash"`
	CreatedAt time.Time `yaml:"created_at"`
}

// Store 是对数据根目录的访问入口
type Store struct {
	Root string
}

// Open 返回数据目录的访问入口，目录优先取环境变量，其次取默认路径
func Open() *Store {
	if dir := os.Getenv(EnvDataDir); dir != "" {
		return &Store{Root: dir}
	}
	return &Store{Root: DefaultRoot}
}

// Path 拼出数据目录下的路径
func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Root}, parts...)...)
}

// Socket 返回客户端连接用的 unix socket 路径
func (s *Store) Socket() string {
	return s.Path("run", Name+".sock")
}

// Init 创建数据目录的骨架并写入配置，目录已初始化时报错
func (s *Store) Init(cfg *Config) error {
	if _, err := os.Stat(s.Path("config.yaml")); err == nil {
		return fmt.Errorf("%s is already initialised", s.Root)
	}
	for _, d := range []string{"", "actions", "secrets", "requests", "audit", "home", "run", "log"} {
		if err := os.MkdirAll(s.Path(d), 0o755); err != nil {
			return err
		}
	}
	if err := writeAtomic(s.Path("VERSION"), []byte(fmt.Sprintf("%d\n", LayoutVersion)), 0o640); err != nil {
		return err
	}
	if err := s.SaveConfig(cfg); err != nil {
		return err
	}
	return s.FixPerms()
}

// CheckVersion 确认数据布局版本是当前程序支持的
//
// 目前只有一个版本，以后新增布局时在这里做向前迁移
func (s *Store) CheckVersion() error {
	data, err := os.ReadFile(s.Path("VERSION"))
	if err != nil {
		return fmt.Errorf("%s is not initialised: %w", s.Root, err)
	}
	var v int
	if _, err := fmt.Sscanf(string(data), "%d", &v); err != nil {
		return fmt.Errorf("bad VERSION file: %w", err)
	}
	if v != LayoutVersion {
		return fmt.Errorf("data layout version %d is not supported by this binary (wants %d)", v, LayoutVersion)
	}
	return nil
}

// LoadConfig 读取 config.yaml
func (s *Store) LoadConfig() (*Config, error) {
	var cfg Config
	if err := readYAML(s.Path("config.yaml"), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// SaveConfig 写入 config.yaml
func (s *Store) SaveConfig(cfg *Config) error {
	return writeYAML(s.Path("config.yaml"), cfg, 0o640)
}

// LoadLock 读取 actions.lock，文件不存在时返回空表
func (s *Store) LoadLock() (map[string]LockEntry, error) {
	lock := map[string]LockEntry{}
	err := readYAML(s.Path("actions.lock"), &lock)
	if errors.Is(err, os.ErrNotExist) {
		return lock, nil
	}
	return lock, err
}

// SaveLock 写入 actions.lock
func (s *Store) SaveLock(lock map[string]LockEntry) error {
	return writeYAML(s.Path("actions.lock"), lock, 0o640)
}

// LoadUsers 读取 users.yaml，文件不存在时返回空列表
func (s *Store) LoadUsers() ([]User, error) {
	var users []User
	err := readYAML(s.Path("users.yaml"), &users)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return users, err
}

// SaveUsers 写入 users.yaml
func (s *Store) SaveUsers(users []User) error {
	return writeYAML(s.Path("users.yaml"), users, 0o640)
}

// RequestDir 返回一个请求的目录
func (s *Store) RequestDir(id string) string {
	return s.Path("requests", id)
}

// SaveRequest 原子地写入请求记录
func (s *Store) SaveRequest(r *Request) error {
	data, err := json.MarshalIndent(r, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.RequestDir(r.ID), 0o700); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.RequestDir(r.ID), "request.json"), data, 0o600); err != nil {
		return err
	}
	// 首次创建的请求目录本身也必须持久化，避免重启后丢失执行记录
	dir, err := os.Open(s.Path("requests"))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// LoadRequest 读取一个请求记录
func (s *Store) LoadRequest(id string) (*Request, error) {
	// ID 只允许十六进制，防止用它拼出目录之外的路径
	if !validID(id) {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(filepath.Join(s.RequestDir(id), "request.json"))
	if err != nil {
		return nil, err
	}
	var r Request
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRequests 读取全部请求记录，按创建时间从新到旧排序
func (s *Store) ListRequests() ([]*Request, error) {
	entries, err := os.ReadDir(s.Path("requests"))
	if err != nil {
		return nil, err
	}
	var out []*Request
	for _, e := range entries {
		r, err := s.LoadRequest(e.Name())
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// validID 返回请求 ID 是否满足安全路径片段的约束
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Audit 向当月的审计日志追加一行
//
// 用 O_APPEND 写入，服务端和管理命令同时写也不会互相覆盖
func (s *Store) Audit(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	path := s.Path("audit", ev.Time.Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// AuditTail 返回最近的 n 行审计日志
func (s *Store) AuditTail(n int) ([]string, error) {
	files, err := filepath.Glob(s.Path("audit", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var lines []string
	// 从最新的月份往回读，凑够 n 行为止
	for i := len(files) - 1; i >= 0 && len(lines) < n; i-- {
		f, err := os.Open(files[i])
		if err != nil {
			return nil, err
		}
		var cur []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			cur = append(cur, sc.Text())
		}
		f.Close()
		lines = append(cur, lines...)
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// SetSecret 保存一个凭证
func (s *Store) SetSecret(name, value string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q, use upper case letters, digits and underscores", name)
	}
	return writeAtomic(s.Path("secrets", name), []byte(value), 0o600)
}

// GetSecret 读取一个凭证的值
func (s *Store) GetSecret(name string) (string, error) {
	if !secretNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid secret name %q", name)
	}
	data, err := os.ReadFile(s.Path("secrets", name))
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return string(data), nil
}

// DeleteSecret 删除一个凭证
func (s *Store) DeleteSecret(name string) error {
	if !secretNameRe.MatchString(name) {
		return fmt.Errorf("invalid secret name %q", name)
	}
	return os.Remove(s.Path("secrets", name))
}

// SecretInfo 是凭证的元信息，不含值
type SecretInfo struct {
	Name      string
	UpdatedAt time.Time
}

// ListSecrets 列出全部凭证的名字和更新时间
func (s *Store) ListSecrets() ([]SecretInfo, error) {
	entries, err := os.ReadDir(s.Path("secrets"))
	if err != nil {
		return nil, err
	}
	var out []SecretInfo
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, SecretInfo{Name: e.Name(), UpdatedAt: info.ModTime()})
	}
	return out, nil
}

// writeAtomic 先写临时文件再改名，保证读到的文件总是完整的
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// readYAML 读取路径中的 YAML 并解码到 v
func readYAML(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, v)
}

// writeYAML 将 v 编码为 YAML，以指定权限原子写入路径
func writeYAML(path string, v any, mode os.FileMode) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, data, mode)
}
