package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/telegram"
)

type resumeFixtureSession struct {
	ID        string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

// The ACP fixture lists only explicitly supplied metadata, never host history.
func runResumeListFixture() {
	var sessions []resumeFixtureSession
	if raw := os.Getenv("OMP_TELEGRAM_FIXTURE_SESSIONS"); raw != "" {
		if json.Unmarshal([]byte(raw), &sessions) != nil {
			os.Exit(2)
		}
	}
	out := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				CWD    string `json:"cwd"`
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"sessionCapabilities": map[string]any{"list": map[string]any{}}}}
		case "session/list":
			matches := make([]resumeFixtureSession, 0)
			for _, s := range sessions {
				if s.CWD == request.Params.CWD {
					matches = append(matches, s)
				}
			}
			start := 0
			if request.Params.Cursor != "" {
				var err error
				start, err = strconv.Atoi(request.Params.Cursor)
				if err != nil || start < 0 || start > len(matches) {
					os.Exit(2)
				}
			}
			end := min(start+3, len(matches))
			page := map[string]any{"sessions": matches[start:end]}
			if end < len(matches) {
				page["nextCursor"] = strconv.Itoa(end)
			}
			result = page
		default:
			// Listing must never create, load, or prompt a session.
			os.Exit(2)
		}
		if out.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}) != nil {
			os.Exit(2)
		}
	}
}

func setResumeFixtures(t *testing.T, cwd string, count int) []resumeFixtureSession {
	t.Helper()
	sessions := make([]resumeFixtureSession, 0, count+1)
	for i := range count {
		s := resumeFixtureSession{ID: fmt.Sprintf("abcd%04x-0000-4000-8000-%012x", i, i), CWD: cwd, Title: fmt.Sprintf("native-choice-%02d", i), UpdatedAt: fmt.Sprintf("2026-09-%02dT12:00:00Z", 20-i)}
		sessions = append(sessions, s)
		data, err := json.Marshal(map[string]string{"cwd": cwd})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), s.ID+".jsonl")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(path) })
	}
	sessions = append(sessions, resumeFixtureSession{ID: "ffff0000-0000-4000-8000-000000000000", CWD: t.TempDir(), Title: "foreign-directory-choice", UpdatedAt: "2026-09-21T12:00:00Z"})
	data, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	return sessions[:count]
}

func finishResumeList(t *testing.T, w *worker) resumeListResult {
	t.Helper()
	select {
	case result := <-w.resumeResults:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("native session listing did not finish")
		return resumeListResult{}
	}
}

func resumeButtons(t *testing.T, f *fakeHTTP) []map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		markup, ok := f.messages[i]["reply_markup"].(map[string]any)
		if !ok {
			continue
		}
		rows, ok := markup["inline_keyboard"].([]any)
		if !ok {
			continue
		}
		var buttons []map[string]any
		for _, row := range rows {
			for _, button := range row.([]any) {
				buttons = append(buttons, button.(map[string]any))
			}
		}
		return buttons
	}
	t.Fatal("session picker keyboard missing")
	return nil
}

func clickResume(w *worker, user int64, data string) {
	message := update(0, 11, "").Message
	message.MessageID = 1
	w.callback(&telegram.CallbackQuery{ID: "picker-callback", From: telegram.User{ID: user}, Message: message, Data: data})
}

func TestResumePickerListsNativeDirectoryAndNavigates(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	original := w.binding
	sessions := setResumeFixtures(t, original.Workspace, 10)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	buttons := resumeButtons(t, f)
	if len(buttons) != 10 {
		t.Fatalf("first page has %d buttons, want eight choices, next, cancel", len(buttons))
	}
	for i := range 8 {
		if !strings.Contains(buttons[i]["text"].(string), sessions[i].Title) {
			t.Fatalf("choice %d lost native ordering or title: %v", i, buttons[i])
		}
	}
	clickResume(w, 7, buttons[8]["callback_data"].(string))
	buttons = resumeButtons(t, f)
	if len(buttons) != 4 || !strings.Contains(buttons[0]["text"].(string), sessions[8].Title) || !strings.Contains(buttons[1]["text"].(string), sessions[9].Title) {
		t.Fatalf("second page does not contain final native choices: %v", buttons)
	}
	clickResume(w, 7, buttons[2]["callback_data"].(string))
	buttons = resumeButtons(t, f)
	if len(buttons) != 10 || !strings.Contains(buttons[0]["text"].(string), sessions[0].Title) {
		t.Fatal("previous did not return to first page")
	}
	if w.binding != original {
		t.Fatal("page navigation changed active session")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, message := range f.messages {
		data, _ := json.Marshal(message)
		if strings.Contains(string(data), "foreign-directory-choice") {
			t.Fatal("picker exposed a session from another directory")
		}
	}
}

func TestResumePickerSelectionRestoresNativeSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before := w.binding
	sessions := setResumeFixtures(t, before.Workspace, 2)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	buttons := resumeButtons(t, f)
	// A nonzero choice must select, not take the ordinary confirmation cancel path.
	token := buttons[1]["callback_data"].(string)
	clickResume(w, 7, token)
	if w.client == nil || w.sessionID != sessions[1].ID || w.binding.Workspace != before.Workspace || w.binding.Generation <= before.Generation {
		t.Fatal("selection failed to restore chosen native identity and working directory")
	}
	if filepath.Base(w.binding.Session) != sessions[1].ID+".jsonl" {
		t.Fatal("selection restored an unrelated native session file")
	}
	restored := w.binding
	clickResume(w, 7, token)
	if w.binding != restored {
		t.Fatal("replayed selection restarted native session")
	}
}

