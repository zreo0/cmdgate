package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cmdgate/internal/channel"
)

// transportFunc 用内存响应验证 Telegram 调用顺序，不访问真实账号
type transportFunc func(*http.Request) (*http.Response, error)

// RoundTrip 将请求交给测试函数并返回响应
func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAttachmentBeforeButtons 附件上传失败时不启用按钮，成功时先交付附件再启用
func TestAttachmentBeforeButtons(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "attachment failure"}[fail], func(t *testing.T) {
			var calls []string
			b := newBot("https://test.invalid", "test-token")
			b.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
				method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				calls = append(calls, method)
				body := `{"ok":true,"result":{"message_id":1}}`
				switch method {
				case "sendMessage":
					var data struct {
						Markup keyboard `json:"reply_markup"`
					}
					if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
						t.Fatal(err)
					}
					if len(data.Markup.Rows) != 0 {
						t.Fatal("buttons enabled before attachment")
					}
				case "sendDocument":
					if fail {
						body = `{"ok":false,"description":"upload failed"}`
					}
				case "editMessageReplyMarkup":
					if fail {
						t.Fatal("buttons enabled after failed upload")
					}
					var data struct {
						Markup keyboard `json:"reply_markup"`
					}
					if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
						t.Fatal(err)
					}
					if len(data.Markup.Rows) != 1 || len(data.Markup.Rows[0]) != 2 {
						t.Fatal("missing approval buttons")
					}
				default:
					t.Fatalf("unexpected method %s", method)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			_, err := b.Send(context.Background(), channel.Request{ID: "id", Nonce: "nonce", Hash: "hash", Params: []channel.KV{{Key: "sql", Value: strings.Repeat("x", 300)}}, Recipients: []string{"1"}, ExpiresAt: time.Now().Add(time.Hour)})
			if (err != nil) != fail {
				t.Fatalf("error=%v", err)
			}
			want := "sendMessage,sendDocument,editMessageReplyMarkup"
			if fail {
				want = "sendMessage,sendDocument"
			}
			if strings.Join(calls, ",") != want {
				t.Fatal(calls)
			}
		})
	}
}
