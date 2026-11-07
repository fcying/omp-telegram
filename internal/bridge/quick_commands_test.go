package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
)

func dispatchTestShortcut(t *testing.T, w *worker, id int64, command string) {
	t.Helper()
	w.b.cfg.AllowedUsers = []int64{7}
	w.b.cfg.AllowedChats = []int64{w.key.chat}
	query := &telegram.CallbackQuery{
		ID: fmt.Sprint(id), From: telegram.User{ID: 7}, Data: "claimed-shortcut",
		Message: &telegram.Message{MessageID: 501, Chat: telegram.Chat{ID: w.key.chat}, MessageThreadID: w.key.thread, From: &telegram.User{ID: w.b.bot.ID, IsBot: true}},
	}
	raw, err := json.Marshal(telegram.Update{UpdateID: id, CallbackQuery: query})
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(id, raw))
	w.dispatchShortcut(incoming{id: id, callback: query}, command)
	w.dispatch()
}

func TestShortcutUnknownNativeNeverBecomesPromptOrSteer(t *testing.T) {
	for _, mode := range []string{"enabled", "hold"} {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/running=%t", mode, running), func(t *testing.T) {
				w, _, trace := nativeWorker(t, mode)
				if mode != "hold" {
					nativeCatalogReady(t, w)
				}
				if running {
					nativeInput(t, w, 100, "wait", false)
					nativeUntil(t, w, func() bool { return w.taskRunning() && !w.rpcOperationActive })
				}
				dispatchTestShortcut(t, w, 200, "/notadvertised dangerous")
				if mode == "hold" {
					if queueState(t, w, 200) != "pending" {
						t.Fatal("shortcut did not wait for authoritative command discovery")
					}
					nativeRPC(t, w, "fixture_catalog_update", map[string]any{"releaseQuery": true})
				}
				nativeUntil(t, w, func() bool { return queueState(t, w, 200) == "cancelled" })
				for _, entry := range fixtureRPCTrace(trace) {
					if (entry.kind == "prompt" || entry.kind == "steer") && strings.Contains(entry.payload, "notadvertised") {
						t.Fatalf("unknown configured command became model input: %s %s", entry.kind, entry.payload)
					}
				}
				if running && (w.active != 100 || queueState(t, w, 100) != "submitted") {
					t.Fatal("rejecting a shortcut disturbed the active root")
				}
			})
		}
	}
}

func TestShortcutNativeRetainsQueueOrderAndLiteralArguments(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	nativeInput(t, w, 100, "wait", false)
	nativeUntil(t, w, func() bool { return w.taskRunning() && !w.rpcOperationActive })
	nativeInput(t, w, 101, "/followup queued first", false)
	dispatchTestShortcut(t, w, 200, "/native_local@fixture_bot a  b")
	if queueState(t, w, 200) != "pending" || len(w.steers) != 0 {
		t.Fatal("configured native command bypassed the queue or became a steer")
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 200) == "done" })
	var prompts []string
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload != "/session info" {
			prompts = append(prompts, entry.payload)
		}
	}
	if strings.Join(prompts, "|") != "wait|queued first|/native_local a  b" {
		t.Fatalf("shortcut changed command queue semantics or literal arguments: %q", prompts)
	}
}