func TestResumePickerCurrentSelectionKeepsProcess(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	before, client := w.binding, w.client
	data, _ := json.Marshal([]resumeFixtureSession{{ID: w.sessionID, CWD: before.Workspace, Title: "current-native-session", UpdatedAt: "2026-09-20T12:00:00Z"}})
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	if w.client != client || w.binding != before {
		t.Fatal("selecting active native session restarted it")
	}
}

func TestResumePickerRejectsInvalidCallbacks(t *testing.T) {
	for _, scenario := range []string{"wrong-user", "expired", "cancel", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new test")
			before, client := w.binding, w.client
			setResumeFixtures(t, before.Workspace, 2)
			command("/resume")
			w.resumeListed(finishResumeList(t, w))
			buttons := resumeButtons(t, f)
			user, index := int64(7), 0
			switch scenario {
			case "wrong-user":
				user = 8
			case "expired":
				for token, c := range w.confirms {
					c.expires = time.Now().Add(-time.Second)
					w.confirms[token] = c
				}
			case "cancel":
				index = len(buttons) - 1
			case "busy":
				w.b.cfg.QueueCapacity = 1
				command("wait")
				w.dispatch()
			}
			token := buttons[index]["callback_data"].(string)
			clickResume(w, user, token)
			if w.binding != before || w.client != client {
				t.Fatal("invalid picker callback switched the active session")
			}
			if scenario == "busy" && !w.busy {
				t.Fatal("picker interrupted the active task")
			}
			if scenario == "cancel" {
				clickResume(w, 7, buttons[0]["callback_data"].(string))
				if w.binding != before || w.client != client {
					t.Fatal("cancelled picker accepted an old selection")
				}
			}
		})
	}
}

func TestResumePickerMissingBindingEmptyListAndBusy(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/resume")
	_, guidanceErr := w.b.db.NextOutput()
	if w.client != nil || len(w.confirms) != 0 || guidanceErr != nil {
		t.Fatal("unbound picker did not provide guidance without allocating a session")
	}
	command("/new test")
	before := w.binding
	setResumeFixtures(t, before.Workspace, 0)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	if len(w.confirms) != 0 || w.binding != before {
		t.Fatal("empty native list created a picker or changed the binding")
	}
	for _, state := range []string{"busy", "compacting", "queued"} {
		w.busy, w.compacting, w.queue = false, false, nil
		switch state {
		case "busy":
			w.busy = true
		case "compacting":
			w.compacting = true
		case "queued":
			w.queue = []queued{{}}
		}
		command("/resume")
		if len(w.confirms) != 0 || w.binding != before {
			t.Fatalf("%s picker request changed current session", state)
		}
		select {
		case <-w.resumeResults:
			t.Fatalf("%s state queried sessions instead of rejecting picker", state)
		default:
		}
	}
	w.busy, w.compacting, w.queue = false, false, nil
}

func TestResumePickerIgnoresStaleListResults(t *testing.T) {
	for _, scenario := range []string{"generation", "reset", "superseded"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			command("/new test")
			setResumeFixtures(t, w.binding.Workspace, 2)
			command("/resume")
			result := finishResumeList(t, w)
			switch scenario {
			case "generation":
				w.binding.Generation++
			case "reset":
				command("/close")
			case "superseded":
				w.cancelResumeList()
				command("/resume")
				_ = finishResumeList(t, w)
			}
			before := f.messageCount()
			w.resumeListed(result)
			if len(w.confirms) != 0 || f.messageCount() != before {
				t.Fatal("stale native listing published an actionable picker")
			}
		})
	}
}

func TestResumePickerRefusesAnotherTopicsActiveSession(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	before, client := w.binding, w.client
	ctx, cancel := context.WithCancel(w.ctx)
	other := &worker{b: w.b, key: target{chat: -10, thread: 22}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)}
	t.Cleanup(func() { other.teardownWorker(true); cancel(); other.background.Wait() })
	other.start(false, before.Workspace, "", false)
	if other.client == nil {
		t.Fatal("other topic did not start a native session")
	}
	otherClient, otherBinding := other.client, other.binding
	data, _ := json.Marshal([]resumeFixtureSession{{ID: other.sessionID, CWD: before.Workspace, Title: "other-topic-native-session", UpdatedAt: "2026-09-20T12:00:00Z"}})
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickResume(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
	if w.client != client || w.binding != before || other.client != otherClient || other.binding != otherBinding {
		t.Fatal("selection of an owned session interrupted a topic or opened it concurrently")
	}
}
