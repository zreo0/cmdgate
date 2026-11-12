// Package action 负责加载操作目录、校验定义和参数、计算目录 hash
package action

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// LevelAuto 表示直接执行
	LevelAuto = "auto"
	// LevelApprove 表示需要人工审批
	LevelApprove = "approve"

	// DefaultApprovalTimeout 是未声明时的审批超时
	DefaultApprovalTimeout = 30 * time.Minute
	// DefaultExecTimeout 是未声明时的执行超时
	DefaultExecTimeout = 10 * time.Minute
	// DefaultMaxFileSize 是文件参数未声明上限时的大小限制
	DefaultMaxFileSize = 1 << 20
)

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	paramRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	secretRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

// Param 描述一个参数的类型和约束
type Param struct {
	// 类型: string、enum、int、file
	Type string `yaml:"type" json:"type"`
	// enum 的可选值
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
	// string 需要完整匹配的正则
	Pattern string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	// int 的下限
	Min *int64 `yaml:"min,omitempty" json:"min,omitempty"`
	// int 的上限
	Max *int64 `yaml:"max,omitempty" json:"max,omitempty"`
	// file 的字节上限
	MaxSize int64 `yaml:"max_size,omitempty" json:"max_size,omitempty"`
	// 未传入时使用的默认值
	Default *string `yaml:"default,omitempty" json:"default,omitempty"`
	// 是否可以不传
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
	// 给调用者看的说明
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	re *regexp.Regexp
}

// Rule 是一条按参数值匹配的风险规则
type Rule struct {
	// 参数名到期望值，全部相等才算匹配
	When map[string]string `yaml:"when" json:"when"`
	// 匹配后的风险等级
	Level string `yaml:"level" json:"level"`
}

// Risk 是操作的风险策略，规则按顺序匹配，第一条命中的生效
type Risk struct {
	Default string `yaml:"default" json:"default"`
	Rules   []Rule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// Approval 是审批相关配置
type Approval struct {
	// 渠道名，省略时使用唯一已配置的渠道
	Channel string `yaml:"channel,omitempty"`
	// 审批超时
	Timeout time.Duration `yaml:"timeout,omitempty"`
	// 允许的审批人别名，省略表示渠道里配置的所有审批人
	Approvers []string `yaml:"approvers,omitempty"`
	// 是否允许批准自己发起的请求，省略为允许
	AllowSelf *bool `yaml:"allow_self,omitempty"`
}

// Exec 是执行相关配置
type Exec struct {
	Timeout time.Duration `yaml:"timeout,omitempty"`
}

// Action 是一个已加载的操作
type Action struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description,omitempty"`
	Params      map[string]Param `yaml:"params,omitempty"`
	Risk        Risk             `yaml:"risk,omitempty"`
	// 需要注入的凭证名
	Secrets  []string `yaml:"secrets,omitempty"`
	Approval Approval `yaml:"approval,omitempty"`
	// 允许发起请求的用户，省略表示不限制
	Requesters []string `yaml:"requesters,omitempty"`
	// 同时执行的上限，0 表示不限制
	Concurrency int  `yaml:"concurrency,omitempty"`
	Exec        Exec `yaml:"exec,omitempty"`

	// 操作目录的绝对路径
	Dir string `yaml:"-"`
	// 目录内容的 hash
	Hash string `yaml:"-"`
}

// Load 读取并校验一个操作目录
//
// 参数 dir 是包含 action.yaml 和 run 的目录
// 返回已填好默认值、Dir 和 Hash 的 Action
func Load(dir string) (*Action, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(abs, "action.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read action.yaml: %w", err)
	}
	var a Action
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// 拒绝未知字段，避免拼写错误的配置被静默忽略
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("parse action.yaml: %w", err)
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	if err := checkExecutable(filepath.Join(abs, "run"), true); err != nil {
		return nil, err
	}
	if err := checkExecutable(filepath.Join(abs, "preflight"), false); err != nil {
		return nil, err
	}
	hash, err := Hash(abs)
	if err != nil {
		return nil, err
	}
	a.Dir = abs
	a.Hash = hash
	return &a, nil
}

