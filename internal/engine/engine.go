// Package engine 实现请求的生命周期：校验、preflight、审批、执行和恢复
//
// 所有与安全相关的判断都在这里：审批必须带回请求的一次性随机数，
// 审批人必须在允许名单内，执行前会再次比对操作目录的 hash
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cmdgate/internal/action"
	"cmdgate/internal/channel"
	"cmdgate/internal/store"
)

const (
	// 未配置时每个请求者每分钟可发起的审批请求数
	defaultRatePerMinute = 10
	// 向渠道发送或更新消息的超时
	channelTimeout = 20 * time.Second
	// 写进请求记录的 preflight 输出上限
	maxPreflightBytes = 2000
)

// Identity 是发起调用的身份
type Identity struct {
	Name string
	// 是否可以使用管理接口，只有本机 socket 上的 root 或服务端自身用户才有
	Admin bool
}

// Approver 是做出决定的一方
type Approver struct {
	// 决定来自哪个渠道
	Channel string
	// 渠道验证过的平台用户 ID
	ID string
	// 是否来自管理员的应急入口，为 true 时不校验随机数和审批人名单
	Admin bool
	// 管理员的名字，仅用于审计
	Name string
}

// Error 是带 HTTP 状态码的错误，服务端直接据此返回
type Error struct {
	Status int
	Msg    string
}

// Error 返回面向调用者的错误说明
func (e *Error) Error() string { return e.Msg }

