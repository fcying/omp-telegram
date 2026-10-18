package bridge

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestEffortInvalidOutputDoesNotDisableRoleSwitch(t *testing.T) {
	for _, mode := range []string{"missing", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", mode)
			t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "1")
			trace := filepath.Join(t.TempDir(), "rpc.trace")
			t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
			w, f, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			before, client, claim := w.binding, w.client, w.claimedSession
			if client == nil || w.configuredThinking != "" {
				t.Fatal("startup did not retain a usable runtime with unknown configured thinking")
			}
			for _, role := range []struct{ name, model string }{{"slow", "deep"}, {"smol", "quick"}} {
				command("/model")
				var data string
				for _, button := range resumeButtons(t, f) {
					label := strings.TrimPrefix(button["text"].(string), "* ")
					if strings.HasPrefix(label, role.name+" - ") {
						data = button["callback_data"].(string)
						break
					}
				}
				if data == "" {
					t.Fatalf("role %s not available", role.name)
				}
				messageID := f.messageCount()
				clickKeyboard(w, 7, data)
				if w.client != client || client.Stopping() || !sameBindingIdentity(w.binding, before) || w.claimedSession != claim {
					t.Fatal("bad effort output disabled role switching or replaced its runtime")
				}
				assertPickerModel(t, w, "fixture", role.model)
				text := settingsMenuResult(t, f, messageID)
				if !strings.Contains(text, "Model switched to fixture/"+role.model+".") || !strings.Contains(text, "Thinking: n/a") || !strings.Contains(text, "Role: "+role.name) || strings.Contains(text, "PRIVATE") {
					t.Fatalf("role outcome was misreported after invalid effort output: %q", text)
				}
				command("/status")
				var status string
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&status))
				if statusFields(status)["Thinking"] != "n/a" {
					t.Fatalf("invalid configured thinking was displayed: %q", status)
				}
			}
			if count := researchTraceCount(trace, "effort_failure", mode); count != 3 {
				t.Fatalf("expected startup and two optional failures, got %d", count)
			}
		})
	}
}
