// Package telegram 是基于 Telegram bot 的审批渠道
//
// 只使用出站连接：发消息和长轮询都是服务端主动访问 Telegram，不需要开入站端口
package telegram

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cmdgate/internal/channel"
	"cmdgate/internal/store"
)

const (
	defaultAPIBase = "https://api.telegram.org"
	// 测试时用来把向导指向假服务的环境变量
	envAPIBase = "CMDGATE_TELEGRAM_API_BASE"
	// 长轮询的秒数
	pollSeconds = 25
	// 消息正文的字符上限，比 Telegram 的 4096 留出余量
	maxTextRunes = 3500
	// 超过这个长度或包含换行的参数值放进附件
	maxInlineValue = 256
	// 向导等待管理员操作的最长时间
	setupTimeout = 5 * time.Minute
)

// init 向渠道注册表登记 Telegram 实现
func init() {
	channel.Register("telegram", channel.Driver{Setup: setup, Open: open})
}

// bot 是一个 Telegram bot 的访问入口
type bot struct {
	// 形如 https://api.telegram.org/bot<token>
	api  string
	http *http.Client
}

// newBot 创建 Bot API 客户端，apiBase 为空时使用官方地址
func newBot(apiBase, token string) *bot {
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	return &bot{
		api: strings.TrimRight(apiBase, "/") + "/bot" + token,
		// 要比长轮询的时间长
		http: &http.Client{Timeout: (pollSeconds + 15) * time.Second},
	}
}

// open 按配置创建渠道实例
func open(name string, cfg store.ChannelConfig, secret func(string) (string, error)) (channel.Channel, error) {
	token, err := secret(cfg.Settings["token_secret"])
	if err != nil {
		return nil, err
	}
	return newBot(cfg.Settings["api_base"], token), nil
}