// errf 构造带 HTTP 状态码的格式化错误
func errf(status int, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// entry 是一个未结束请求在内存里的状态
type entry struct {
	req *store.Request
	// 请求进入终态时关闭
	done chan struct{}
	// 审批过期定时器
	timer *time.Timer
	// 批准后用于取消排队和执行
	ctx    context.Context
	cancel context.CancelFunc
	// 是否被调用者取消
	canceled bool
}

// Engine 管理所有请求
type Engine struct {
	st       *store.Store
	cfg      *store.Config
	channels map[string]channel.Channel
	log      *log.Logger

	// 保护下面的字段以及所有 entry.req 的可变部分
	mu     sync.Mutex
	active map[string]*entry
	// 每个操作的并发信号量
	sems map[string]chan struct{}
	// 每个请求者最近发起审批请求的时间
	rate     map[string][]time.Time
	stopping bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New 创建引擎
//
// 参数 channels 是渠道名到已打开渠道的映射，可以为空
func New(st *store.Store, cfg *store.Config, channels map[string]channel.Channel, logger *log.Logger) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{
		st:       st,
		cfg:      cfg,
		channels: channels,
		log:      logger,
		active:   map[string]*entry{},
		sems:     map[string]chan struct{}{},
		rate:     map[string][]time.Time{},
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Start 恢复上次未结束的请求，并开始接收各渠道的决定
func (e *Engine) Start() error {
	if err := e.recover(); err != nil {
		return err
	}
	for name, ch := range e.channels {
		decisions, err := ch.Decisions(e.ctx)
		if err != nil {
			return fmt.Errorf("channel %s: %w", name, err)
		}
		e.wg.Add(1)
		go func(name string) {
			defer e.wg.Done()
			for d := range decisions {
				by := Approver{Channel: name, ID: d.ApproverID}
				if err := e.Decide(d.RequestID, d.Nonce, d.Approve, by); err != nil {
					e.log.Printf("decision rejected: request=%s channel=%s approver=%s: %v", d.RequestID, name, d.ApproverID, err)
					e.st.Audit(store.Event{Event: "decision_rejected", RequestID: d.RequestID, Approver: name + ":" + d.ApproverID, Detail: err.Error()})
				}
			}
		}(name)
	}
	return nil
}

// Stop 停止引擎
//
// 正在执行的脚本会被终止并标记为结果未知，待审批和排队中的请求保留在磁盘上，下次启动时恢复
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopping = true
	for _, ent := range e.active {
		if ent.timer != nil {
			ent.timer.Stop()
		}
	}
	e.mu.Unlock()
	e.cancel()
	e.wg.Wait()
}

// recover 在启动时处理磁盘上未结束的请求
//
// 待审批的继续等待，已批准未开始的重新排队，执行中被打断的标记为结果未知且不重试，
// 因为无法知道脚本已经做到了哪一步
func (e *Engine) recover() error {
	reqs, err := e.st.ListRequests()
	if err != nil {
		return err
	}
	for _, r := range reqs {
		if store.Terminal(r.State) {
			continue
		}
		ent := &entry{req: r, done: make(chan struct{})}
		e.mu.Lock()
		e.active[r.ID] = ent
		e.mu.Unlock()
		switch r.State {
		case store.StatePending:
			if r.ExpiresAt == nil || !time.Now().Before(*r.ExpiresAt) {
				e.finish(ent, store.StateExpired, "approval timed out", nil)
				continue
			}
			e.mu.Lock()
			e.armTimer(ent)
			e.mu.Unlock()
		case store.StateApproved:
			e.mu.Lock()
			ent.ctx, ent.cancel = context.WithCancel(e.ctx)
			e.mu.Unlock()
			e.wg.Add(1)
			go e.run(ent)
		case store.StateRunning:
			e.finish(ent, store.StateUnknown, "server restarted during execution", nil)
		}
	}
	return nil
}

// List 返回调用者可以发起的全部操作，按名字排序
//
// 加载失败或与安装记录不一致的操作会被跳过并记日志
func (e *Engine) List(id Identity) []*action.Action {
	entries, err := os.ReadDir(e.st.Path("actions"))
	if err != nil {
		return nil
	}
	var out []*action.Action
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		act, err := e.loadAction(ent.Name())
		if err != nil {
			e.log.Printf("skip action %s: %v", ent.Name(), err)
			continue
		}
		if allowed(act.Requesters, id.Name) {
			out = append(out, act)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Describe 返回一个调用者可以发起的操作
func (e *Engine) Describe(id Identity, name string) (*action.Action, error) {
	act, err := e.loadAction(name)
	if err != nil {
		return nil, err
	}
	if !allowed(act.Requesters, id.Name) {
		return nil, errf(http.StatusNotFound, "action %q not found", name)
	}
	return act, nil
}

// loadAction 加载一个已安装的操作，并确认它与安装时记录的 hash 一致
func (e *Engine) loadAction(name string) (*action.Action, error) {
	if !action.ValidName(name) {
		return nil, errf(http.StatusNotFound, "action %q not found", name)
	}
	dir := e.st.Path("actions", name)
	if _, err := os.Stat(dir); err != nil {
		return nil, errf(http.StatusNotFound, "action %q not found", name)
	}
	act, err := action.Load(dir)
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "action %q is broken: %v", name, err)
	}
	lock, err := e.st.LoadLock()
	if err != nil {
		return nil, errf(http.StatusInternalServerError, "read actions.lock: %v", err)
	}
	if lock[name].Hash != act.Hash {
		return nil, errf(http.StatusConflict, "action %q does not match its installed hash, reinstall it with admin sync", name)
	}
	return act, nil
}

// Submit 校验并创建一个请求
//
// 参数 params 是普通参数，files 是文件参数的内容
// auto 级别的请求会立即开始执行，approve 级别的发给审批渠道后返回
// 返回创建好的请求；校验不通过时返回 *Error，不创建请求
func (e *Engine) Submit(id Identity, name string, params map[string]string, files map[string][]byte) (*store.Request, error) {
	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		return nil, errf(http.StatusServiceUnavailable, "server is stopping")
	}
	e.wg.Add(1)
	e.mu.Unlock()
	defer e.wg.Done()
	req, err := e.submit(id, name, params, files)
	if err != nil {
		e.st.Audit(store.Event{Event: "request_rejected", Action: name, Requester: id.Name, Params: params, Detail: err.Error()})
	}
	return req, err
}

// submit 执行请求校验、预检和审批分发，调用者负责注册生命周期等待
func (e *Engine) submit(id Identity, name string, params map[string]string, files map[string][]byte) (*store.Request, error) {
	act, err := e.loadAction(name)
	if err != nil {
		return nil, err
	}
	if !allowed(act.Requesters, id.Name) {
		return nil, errf(http.StatusNotFound, "action %q not found", name)
	}
	norm, err := act.Validate(params, files)
	if err != nil {
		return nil, errf(http.StatusBadRequest, "%v", err)
	}
	level := act.Level(norm)

	req := &store.Request{
		ID:        randomHex(8),
		Action:    act.Name,
		Params:    norm,
		Requester: id.Name,
		Hash:      act.Hash,
		Risk:      level,
		CreatedAt: time.Now(),
	}
	var recipients []string
	if level == action.LevelApprove {
		// 审批相关的配置在创建请求前全部确定，任何一项有问题都直接拒绝
		req.Channel, recipients, err = e.resolveApproval(act)
		if err != nil {
			return nil, err
		}
		if !e.allowRate(id.Name) {
			return nil, errf(http.StatusTooManyRequests, "too many approval requests, try again in a minute")
		}
		req.Approvers = act.Approval.Approvers
		req.AllowSelf = act.AllowSelf()
		req.Nonce = randomHex(16)
		expires := req.CreatedAt.Add(act.Approval.Timeout)
		req.ExpiresAt = &expires
	}

	dir := e.st.RequestDir(req.ID)
	if err := os.MkdirAll(filepath.Join(dir, "inputs"), 0o700); err != nil {
		return nil, errf(http.StatusInternalServerError, "%v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		return nil, errf(http.StatusInternalServerError, "%v", err)
	}
	for fname, content := range files {
		if err := os.WriteFile(filepath.Join(dir, "inputs", fname), content, 0o600); err != nil {
			return nil, errf(http.StatusInternalServerError, "%v", err)
		}
		sum := sha256.Sum256(content)
		req.Files = append(req.Files, store.FileInfo{Name: fname, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
	}
	sort.Slice(req.Files, func(i, j int) bool { return req.Files[i].Name < req.Files[j].Name })

	ent := &entry{req: req, done: make(chan struct{})}

	// preflight 不注入任何凭证，失败则请求直接结束
	if act.HasPreflight() {
		snapshot, err := action.Snapshot(act, dir)
		if err != nil {
			return nil, errf(http.StatusConflict, "%v", err)
		}
		defer os.RemoveAll(snapshot.Dir)
		ctx, cancel := context.WithTimeout(e.ctx, preflightTimeout)
		out := filepath.Join(dir, "preflight.log")
		code, truncated, err := runScript(ctx, filepath.Join(snapshot.Dir, "preflight"), filepath.Join(dir, "work"), e.scriptEnv(req, nil), out)
		cancel()
		data, _ := os.ReadFile(out)
		data = redact(data, nil, truncated)
		req.Preflight = tail(data, maxPreflightBytes)
		if err != nil || code != 0 {
			reason := fmt.Sprintf("preflight failed with exit code %d", code)
			if err != nil {
				reason = "preflight failed: " + err.Error()
			}
			os.WriteFile(filepath.Join(dir, "output.log"), data, 0o600)
			e.mu.Lock()
			e.active[req.ID] = ent
			e.mu.Unlock()
			e.audit("request_created", req, "")
			return e.finish(ent, store.StateFailed, reason, nil), nil
		}
	}

	e.mu.Lock()
	if e.stopping {
		e.mu.Unlock()
		return nil, errf(http.StatusServiceUnavailable, "server is stopping")
	}
	if level == action.LevelAuto {
		req.State = store.StateApproved
		ent.ctx, ent.cancel = context.WithCancel(e.ctx)
	} else {
		req.State = store.StatePending
	}
	err = e.st.SaveRequest(req)
	if err != nil {
		if ent.cancel != nil {
			ent.cancel()
		}
		e.mu.Unlock()
		return nil, errf(http.StatusInternalServerError, "%v", err)
	}
	e.active[req.ID] = ent
	if level == action.LevelAuto {
		e.wg.Add(1)
	} else {
		e.armTimer(ent)
	}
	snapshot := *req
	e.mu.Unlock()
	e.audit("request_created", &snapshot, "")

	if level == action.LevelAuto {
		go e.run(ent)
		return &snapshot, nil
	}
	e.sendApproval(ent, act, recipients)
	return e.snapshot(ent), nil
}

// resolveApproval 确定一个操作使用的审批渠道和接收人
//
// 返回渠道名和接收人的平台用户 ID；没有配置任何渠道时渠道名为空，
// 这时请求只能通过管理员的应急入口批准
func (e *Engine) resolveApproval(act *action.Action) (string, []string, error) {
	name := act.Approval.Channel
	if name == "" {
		switch len(e.cfg.Channels) {
		case 0:
			return "", nil, nil
		case 1:
			for n := range e.cfg.Channels {
				name = n
			}
		default:
			return "", nil, errf(http.StatusConflict, "action %q must set approval.channel because several channels are configured", act.Name)
		}
	}
	cfg, ok := e.cfg.Channels[name]
	if !ok || e.channels[name] == nil {
		return "", nil, errf(http.StatusConflict, "approval channel %q is not configured", name)
	}
	aliases := act.Approval.Approvers
	if len(aliases) == 0 {
		for alias := range cfg.Approvers {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
	}
	var recipients []string
	for _, alias := range aliases {
		pid, ok := cfg.Approvers[alias]
		if !ok {
			return "", nil, errf(http.StatusConflict, "approver %q is not configured in channel %q", alias, name)
		}
		recipients = append(recipients, pid)
	}
	if len(recipients) == 0 {
		return "", nil, errf(http.StatusConflict, "channel %q has no approvers", name)
	}
	return name, recipients, nil
}

// sendApproval 把审批请求发到渠道
//
// 发送失败时请求保持待审批，不会执行，原因记在请求上，管理员可用应急入口处理
func (e *Engine) sendApproval(ent *entry, act *action.Action, recipients []string) {
	req := e.snapshot(ent)
	ch := e.channels[req.Channel]
	if ch == nil {
		e.setReason(ent, "no approval channel configured, an administrator must run: admin approve "+req.ID)
		return
	}
	msg := channel.Request{
		ID:          req.ID,
		Nonce:       req.Nonce,
		Action:      req.Action,
		Description: act.Description,
		Requester:   req.Requester,
		Hash:        req.Hash,
		Preflight:   req.Preflight,
		ExpiresAt:   *req.ExpiresAt,
		Recipients:  recipients,
	}
	keys := make([]string, 0, len(req.Params))
	for k := range req.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		msg.Params = append(msg.Params, channel.KV{Key: k, Value: req.Params[k]})
	}
	for _, f := range req.Files {
		content, err := os.ReadFile(filepath.Join(e.st.RequestDir(req.ID), "inputs", f.Name))
		if err != nil {
			e.setReason(ent, "approval input could not be read: "+err.Error())
			return
		}
		msg.Files = append(msg.Files, channel.File{FileInfo: f, Content: content})
	}
	ctx, cancel := context.WithTimeout(e.ctx, channelTimeout)
	defer cancel()
	handles, err := ch.Send(ctx, msg)
	if err != nil {
		e.log.Printf("request %s: send approval: %v", req.ID, err)
		e.setReason(ent, "approval message could not be sent: "+err.Error())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := *ent.req
	next.Handles = handles
	next.ApprovalDelivered = err == nil && len(handles) > 0
	if saveErr := e.st.SaveRequest(&next); saveErr != nil {
		e.log.Printf("request %s: save delivery: %v", req.ID, saveErr)
		return
	}
	ent.req.Handles = handles
	ent.req.ApprovalDelivered = next.ApprovalDelivered
}

// Decide 处理一次批准或拒绝
//
// 参数 nonce 必须等于请求创建时生成的随机数，by 描述决定的来源
// 校验通过后请求进入已批准或已拒绝，同一个请求只能被决定一次
func (e *Engine) Decide(id, nonce string, approve bool, by Approver) error {
	e.mu.Lock()
	ent := e.active[id]
	if ent == nil || ent.req.State != store.StatePending || e.stopping {
		e.mu.Unlock()
		return errf(http.StatusConflict, "request %s is not waiting for approval", id)
	}
	req := ent.req
	name := "admin:" + by.Name
	if !by.Admin {
		if !req.ApprovalDelivered {
			e.mu.Unlock()
			return errf(http.StatusConflict, "approval content has not been delivered")
		}
		alias, err := e.authorize(req, nonce, by)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		name = alias
		// 管理员入口是渠道故障时的后备，所以只有渠道来的决定才检查过期
		if req.ExpiresAt != nil && !time.Now().Before(*req.ExpiresAt) {
			e.mu.Unlock()
			return errf(http.StatusConflict, "request %s has expired", id)
		}
	}
	now := time.Now()
	next := *req
	next.Approver = name
	next.DecidedAt = &now
	next.State = store.StateApproved
	next.Reason = ""
	if !approve {
		next.State = store.StateDenied
	}
	if err := e.st.SaveRequest(&next); err != nil {
		e.mu.Unlock()
		return errf(http.StatusInternalServerError, "save decision: %v", err)
	}
	req.Approver, req.DecidedAt = name, &now
	if ent.timer != nil {
		ent.timer.Stop()
	}
	if !approve {
		e.finishLocked(ent, store.StateDenied, "denied by "+name, nil)
		e.mu.Unlock()
		e.notify(ent, "Denied by "+name)
		return nil
	}
	req.State, req.Reason = store.StateApproved, ""
	ent.ctx, ent.cancel = context.WithCancel(e.ctx)
	snapshot := *req
	e.wg.Add(1)
	e.mu.Unlock()

	e.audit("request_approved", &snapshot, "")
	e.notify(ent, "Approved by "+name+", running")
	go e.run(ent)
	return nil
}

// authorize 校验来自渠道的决定，调用时必须持有 e.mu
//
// 返回审批人的别名
func (e *Engine) authorize(req *store.Request, nonce string, by Approver) (string, error) {
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(req.Nonce)) != 1 {
		return "", errf(http.StatusForbidden, "nonce does not match")
	}
	if by.Channel != req.Channel {
		return "", errf(http.StatusForbidden, "decision came from channel %q, request uses %q", by.Channel, req.Channel)
	}
	alias := ""
	for a, pid := range e.cfg.Channels[req.Channel].Approvers {
		if pid == by.ID {
			alias = a
			break
		}
	}
	if alias == "" {
		return "", errf(http.StatusForbidden, "user %s is not an approver", by.ID)
	}
	if len(req.Approvers) > 0 && !contains(req.Approvers, alias) {
		return "", errf(http.StatusForbidden, "approver %s is not allowed for this action", alias)
	}
	if !req.AllowSelf && alias == req.Requester {
		return "", errf(http.StatusForbidden, "approver %s cannot approve their own request", alias)
	}
	return alias, nil
}

// run 执行一个已批准的请求，在独立的 goroutine 里运行
func (e *Engine) run(ent *entry) {
	defer e.wg.Done()
	// ID、Action、Params、Files、Hash、Requester 创建后不再变化，可以不加锁读取
	req := ent.req
	dir := e.st.RequestDir(req.ID)

	// 重新加载并比对 hash，保证执行的就是创建请求时的那份脚本
	act, err := action.Load(e.st.Path("actions", req.Action))
	if err != nil || act.Hash != req.Hash {
		e.finish(ent, store.StateFailed, "action changed after the request was created", nil)
		e.notify(ent, "Not executed: action changed after the request was created")
		return
	}

	if sem := e.sem(act); sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ent.ctx.Done():
			e.abortQueued(ent)
			return
		}
	}

	snapshotAction, err := action.Snapshot(act, dir)
	if err != nil {
		e.finish(ent, store.StateFailed, "action changed or snapshot failed: "+err.Error(), nil)
		return
	}
	defer os.RemoveAll(snapshotAction.Dir)
	act = snapshotAction

	secrets := map[string]string{}
	for _, name := range act.Secrets {
		value, err := e.st.GetSecret(name)
		if err != nil {
			e.finish(ent, store.StateFailed, "secret "+name+" is not set", nil)
			e.notify(ent, "Not executed: secret "+name+" is not set")
			return
		}
		secrets[name] = value
	}

	e.mu.Lock()
	if ent.canceled || e.stopping || req.State != store.StateApproved {
		e.mu.Unlock()
		e.abortQueued(ent)
		return
	}
	now := time.Now()
	next := *req
	next.State, next.StartedAt = store.StateRunning, &now
	if err := e.st.SaveRequest(&next); err != nil {
		e.finishLocked(ent, store.StateFailed, "save execution state: "+err.Error(), nil)
		e.mu.Unlock()
		return
	}
	req.State, req.StartedAt = next.State, next.StartedAt
	snapshot := *req
	e.mu.Unlock()
	e.audit("request_started", &snapshot, "")

	ctx, cancel := context.WithTimeout(ent.ctx, act.Exec.Timeout)
	raw := filepath.Join(dir, "output.raw")
	code, truncated, runErr := runScript(ctx, filepath.Join(act.Dir, "run"), filepath.Join(dir, "work"), e.scriptEnv(req, secrets), raw)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	cancel()

	// 脱敏后的输出才落到 output.log，原始输出不保留
	data, _ := os.ReadFile(raw)
	os.WriteFile(filepath.Join(dir, "output.log"), redact(data, secrets, truncated), 0o600)
	os.Remove(raw)

	e.mu.Lock()
	stopping, canceled := e.stopping, ent.canceled
	e.mu.Unlock()
	var final *store.Request
	switch {
	case stopping:
		final = e.finish(ent, store.StateUnknown, "server stopped during execution", nil)
	case canceled:
		final = e.finish(ent, store.StateCanceled, "canceled during execution", nil)
	case timedOut:
		final = e.finish(ent, store.StateFailed, "timed out after "+act.Exec.Timeout.String(), nil)
	case runErr != nil:
		final = e.finish(ent, store.StateFailed, runErr.Error(), nil)
	case code == 0:
		final = e.finish(ent, store.StateSucceeded, "", &code)
	default:
		final = e.finish(ent, store.StateFailed, fmt.Sprintf("exit code %d", code), &code)
	}
	status := "Finished: " + final.State
	if final.Reason != "" {
		status += " (" + final.Reason + ")"
	}
	e.notify(ent, status)
}

// abortQueued 处理还没开始执行就被中断的请求
//
// 被调用者取消的标记为已取消；因为服务端停止而中断的保持已批准，下次启动时重新排队
func (e *Engine) abortQueued(ent *entry) {
	e.mu.Lock()
	canceled := ent.canceled
	e.mu.Unlock()
	if canceled {
		e.finish(ent, store.StateCanceled, "canceled before execution", nil)
		e.notify(ent, "Canceled before execution")
	}
}

// Cancel 取消一个未结束的请求，只有发起者和管理员可以取消
func (e *Engine) Cancel(id string, who Identity) error {
	e.mu.Lock()
	ent := e.active[id]
	if ent == nil || !visible(ent.req, who) {
		e.mu.Unlock()
		if r, err := e.st.LoadRequest(id); err == nil && visible(r, who) {
			return errf(http.StatusConflict, "request %s is already %s", id, r.State)
		}
		return errf(http.StatusNotFound, "request %s not found", id)
	}
	if ent.req.State == store.StatePending || ent.req.State == store.StateApproved {
		next := *ent.req
		next.State = store.StateCanceled
		if err := e.st.SaveRequest(&next); err != nil {
			e.mu.Unlock()
			return errf(http.StatusInternalServerError, "save cancellation: %v", err)
		}
		if ent.timer != nil {
			ent.timer.Stop()
		}
		e.finishLocked(ent, store.StateCanceled, "canceled by "+who.Name, nil)
		e.mu.Unlock()
		e.notify(ent, "Canceled by "+who.Name)
		return nil
	}
	// 排队或执行中的请求由 run 负责收尾
	ent.canceled = true
	ent.cancel()
	e.mu.Unlock()
	return nil
}

// Get 返回一个请求的当前状态，只有发起者和管理员可以查看
func (e *Engine) Get(id string, who Identity) (*store.Request, error) {
	e.mu.Lock()
	if ent := e.active[id]; ent != nil {
		snapshot := *ent.req
		e.mu.Unlock()
		if !visible(&snapshot, who) {
			return nil, errf(http.StatusNotFound, "request %s not found", id)
		}
		return &snapshot, nil
	}
	e.mu.Unlock()
	r, err := e.st.LoadRequest(id)
	if err != nil || !visible(r, who) {
		return nil, errf(http.StatusNotFound, "request %s not found", id)
	}
	return r, nil
}

// Wait 等待请求进入终态，最多等 d，然后返回当前状态
func (e *Engine) Wait(ctx context.Context, id string, who Identity, d time.Duration) (*store.Request, error) {
	if _, err := e.Get(id, who); err != nil {
		return nil, err
	}
	e.mu.Lock()
	ent := e.active[id]
	e.mu.Unlock()
	if ent != nil && d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ent.done:
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return e.Get(id, who)
}

// Output 返回一个已结束请求的输出末尾，输出已经过脱敏
func (e *Engine) Output(id string) string {
	data, err := os.ReadFile(filepath.Join(e.st.RequestDir(id), "output.log"))
	if err != nil {
		return ""
	}
	return tail(data, maxViewBytes)
}

// finish 把请求置为终态并落盘，重复调用时只有第一次生效
//
// 返回终态的请求副本
func (e *Engine) finish(ent *entry, state, reason string, exitCode *int) *store.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.finishLocked(ent, state, reason, exitCode)
}

// finishLocked 在持有 e.mu 时完成终态转换，调用方可将检查和转换保持原子性
func (e *Engine) finishLocked(ent *entry, state, reason string, exitCode *int) *store.Request {
	if store.Terminal(ent.req.State) {
		snapshot := *ent.req
		return &snapshot
	}
	now := time.Now()
	ent.req.State = state
	ent.req.Reason = reason
	ent.req.ExitCode = exitCode
	ent.req.FinishedAt = &now
	if err := e.st.SaveRequest(ent.req); err != nil {
		e.log.Printf("request %s: save: %v", ent.req.ID, err)
	}
	if ent.timer != nil {
		ent.timer.Stop()
	}
	if ent.cancel != nil {
		ent.cancel()
	}
	delete(e.active, ent.req.ID)
	close(ent.done)
	snapshot := *ent.req
	e.audit("request_finished", &snapshot, reason)
	return &snapshot
}

// armTimer 为待审批的请求设置过期定时器，调用时必须持有 e.mu
func (e *Engine) armTimer(ent *entry) {
	d := time.Until(*ent.req.ExpiresAt)
	ent.timer = time.AfterFunc(d, func() {
		e.mu.Lock()
		pending := ent.req.State == store.StatePending && !e.stopping
		if pending {
			e.finishLocked(ent, store.StateExpired, "approval timed out", nil)
		}
		e.mu.Unlock()
		if pending {
			e.notify(ent, "Expired")
		}
	})
}

// notify 在渠道的审批消息上标记最新状态，失败只记日志
func (e *Engine) notify(ent *entry, status string) {
	req := e.snapshot(ent)
	ch := e.channels[req.Channel]
	if ch == nil {
		return
	}
	// 服务端停止时也要把最后的状态发出去，所以不用 e.ctx
	ctx, cancel := context.WithTimeout(context.Background(), channelTimeout)
	defer cancel()
	for _, h := range req.Handles {
		if err := ch.Update(ctx, h, status); err != nil {
			e.log.Printf("request %s: update message: %v", req.ID, err)
		}
	}
}

// scriptEnv 构造脚本的环境变量
//
// 只包含固定的基础变量、参数和显式声明的凭证，不继承服务端自己的环境
func (e *Engine) scriptEnv(req *store.Request, secrets map[string]string) []string {
	path := defaultPath
	work := filepath.Join(e.st.RequestDir(req.ID), "work")
	env := []string{
		"PATH=" + path,
		"HOME=" + e.st.Path("home"),
		"TMPDIR=" + work,
		"CMDGATE_REQUEST_ID=" + req.ID,
		"CMDGATE_ACTION=" + req.Action,
		"CMDGATE_REQUESTER=" + req.Requester,
	}
	for k, v := range req.Params {
		env = append(env, "CMDGATE_PARAM_"+strings.ToUpper(k)+"="+v)
	}
	// 文件参数传的是快照文件的路径
	for _, f := range req.Files {
		env = append(env, "CMDGATE_PARAM_"+strings.ToUpper(f.Name)+"="+filepath.Join(e.st.RequestDir(req.ID), "inputs", f.Name))
	}
	for k, v := range secrets {
		env = append(env, k+"="+v)
	}
	return env
}

// sem 返回一个操作的并发信号量，不限制并发时返回 nil
func (e *Engine) sem(act *action.Action) chan struct{} {
	if act.Concurrency <= 0 {
		return nil
	}
	// 键里带上并发数，操作重新安装并修改上限后会用新的信号量
	key := fmt.Sprintf("%s/%d", act.Name, act.Concurrency)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sems[key] == nil {
		e.sems[key] = make(chan struct{}, act.Concurrency)
	}
	return e.sems[key]
}

// allowRate 判断请求者是否还能发起审批请求，并记下这一次
func (e *Engine) allowRate(name string) bool {
	limit := e.cfg.RateLimitPerMinute
	if limit <= 0 {
		limit = defaultRatePerMinute
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-time.Minute)
	recent := e.rate[name][:0]
	for _, t := range e.rate[name] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= limit {
		e.rate[name] = recent
		return false
	}
	e.rate[name] = append(recent, time.Now())
	return true
}

// snapshot 在锁内复制请求状态，返回只读浅拷贝，调用者不能修改其中的 map 或 slice
func (e *Engine) snapshot(ent *entry) *store.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot := *ent.req
	return &snapshot
}

// setReason 更新请求说明并尝试保存，不改变审批或执行状态
func (e *Engine) setReason(ent *entry, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent.req.Reason = reason
	e.st.SaveRequest(ent.req)
}

// audit 记录请求事件，写入失败时输出服务日志
func (e *Engine) audit(event string, r *store.Request, detail string) {
	err := e.st.Audit(store.Event{
		Event:     event,
		RequestID: r.ID,
		Action:    r.Action,
		Requester: r.Requester,
		Params:    r.Params,
		Files:     r.Files,
		Hash:      r.Hash,
		Approver:  r.Approver,
		State:     r.State,
		ExitCode:  r.ExitCode,
		Detail:    detail,
	})
	if err != nil {
		e.log.Printf("audit: %v", err)
	}
}

// visible 返回调用者是否可以查看这个请求
func visible(r *store.Request, who Identity) bool {
	return who.Admin || r.Requester == who.Name
}

// allowed 返回名字是否在名单里，名单为空表示不限制
func allowed(list []string, name string) bool {
	return len(list) == 0 || contains(list, name)
}

// contains 返回列表中是否包含指定字符串
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// randomHex 返回 n 个随机字节的十六进制表示，随机源失败时终止执行
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
