package e2e

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"cmdgate/internal/store"
	"cmdgate/internal/testutil/faketg"
)

// TestChannelAddPromptsBeforeSetup 验证名称引导、覆盖确认和输入缓冲，不访问真实 Telegram
func TestChannelAddPromptsBeforeSetup(t *testing.T) {
	for _, tc := range []struct {
		name, input, flagName, wantName          string
		existing, success, replace, setupFailure bool
	}{
		{name: "default name", input: "\n", wantName: "telegram", success: true},
		{name: "second bot with typed name", input: "ops\n", wantName: "ops", existing: true, success: true},
		{name: "flag supplies default", input: "\n", flagName: "ops", wantName: "ops", existing: true, success: true},
		{name: "confirm replacement", input: "\nyes\n", wantName: "telegram", existing: true, success: true, replace: true},
		{name: "replacement setup failure", input: "\nyes\ninvalid-token\n", existing: true, setupFailure: true},
		{name: "decline replacement", input: "\nn\n", existing: true},
		{name: "default declines replacement", input: "\n\n", existing: true},
		{name: "EOF at confirmation", input: "\n", existing: true},
		{name: "EOF at name"},
		{name: "flag cannot bypass confirmation", input: "\nn\n", flagName: "telegram", existing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := &store.Store{Root: e.data}
			cfg := &store.Config{Mode: store.ModeDev}
			const oldToken = "old-bot-token"
			if tc.existing {
				cfg.Channels = map[string]store.ChannelConfig{
					"telegram": {
						Type:      "telegram",
						Settings:  map[string]string{"token_secret": "CMDGATE_TELEGRAM_TOKEN"},
						Approvers: map[string]string{"previous": "77"},
					},
				}
			}
			if err := st.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if tc.existing {
				if err := st.SetSecret("CMDGATE_TELEGRAM_TOKEN", oldToken); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(st.Path("config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			input := tc.input
			if tc.success {
				e.respondToTelegramSetup()
				input += faketg.Token + "\ny\nowner\n"
			}
			args := []string{"channel", "add", "telegram"}
			def := "telegram"
			if tc.flagName != "" {
				args = append(args, "--name", tc.flagName)
				def = tc.flagName
			}
			out, code := e.runIn(input, args...)
			mustContain(t, out, "Channel name ["+def+"]")
			if !tc.success {
				if code == 0 || strings.Contains(out, "Create a bot") != tc.setupFailure {
					t.Fatalf("unexpected setup after confirmation (exit %d):\n%s", code, out)
				}
				after, err := os.ReadFile(st.Path("config.yaml"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("configuration changed after cancellation: %v", err)
				}
				if tc.existing {
					token, err := st.GetSecret("CMDGATE_TELEGRAM_TOKEN")
					if err != nil || token != oldToken {
						t.Fatalf("credential changed after cancellation: %v", err)
					}
					mustContain(t, out, "already exists", "Replace")
				}
				return
			}
			if code != 0 {
				t.Fatalf("channel add exited %d:\n%s", code, out)
			}
			if tc.replace {
				mustContain(t, out, "already exists", "Replace")
			} else if strings.Contains(out, "Replace") {
				t.Fatalf("replacement prompt for a new channel: %s", out)
			}
			saved, err := st.LoadConfig()
			if err != nil {
				t.Fatal(err)
			}
			added, ok := saved.Channels[tc.wantName]
			if !ok || added.Type != "telegram" || added.Approvers["owner"] != "42" {
				t.Fatalf("wrong channel saved: %+v", saved.Channels)
			}
			token, err := st.GetSecret(added.Settings["token_secret"])
			if err != nil || token != faketg.Token {
				t.Fatalf("wizard input or credential storage failed: %v", err)
			}
			if tc.existing && !tc.replace {
				token, err := st.GetSecret("CMDGATE_TELEGRAM_TOKEN")
				if err != nil || token != oldToken || saved.Channels["telegram"].Approvers["previous"] != "77" || len(saved.Channels) != 2 {
					t.Fatalf("adding second bot changed the first: %v", err)
				}
			}
			mustContain(t, e.ok("channel", "list"), "NAME", "TYPE", "APPROVERS", tc.wantName, "owner=42")
		})
	}
}