// Snapshot 将操作复制到 parent 下的独立目录，校验副本与原 hash 一致后返回
//
// 调用者负责删除返回的目录，执行只使用副本，避免安装更新改变已校验的路径
func Snapshot(a *Action, parent string) (*Action, error) {
	dir, err := os.MkdirTemp(parent, "action-")
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(a.Dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(a.Dir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed", path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600|info.Mode().Perm()&0o100)
	})
	var snapshot *Action
	if err == nil {
		snapshot, err = Load(dir)
		if err == nil && snapshot.Hash != a.Hash {
			err = fmt.Errorf("action changed while creating execution snapshot")
		}
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return snapshot, nil
}

// check 校验定义本身并填充默认值
func (a *Action) check() error {
	if !nameRe.MatchString(a.Name) {
		return fmt.Errorf("invalid action name %q", a.Name)
	}
	for name, p := range a.Params {
		if !paramRe.MatchString(name) {
			return fmt.Errorf("invalid param name %q", name)
		}
		switch p.Type {
		case "string":
			if p.Pattern != "" {
				re, err := regexp.Compile(`^(?:` + p.Pattern + `)$`)
				if err != nil {
					return fmt.Errorf("param %s: bad pattern: %w", name, err)
				}
				p.re = re
			}
		case "enum":
			if len(p.Values) == 0 {
				return fmt.Errorf("param %s: enum needs values", name)
			}
		case "int":
		case "file":
			if p.Default != nil {
				return fmt.Errorf("param %s: file cannot have a default", name)
			}
			if p.MaxSize <= 0 {
				p.MaxSize = DefaultMaxFileSize
			}
		default:
			return fmt.Errorf("param %s: unknown type %q", name, p.Type)
		}
		a.Params[name] = p
	}
	if a.Risk.Default == "" {
		a.Risk.Default = LevelApprove
	}
	if !validLevel(a.Risk.Default) {
		return fmt.Errorf("invalid risk.default %q", a.Risk.Default)
	}
	for i, r := range a.Risk.Rules {
		if !validLevel(r.Level) {
			return fmt.Errorf("risk.rules[%d]: invalid level %q", i, r.Level)
		}
		for k := range r.When {
			if _, ok := a.Params[k]; !ok {
				return fmt.Errorf("risk.rules[%d]: unknown param %q", i, k)
			}
		}
	}
	for _, s := range a.Secrets {
		if !secretRe.MatchString(s) {
			return fmt.Errorf("invalid secret name %q", s)
		}
		// CMDGATE_ 前缀留给内部变量和渠道凭证
		if strings.HasPrefix(s, "CMDGATE_") {
			return fmt.Errorf("secret name %q uses the reserved CMDGATE_ prefix", s)
		}
	}
	if a.Concurrency < 0 {
		return fmt.Errorf("concurrency must not be negative")
	}
	if a.Approval.Timeout <= 0 {
		a.Approval.Timeout = DefaultApprovalTimeout
	}
	if a.Exec.Timeout <= 0 {
		a.Exec.Timeout = DefaultExecTimeout
	}
	return nil
}

// ValidName 返回字符串是否是合法的操作名，用它拼路径前必须先校验
func ValidName(name string) bool {
	return nameRe.MatchString(name)
}

// validLevel 返回风险等级是否属于支持的策略
func validLevel(l string) bool {
	return l == LevelAuto || l == LevelApprove
}

// checkExecutable 确认脚本是可执行的普通文件
//
// 参数 required 为 false 时文件不存在不算错误
func checkExecutable(path string, required bool) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("%s must be an executable regular file", filepath.Base(path))
	}
	return nil
}

// AllowSelf 返回是否允许审批人批准自己发起的请求
func (a *Action) AllowSelf() bool {
	return a.Approval.AllowSelf == nil || *a.Approval.AllowSelf
}

// HasPreflight 返回操作是否带 preflight 脚本
func (a *Action) HasPreflight() bool {
	_, err := os.Stat(filepath.Join(a.Dir, "preflight"))
	return err == nil
}

// Validate 按定义校验调用者传入的参数
//
// 参数 given 是普通参数，files 是文件参数的内容
// 返回补全默认值后的普通参数，文件参数不在其中
func (a *Action) Validate(given map[string]string, files map[string][]byte) (map[string]string, error) {
	for k := range given {
		p, ok := a.Params[k]
		if !ok {
			return nil, fmt.Errorf("unknown param %q", k)
		}
		if p.Type == "file" {
			return nil, fmt.Errorf("param %q is a file, pass it with --file", k)
		}
	}
	for k := range files {
		if p, ok := a.Params[k]; !ok || p.Type != "file" {
			return nil, fmt.Errorf("%q is not a file param", k)
		}
	}
	out := map[string]string{}
	for name, p := range a.Params {
		if p.Type == "file" {
			content, ok := files[name]
			if !ok {
				if p.Optional {
					continue
				}
				return nil, fmt.Errorf("missing file param %q", name)
			}
			if int64(len(content)) > p.MaxSize {
				return nil, fmt.Errorf("file param %q exceeds %d bytes", name, p.MaxSize)
			}
			continue
		}
		v, ok := given[name]
		if !ok {
			if p.Default != nil {
				v = *p.Default
			} else if p.Optional {
				continue
			} else {
				return nil, fmt.Errorf("missing param %q", name)
			}
		}
		if err := p.checkValue(v); err != nil {
			return nil, fmt.Errorf("param %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// checkValue 校验单个参数值
func (p Param) checkValue(v string) error {
	// 环境变量不能包含 NUL
	if strings.ContainsRune(v, 0) {
		return fmt.Errorf("value contains NUL")
	}
	switch p.Type {
	case "string":
		if p.re != nil && !p.re.MatchString(v) {
			return fmt.Errorf("value does not match pattern %s", p.Pattern)
		}
	case "enum":
		for _, allowed := range p.Values {
			if v == allowed {
				return nil
			}
		}
		return fmt.Errorf("value must be one of %s", strings.Join(p.Values, ", "))
	case "int":
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("value is not an integer")
		}
		if p.Min != nil && n < *p.Min {
			return fmt.Errorf("value is below %d", *p.Min)
		}
		if p.Max != nil && n > *p.Max {
			return fmt.Errorf("value is above %d", *p.Max)
		}
	}
	return nil
}

// Level 根据参数值返回本次请求的风险等级
func (a *Action) Level(params map[string]string) string {
	for _, r := range a.Risk.Rules {
		matched := true
		for k, want := range r.When {
			if got, ok := params[k]; !ok || got != want {
				matched = false
				break
			}
		}
		if matched {
			return r.Level
		}
	}
	return a.Risk.Default
}

// Hash 计算目录内容的 sha256
//
// 路径、可执行位和内容都参与计算，目录里出现符号链接或特殊文件会报错，
// 避免操作指向目录之外的内容
func Hash(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files are allowed in an action", path)
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, path := range files {
		rel, _ := filepath.Rel(dir, path)
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		exec := info.Mode().Perm()&0o100 != 0
		fmt.Fprintf(h, "%s\x00%t\x00%d\x00", filepath.ToSlash(rel), exec, len(content))
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
