package bridge

import (
	"net/http"
	"strings"
	"testing"

	"omp-telegram/internal/i18n"
)

func TestTitleWithoutSessionPreservesExactTitle(t *testing.T) {
	w, fake, command := setupWorkspaceWorker(t)
	w.b.bot.Username = "fixture_bot"
	title := "Trading research 😀"
	requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 22, title))
	command("/title@fixture_bot " + title)
	finishTopicRename(t, w)
	if name, _ := recordedTopicName(t, w.b.db, -10, 11); name != title {
		t.Fatalf("explicit title changed to %q", name)
	}
	fake.mu.Lock()
	edits := append([]map[string]any(nil), fake.forumTopicEdits...)
	fake.mu.Unlock()
	if len(edits) != 1 || edits[0]["chat_id"] != float64(-10) || edits[0]["message_thread_id"] != float64(11) || edits[0]["name"] != title {
		t.Fatalf("topic edits = %+v", edits)
	}
	if w.client != nil || w.binding.Running || w.taskActive() || len(w.queue) != 0 {
		t.Fatal("Telegram-only command started or changed an OMP session")
	}
	if !strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.TopicTitleRenameAccepted)) {
		t.Fatal("API acceptance and client-sync limitation were not reported")
	}
	command("/title")
	w.key.thread = 0
	command("/title ordinary chat")
	if w.topicRenamePending || !strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.UsageTitleTopicTitle)) || !strings.Contains(nativeOutputs(t, w), w.tr(i18n.Msg.TopicTitleRequiresTopic)) {
		t.Fatal("missing title or ordinary chat was not rejected locally")
	}
}

func TestTitleFailureDoesNotConfirmTopicName(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		uncertain  bool
	}{
		{name: "permission denied", status: 403, body: `{"ok":false,"error_code":403,"description":"SECRET_PERMISSION"}`},
		{name: "invalid title", status: 400, body: `{"ok":false,"error_code":400,"description":"SECRET_INVALID_TITLE"}`},
		{name: "unconfirmed", status: 200, body: `{`, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, fake, command := setupWorkspaceWorker(t)
			requireStoreOK(t, w.b.db.ObserveTopicName(99, -10, 11, "previous"))
			fake.forumTopicEditResponse = &fakeHTTPResponse{status: tc.status, body: tc.body}
			command("/title Trading research")
			result := finishTopicRename(t, w)
			if result.err == nil || result.persistenceFailed {
				t.Fatalf("unexpected rename result: %+v", result)
			}
			if name, _ := recordedTopicName(t, w.b.db, -10, 11); name != "previous" {
				t.Fatalf("unconfirmed rename replaced known title: %q", name)
			}
			probe, err := w.b.db.ReserveTopicName(99, -10, 22, "Trading research")
			requireStoreOK(t, err)
			want := "Trading research"
			message := i18n.Msg.TopicTitleRenameFailed
			if tc.uncertain {
				want += " [22]"
				message = i18n.Msg.TopicTitleRenameUnconfirmed
			}
			output := nativeOutputs(t, w)
			if probe.Name != want || !strings.Contains(output, w.tr(message)) || strings.Contains(output, w.tr(i18n.Msg.TopicTitleRenameAccepted)) || strings.Contains(output, "SECRET_") {
				t.Fatalf("incorrect failure settlement or notice: claim=%q output=%q", probe.Name, output)
			}
		})
	}
}

func TestTitleDuringTaskPreservesRootAndQueue(t *testing.T) {
	w, _, trace := nativeWorker(t, "fast")
	nativeCatalogReady(t, w)
	finishTopicRename(t, w)
	nativeInput(t, w, 100, "wait", false)
	researchBarrier(t, w)
	nativeInput(t, w, 101, "/followup queued after root", false)
	client, binding, root, turn, owner := w.client, w.binding, w.rootRequestID, w.turn, w.owner
	// Hold the HTTP call while the actor continues serving commands.
	fake := http.DefaultTransport.(*fakeHTTP)
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	fake.mu.Lock()
	fake.forumTopicEditGate, fake.forumTopicEditStarted = gate, started
	fake.mu.Unlock()
	nativeInput(t, w, 102, "/title Trading research", false)
	waitTopicRenameStarted(t, started)
	nativeInput(t, w, 103, "/title another title", false)
	nativeInput(t, w, 104, "/status", false)
	researchBarrier(t, w)
	if w.client != client || !sameBindingIdentity(w.binding, binding) || w.rootRequestID != root || w.turn != turn || w.owner != owner || w.active != 100 || !w.busy || len(w.queue) != 1 || queueState(t, w, 100) != "submitted" || queueState(t, w, 101) != "pending" {
		t.Fatal("pending title request changed the running root or dispatched the followup")
	}
	close(gate)
	finishTopicRename(t, w)
	fake.mu.Lock()
	editCount := len(fake.forumTopicEdits)
	fake.mu.Unlock()
	if editCount != 2 {
		t.Fatalf("overlapping title command reached Telegram: %d edits including auto-name", editCount)
	}
	nativeRPC(t, w, "fixture_finish_root", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && queueState(t, w, 101) == "done" })
	if researchBody(t, w, 100) != "root completed" || researchTraceCount(trace, "prompt", "wait") != 1 || researchTraceCount(trace, "prompt", "queued after root") != 1 || researchTraceCount(trace, "prompt", "/title Trading research") != 0 {
		t.Fatal("Telegram-only control changed model work or the final reply")
	}
}

func TestTitleBeforeFirstNewSurvivesWorkerReplacement(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		name := "same worker"
		if rebuild {
			name = "rebuilt worker"
		}
		t.Run(name, func(t *testing.T) {
			w, fake, command := setupWorkspaceWorker(t)
			command("/title Trading research")
			finishTopicRename(t, w)
			if rebuild {
				w = newArgsTestPeer(t, w, 11)
			}
			nativeInput(t, w, 100, "/new "+t.TempDir(), false)
			fake.mu.Lock()
			edits := len(fake.forumTopicEdits)
			fake.mu.Unlock()
			if w.client == nil || !w.binding.Running {
				t.Fatal("first /new did not start the OMP session")
			}
			if w.topicRenamePending || edits != 1 {
				t.Fatalf("first /new attempted to overwrite explicit title: pending=%t edits=%d", w.topicRenamePending, edits)
			}
			if title, _ := recordedTopicName(t, w.b.db, -10, 11); title != "Trading research" {
				t.Fatalf("first /new replaced the explicit title: %q", title)
			}
		})
	}
}
