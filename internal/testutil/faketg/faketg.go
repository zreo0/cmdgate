// Package faketg 是测试用的假 Telegram Bot API
//
// 它实现了渠道用到的那几个接口，并提供模拟用户发消息和点按钮的方法
package faketg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Token 是假服务接受的 bot token
const Token = "123456:TEST-TOKEN"

// Message 是 bot 发出的一条消息
type Message struct {
	ID   int64
	Chat int64
	Text string
	// 每个按钮的回调数据
	Buttons []string
	// 回复的消息 ID，0 表示不是回复
	ReplyTo int64
	// 作为文件发送时的内容
	Document string
	// 按钮是否已被移除
	ButtonsCleared bool
}

// Server 是一个假的 Telegram 服务
type Server struct {
	*httptest.Server

	mu              sync.Mutex
	msgs            []*Message
	updates         []map[string]any
	nextMsg         int64
	nextUpdate      int64
	messagePoll     chan struct{}
	messagePollOnce sync.Once
}

// New 启动假服务，调用者负责 Close
func New() *Server {
	s := &Server{messagePoll: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// handle 处理测试覆盖的 Bot API 请求，不连接真实 Telegram
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + Token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	var result any = true
	switch method {
	case "getMe":
		result = map[string]any{"id": 1, "username": "cmdgate_test_bot", "first_name": "bot"}
	case "sendMessage":
		var body struct {
			Chat   int64  `json:"chat_id"`
			Text   string `json:"text"`
			Markup struct {
				Rows [][]struct {
					Data string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
			Reply struct {
				ID int64 `json:"message_id"`
			} `json:"reply_parameters"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		m := &Message{Chat: body.Chat, Text: body.Text, ReplyTo: body.Reply.ID}
		for _, row := range body.Markup.Rows {
			for _, b := range row {
				m.Buttons = append(m.Buttons, b.Data)
			}
		}
		result = s.add(m)
	case "sendDocument":
		r.ParseMultipartForm(32 << 20)
		m := &Message{}
		json.Unmarshal([]byte(r.FormValue("chat_id")), &m.Chat)
		var reply struct {
			ID int64 `json:"message_id"`
		}
		json.Unmarshal([]byte(r.FormValue("reply_parameters")), &reply)
		m.ReplyTo = reply.ID
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		if f, _, err := r.FormFile("document"); err == nil {
			defer f.Close()
			data, _ := io.ReadAll(f)
			m.Document = string(data)
		}
		result = s.add(m)
	case "editMessageReplyMarkup":
		var body struct {
			ID     int64 `json:"message_id"`
			Markup struct {
				Rows [][]struct {
					Data string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		for _, m := range s.msgs {
			if m.ID == body.ID {
				m.Buttons = nil
				for _, row := range body.Markup.Rows {
					for _, button := range row {
						m.Buttons = append(m.Buttons, button.Data)
					}
				}
				m.ButtonsCleared = len(m.Buttons) == 0
			}
		}
		s.mu.Unlock()
	case "answerCallbackQuery":
	case "getUpdates":
		var body struct {
			Offset  int64    `json:"offset"`
			Allowed []string `json:"allowed_updates"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, kind := range body.Allowed {
			if kind == "message" {
				s.messagePollOnce.Do(func() { close(s.messagePoll) })
			}
		}
		result = s.poll(body.Offset, body.Allowed)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "unknown method " + method})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// add 在锁内保存发送消息并返回对应的 API 结果
func (s *Server) add(m *Message) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMsg++
	m.ID = s.nextMsg
	s.msgs = append(s.msgs, m)
	return map[string]any{"message_id": m.ID, "chat": map[string]any{"id": m.Chat}}
}

// poll 模拟长轮询：丢弃已确认的更新，短暂等待后返回类型匹配的更新
func (s *Server) poll(offset int64, allowed []string) []map[string]any {
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		s.mu.Lock()
		kept := s.updates[:0]
		for _, u := range s.updates {
			if u["update_id"].(int64) >= offset {
				kept = append(kept, u)
			}
		}
		s.updates = kept
		out := []map[string]any{}
		for _, u := range s.updates {
			for _, kind := range allowed {
				if _, ok := u[kind]; ok {
					out = append(out, u)
				}
			}
		}
		s.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// push 把模拟的平台事件加入更新队列并分配递增 ID
func (s *Server) push(kind string, payload map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextUpdate++
	s.updates = append(s.updates, map[string]any{"update_id": s.nextUpdate, kind: payload})
}

// UserMessage 模拟一个用户在私聊里给 bot 发消息
func (s *Server) UserMessage(from int64, text string) {
	s.push("message", map[string]any{
		"message_id": 0,
		"date":       time.Now().Unix(),
		"text":       text,
		"from":       map[string]any{"id": from, "first_name": "Tester"},
		"chat":       map[string]any{"id": from, "type": "private"},
	})
}

// Click 模拟一个用户点击了带有指定回调数据的按钮
func (s *Server) Click(from int64, data string) {
	s.push("callback_query", map[string]any{
		"id":   "cb",
		"from": map[string]any{"id": from, "first_name": "Tester"},
		"data": data,
	})
}

// Messages 返回 bot 到目前为止发出的全部消息
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.msgs))
	for i, m := range s.msgs {
		out[i] = *m
	}
	return out
}

// WaitFor 等待一条满足条件的消息出现，超时返回 false
func (s *Server) WaitFor(timeout time.Duration, match func(Message) bool) (Message, bool) {
	deadline := time.Now().Add(timeout)
	for {
		for _, m := range s.Messages() {
			if match(m) {
				return m, true
			}
		}
		if time.Now().After(deadline) {
			return Message{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// WaitForMessagePoll 等待配置向导开始接收用户消息，超时返回 false
func (s *Server) WaitForMessagePoll(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.messagePoll:
		return true
	case <-timer.C:
		return false
	}
}