// apiResponse 是 Telegram 接口的统一响应外壳
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	From      tgUser `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
}

type tgCallback struct {
	ID   string `json:"id"`
	From tgUser `json:"from"`
	Data string `json:"data"`
}

type tgUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

type button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}

type keyboard struct {
	Rows [][]button `json:"inline_keyboard"`
}

// call 调用一个 Telegram 接口
//
// 参数 payload 会编码成 JSON 请求体，out 不为 nil 时接收 result 字段
func (b *bot) call(ctx context.Context, method string, payload, out any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", b.api+"/"+method, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return b.send(req, method, out)
}

// send 发送 API 请求并解析结果，返回不包含 bot token 的错误
func (b *bot) send(req *http.Request, method string, out any) error {
	resp, err := b.http.Do(req)
	if err != nil {
		// 错误信息里的地址带有 bot token，不能原样返回
		var ne interface{ Timeout() bool }
		if errors.As(err, &ne) && ne.Timeout() {
			return fmt.Errorf("telegram %s: timed out", method)
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	var r apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: bad response (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// sendDocument 把一段文本作为文件发出，并回复到指定消息下
func (b *bot) sendDocument(ctx context.Context, chat, replyTo int64, filename string, content []byte) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("chat_id", strconv.FormatInt(chat, 10))
	w.WriteField("reply_parameters", fmt.Sprintf(`{"message_id":%d}`, replyTo))
	part, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	part.Write(content)
	w.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", b.api+"/sendDocument", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return b.send(req, "sendDocument", nil)
}

// Send 给每个接收人发一条带批准和拒绝按钮的消息
//
// 按钮的回调数据里带着请求 ID 和一次性随机数，核心靠它们把决定绑定到具体请求
func (b *bot) Send(ctx context.Context, req channel.Request) ([]store.Handle, error) {
	text, attachment := render(req)
	markup := keyboard{Rows: [][]button{{
		{Text: "Approve", Data: "a:" + req.ID + ":" + req.Nonce},
		{Text: "Deny", Data: "d:" + req.ID + ":" + req.Nonce},
	}}}
	var handles []store.Handle
	var errs []error
	for _, r := range req.Recipients {
		chat, err := strconv.ParseInt(r, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("bad telegram user id %q", r))
			continue
		}
		var msg tgMessage
		// 附件完整交付前不显示可操作的按钮
		initial := markup
		if attachment != nil {
			initial = keyboard{Rows: [][]button{}}
		}
		err = b.call(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text, "reply_markup": initial}, &msg)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		handles = append(handles, store.Handle{Chat: r, Message: strconv.FormatInt(msg.MessageID, 10)})
		if attachment != nil {
			if err := b.sendDocument(ctx, chat, msg.MessageID, "request-"+req.ID+".txt", attachment); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := b.call(ctx, "editMessageReplyMarkup", map[string]any{
				"chat_id": chat, "message_id": msg.MessageID, "reply_markup": markup,
			}, nil); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return handles, errors.Join(errs...)
}

// render 把审批请求渲染成消息正文和可选的附件
//
// 参数值永远不会被悄悄截断：过长或多行的值在正文里显示长度和 hash，完整内容放进附件
// 正文仍然超长时，preflight 输出会被明确标注地截短，再不行就把全部参数移到附件
func render(req channel.Request) (string, []byte) {
	needAttachment := len(req.Files) > 0
	var params []string
	for _, kv := range req.Params {
		if len(kv.Value) > maxInlineValue || strings.ContainsAny(kv.Value, "\r\n") {
			needAttachment = true
			params = append(params, fmt.Sprintf("  %s: (%d bytes, sha256 %s, full value attached)", kv.Key, len(kv.Value), shortHash([]byte(kv.Value))))
			continue
		}
		params = append(params, fmt.Sprintf("  %s = %s", kv.Key, kv.Value))
	}
	build := func(params []string, preflight string) string {
		var sb strings.Builder
		sb.WriteString("cmdgate approval request\n\n")
		fmt.Fprintf(&sb, "Action: %s\n", req.Action)
		if req.Description != "" {
			fmt.Fprintf(&sb, "  %s\n", req.Description)
		}
		fmt.Fprintf(&sb, "Requester: %s\n", req.Requester)
		if len(params) > 0 {
			sb.WriteString("Parameters:\n" + strings.Join(params, "\n") + "\n")
		}
		if len(req.Files) > 0 {
			sb.WriteString("Files (content attached):\n")
			for _, f := range req.Files {
				fmt.Fprintf(&sb, "  %s: %d bytes, sha256 %s\n", f.Name, f.Size, f.SHA256[:16])
			}
		}
		fmt.Fprintf(&sb, "Script hash: %s\n", req.Hash[:min(16, len(req.Hash))])
		if preflight != "" {
			sb.WriteString("Preflight output:\n" + strings.TrimRight(preflight, "\n") + "\n")
		}
		fmt.Fprintf(&sb, "Expires: %s\n", req.ExpiresAt.Local().Format("2006-01-02 15:04 MST"))
		fmt.Fprintf(&sb, "Request: %s", req.ID)
		return sb.String()
	}
	text := build(params, req.Preflight)
	if utf8.RuneCountInString(text) > maxTextRunes {
		needAttachment = true
		text = build(params, "(too long to show here, see the attachment)")
	}
	if utf8.RuneCountInString(text) > maxTextRunes {
		text = build([]string{fmt.Sprintf("  %d values, see the attachment", len(req.Params))}, "(see the attachment)")
	}
	if !needAttachment {
		return text, nil
	}
	return text, renderAttachment(req)
}

// renderAttachment 生成包含全部参数、文件内容和 preflight 输出的完整文本
func renderAttachment(req channel.Request) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "cmdgate request %s\naction: %s\nrequester: %s\nscript hash: %s\n", req.ID, req.Action, req.Requester, req.Hash)
	for _, kv := range req.Params {
		fmt.Fprintf(&sb, "\n===== param %s (%d bytes, sha256 %s) =====\n%s\n", kv.Key, len(kv.Value), shortHash([]byte(kv.Value)), kv.Value)
	}
	for _, f := range req.Files {
		fmt.Fprintf(&sb, "\n===== file %s (%d bytes, sha256 %s) =====\n", f.Name, f.Size, f.SHA256)
		if utf8.Valid(f.Content) {
			sb.Write(f.Content)
			sb.WriteString("\n")
		} else {
			sb.WriteString("(binary content not shown)\n")
		}
	}
	if req.Preflight != "" {
		fmt.Fprintf(&sb, "\n===== preflight output =====\n%s\n", req.Preflight)
	}
	return []byte(sb.String())
}

// shortHash 返回内容的 SHA-256 短摘要，供审批消息展示
func shortHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// Decisions 用长轮询接收按钮点击，转成决定后送入返回的通道
//
// 用户身份取自 Telegram 返回的 from.id，回调数据格式不对的更新直接丢弃
func (b *bot) Decisions(ctx context.Context) (<-chan channel.Decision, error) {
	out := make(chan channel.Decision)
	go func() {
		defer close(out)
		var offset int64
		for ctx.Err() == nil {
			var updates []tgUpdate
			err := b.call(ctx, "getUpdates", map[string]any{
				"offset": offset, "timeout": pollSeconds, "allowed_updates": []string{"callback_query"},
			}, &updates)
			if err != nil {
				// 网络故障时稍等再试，期间收不到任何决定，请求不会被执行
				select {
				case <-ctx.Done():
				case <-time.After(3 * time.Second):
				}
				continue
			}
			for _, u := range updates {
				offset = u.UpdateID + 1
				if u.Callback == nil {
					continue
				}
				b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.Callback.ID, "text": "Received"}, nil)
				parts := strings.Split(u.Callback.Data, ":")
				if len(parts) != 3 || (parts[0] != "a" && parts[0] != "d") {
					continue
				}
				d := channel.Decision{
					RequestID:  parts[1],
					Nonce:      parts[2],
					Approve:    parts[0] == "a",
					ApproverID: strconv.FormatInt(u.Callback.From.ID, 10),
				}
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Update 移除审批消息上的按钮，并在它下面回复最新状态
func (b *bot) Update(ctx context.Context, h store.Handle, status string) error {
	chat, err1 := strconv.ParseInt(h.Chat, 10, 64)
	msg, err2 := strconv.ParseInt(h.Message, 10, 64)
	if err1 != nil || err2 != nil {
		return fmt.Errorf("bad message handle %v", h)
	}
	// 按钮可能已经被移除过，这一步失败不影响后面的状态回复
	b.call(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id": chat, "message_id": msg, "reply_markup": keyboard{Rows: [][]button{}},
	}, nil)
	return b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chat, "text": status, "reply_parameters": map[string]any{"message_id": msg},
	}, nil)
}

// Ping 检查 bot token 是否有效、Telegram 是否可达
func (b *bot) Ping(ctx context.Context) error {
	return b.call(ctx, "getMe", map[string]any{}, nil)
}

// setup 是交互式配置向导
//
// 依次完成：验证 bot token、让管理员给 bot 发一条消息以确定审批人账号、
// 发一条测试审批并等待点击，全部通过才返回配置
func setup(ctx context.Context, name string, p channel.Prompter) (*channel.SetupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()

	p.Say("Create a bot with @BotFather in Telegram, then paste its token here.")
	token, err := p.AskSecret("Bot token: ")
	if err != nil {
		return nil, err
	}
	token = strings.TrimSpace(token)
	apiBase := os.Getenv(envAPIBase)
	b := newBot(apiBase, token)
	var me tgUser
	if err := b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return nil, fmt.Errorf("the token was not accepted: %w", err)
	}

	p.Say("Token accepted. Now open Telegram and send any message to @%s.", me.Username)
	p.Say("Waiting for your message...")
	started := time.Now().Unix()
	var from tgUser
	var offset int64
	for from.ID == 0 {
		updates, err := b.poll(ctx, &offset, "message")
		if err != nil {
			return nil, err
		}
		for _, u := range updates {
			// 只接受向导开始之后发来的私聊消息，避免把旧消息的发送者当成审批人
			if u.Message != nil && u.Message.Chat.Type == "private" && u.Message.Date >= started-5 {
				from = u.Message.From
			}
		}
	}
	who := from.FirstName
	if from.Username != "" {
		who += " (@" + from.Username + ")"
	}
	answer, err := p.Ask(fmt.Sprintf("Got a message from %s, id %d. Use this account as an approver? [Y/n]", who, from.ID), "y")
	if err != nil {
		return nil, err
	}
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return nil, errors.New("aborted")
	}
	alias, err := p.Ask("Name for this approver, used in action.yaml and the audit log", "owner")
	if err != nil {
		return nil, err
	}

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	var msg tgMessage
	err = b.call(ctx, "sendMessage", map[string]any{
		"chat_id":      from.ID,
		"text":         "cmdgate test message\n\nPress the button to confirm that approvals reach you.",
		"reply_markup": keyboard{Rows: [][]button{{{Text: "Confirm", Data: "t:" + nonce}}}},
	}, &msg)
	if err != nil {
		return nil, err
	}
	p.Say("A test message was sent to you. Press its button in Telegram...")
	for confirmed := false; !confirmed; {
		updates, err := b.poll(ctx, &offset, "callback_query")
		if err != nil {
			return nil, err
		}
		for _, u := range updates {
			if u.Callback != nil && u.Callback.From.ID == from.ID && u.Callback.Data == "t:"+nonce {
				b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": u.Callback.ID, "text": "Confirmed"}, nil)
				confirmed = true
			}
		}
	}
	h := store.Handle{Chat: strconv.FormatInt(from.ID, 10), Message: strconv.FormatInt(msg.MessageID, 10)}
	b.Update(ctx, h, "Confirmed. This chat will receive cmdgate approval requests.")

	secretName := "CMDGATE_TELEGRAM_TOKEN"
	if name != "telegram" {
		secretName += "_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
	}
	settings := map[string]string{"token_secret": secretName}
	if apiBase != "" {
		settings["api_base"] = apiBase
	}
	return &channel.SetupResult{
		Config: store.ChannelConfig{
			Type:      "telegram",
			Settings:  settings,
			Approvers: map[string]string{alias: strconv.FormatInt(from.ID, 10)},
		},
		Secrets: map[string]string{secretName: token},
	}, nil
}

// poll 做一次长轮询并推进 offset，供向导使用
//
// 参数 kind 是要接收的更新类型
func (b *bot) poll(ctx context.Context, offset *int64, kind string) ([]tgUpdate, error) {
	var updates []tgUpdate
	err := b.call(ctx, "getUpdates", map[string]any{
		"offset": *offset, "timeout": pollSeconds, "allowed_updates": []string{kind},
	}, &updates)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("timed out waiting for Telegram")
		}
		return nil, err
	}
	for _, u := range updates {
		*offset = u.UpdateID + 1
	}
	return updates, nil
}
