// Package server 提供 HTTP 接口，并负责识别调用者身份
//
// 本机 unix socket 上的身份取自对端进程的 OS 用户，网络连接上的身份取自令牌
// 管理接口只在 unix socket 上对 root 和服务端自身用户开放
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"cmdgate/internal/api"
	"cmdgate/internal/channel"
	"cmdgate/internal/engine"
	"cmdgate/internal/store"
)

const (
	// 请求体上限，文件参数也在其中
	maxBodyBytes = 16 << 20
	// 单次长轮询的最长等待
	maxWait = 60 * time.Second
)

type ctxKey int

// peerKey 是连接上下文里保存对端 uid 的键，只有 unix socket 连接才有
const peerKey ctxKey = 0

// Server 持有服务端运行所需的全部对象
type Server struct {
	st  *store.Store
	cfg *store.Config
	eng *engine.Engine
	log *log.Logger
}

// Run 启动服务端并阻塞到 ctx 结束
//
// 依次打开审批渠道、恢复未结束的请求、监听 unix socket，server 模式下再监听网络
func Run(ctx context.Context, st *store.Store, logger *log.Logger) error {
	if err := st.CheckVersion(); err != nil {
		return err
	}
	lock, err := st.LockServer()
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := st.LoadConfig()
	if err != nil {
		return err
	}
	channels := map[string]channel.Channel{}
	for name, c := range cfg.Channels {
		driver, err := channel.Lookup(c.Type)
		if err != nil {
			return fmt.Errorf("channel %s: %w", name, err)
		}
		ch, err := driver.Open(name, c, st.GetSecret)
		if err != nil {
			return fmt.Errorf("channel %s: %w", name, err)
		}
		channels[name] = ch
	}

	eng := engine.New(st, cfg, channels, logger)
	s := &Server{st: st, cfg: cfg, eng: eng, log: logger}
	handler := s.routes()

	// 上次异常退出可能留下旧的 socket 文件
	os.Remove(st.Socket())
	unixLn, err := net.Listen("unix", st.Socket())
	if err != nil {
		return err
	}
	defer unixLn.Close()
	// 任何本机用户都可以连接，身份由对端 uid 决定
	if err := os.Chmod(st.Socket(), 0o666); err != nil {
		return err
	}
	unixSrv := &http.Server{
		Handler: handler,
		ConnContext: func(c context.Context, conn net.Conn) context.Context {
			if uc, ok := conn.(*net.UnixConn); ok {
				if uid, err := peerUID(uc); err == nil {
					return context.WithValue(c, peerKey, uid)
				}
			}
			return c
		},
	}

	var netSrv *http.Server
	if cfg.Mode == store.ModeServer {
		if cfg.Listen == "" {
			return errors.New("server mode needs a listen address in config.yaml")
		}
		hasTLS := cfg.TLSCert != "" && cfg.TLSKey != ""
		if !hasTLS && !cfg.Insecure {
			return errors.New("server mode needs tls_cert and tls_key, or insecure: true to serve plain HTTP")
		}
		netSrv = &http.Server{Addr: cfg.Listen, Handler: handler}
	}

	defer func() {
		eng.Stop()
		os.Remove(st.Socket())
	}()
	if err := eng.Start(); err != nil {
		return err
	}
	logger.Printf("%s serving: mode=%s socket=%s actions=%d channels=%d", store.Name, cfg.Mode, st.Socket(), len(eng.List(engine.Identity{Admin: true})), len(channels))
	if cfg.Mode == store.ModeDev {
		logger.Printf("WARNING: dev mode has no isolation, anything running as this user can read secrets and approve requests")
	}

	errc := make(chan error, 2)
	go func() { errc <- unixSrv.Serve(unixLn) }()
	if netSrv != nil {
		go func() {
			if cfg.TLSCert != "" && cfg.TLSKey != "" {
				logger.Printf("listening on https://%s", cfg.Listen)
				errc <- netSrv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
				return
			}
			logger.Printf("WARNING: listening on http://%s without TLS, tokens travel in clear text", cfg.Listen)
			errc <- netSrv.ListenAndServe()
		}()
	}

	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	unixSrv.Shutdown(shutdown)
	if netSrv != nil {
		netSrv.Shutdown(shutdown)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// routes 注册全部接口
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/whoami", s.auth(s.whoami))
	mux.HandleFunc("GET /v1/actions", s.auth(s.listActions))
	mux.HandleFunc("GET /v1/actions/{name}", s.auth(s.describeAction))
	mux.HandleFunc("POST /v1/requests", s.auth(s.submit))
	mux.HandleFunc("GET /v1/requests/{id}", s.auth(s.getRequest))
	mux.HandleFunc("POST /v1/requests/{id}/cancel", s.auth(s.cancel))
	mux.HandleFunc("POST /v1/admin/requests/{id}/decision", s.auth(s.adminDecide))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, who engine.Identity)

// auth 识别调用者身份，识别不出时返回 401
func (s *Server) auth(next handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, ok := s.identify(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next(w, r, who)
	}
}

// identify 从连接或令牌得出调用者身份
func (s *Server) identify(r *http.Request) (engine.Identity, bool) {
	if uid, ok := r.Context().Value(peerKey).(int); ok {
		name := "uid:" + strconv.Itoa(uid)
		if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
			name = u.Username
		}
		return engine.Identity{Name: name, Admin: uid == 0 || uid == os.Getuid()}, true
	}
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		return engine.Identity{}, false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	// 每次都读文件，这样新增和吊销用户不需要重启服务端
	users, err := s.st.LoadUsers()
	if err != nil {
		s.log.Printf("load users: %v", err)
		return engine.Identity{}, false
	}
	for _, u := range users {
		if subtle.ConstantTimeCompare([]byte(u.TokenHash), []byte(hash)) == 1 {
			return engine.Identity{Name: u.Name}, true
		}
	}
	return engine.Identity{}, false
}

// whoami 返回经过认证的调用者身份和服务模式
func (s *Server) whoami(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	writeJSON(w, http.StatusOK, api.WhoAmI{Name: who.Name, Admin: who.Admin, Mode: s.cfg.Mode})
}

// listActions 返回当前身份可见的操作说明
func (s *Server) listActions(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	views := []api.ActionView{}
	for _, a := range s.eng.List(who) {
		views = append(views, api.NewActionView(a))
	}
	writeJSON(w, http.StatusOK, views)
}

// describeAction 返回路径指定的操作说明，权限校验交给引擎
func (s *Server) describeAction(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	a, err := s.eng.Describe(who, r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.NewActionView(a))
}

// submit 解析提交请求并交给引擎校验、持久化与调度
func (s *Server) submit(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	var body api.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	req, err := s.eng.Submit(who, body.Action, body.Params, body.Files)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.view(req))
}

// getRequest 查询请求并按等待参数返回状态与可见输出
func (s *Server) getRequest(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad wait duration")
			return
		}
		wait = min(d, maxWait)
	}
	req, err := s.eng.Wait(r.Context(), r.PathValue("id"), who, wait)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(req))
}

// cancel 取消指定请求，权限和状态校验交给引擎
func (s *Server) cancel(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	id := r.PathValue("id")
	if err := s.eng.Cancel(id, who); err != nil {
		writeErr(w, err)
		return
	}
	// 执行中的请求需要一点时间才能结束，稍等一下让调用者直接看到结果
	req, err := s.eng.Wait(r.Context(), id, who, 10*time.Second)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(req))
}

// adminDecide 校验管理员身份后提交应急批准或拒绝决定
func (s *Server) adminDecide(w http.ResponseWriter, r *http.Request, who engine.Identity) {
	if !who.Admin {
		writeError(w, http.StatusForbidden, "administrator access required")
		return
	}
	var body api.Decision
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	id := r.PathValue("id")
	if err := s.eng.Decide(id, "", body.Approve, engine.Approver{Admin: true, Name: who.Name}); err != nil {
		writeErr(w, err)
		return
	}
	req, err := s.eng.Get(id, who)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(req))
}

// view 把请求记录转成返回给调用者的结构，结束后的请求带上输出
func (s *Server) view(r *store.Request) api.RequestView {
	v := api.RequestView{
		ID:        r.ID,
		Action:    r.Action,
		Params:    r.Params,
		Files:     r.Files,
		Requester: r.Requester,
		Risk:      r.Risk,
		State:     r.State,
		Approver:  r.Approver,
		Reason:    r.Reason,
		Preflight: r.Preflight,
		ExitCode:  r.ExitCode,
		CreatedAt: r.CreatedAt,
		ExpiresAt: r.ExpiresAt,
	}
	if store.Terminal(r.State) {
		v.Output = s.eng.Output(r.ID)
	}
	return v
}

// writeJSON 设置状态码并把响应值编码为 JSON
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError 输出指定状态码的 JSON 错误响应
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, api.Error{Error: msg})
}

// writeErr 按引擎返回的错误类型写出响应
func writeErr(w http.ResponseWriter, err error) {
	var e *engine.Error
	if errors.As(err, &e) {
		writeError(w, e.Status, e.Msg)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
