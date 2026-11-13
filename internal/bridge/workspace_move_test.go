package bridge

import (
	"bytes"
	"encoding/json"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/telegram"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertMovedSessionFile(t *testing.T, oldSession, target string) string {
	t.Helper()
	if _, err := os.Stat(oldSession); !os.IsNotExist(err) {
		t.Fatalf("old session file still exists after native move: %v", err)
	}
	movedSession := filepath.Join(filepath.Dir(oldSession), "moved", filepath.Base(oldSession))
	data, err := os.ReadFile(movedSession)
	if err != nil {
		t.Fatalf("read moved session file: %v", err)
	}
	var header struct {
		CWD string `json:"cwd"`
	}
	line := bytes.SplitN(data, []byte{'\n'}, 2)[0]
	if err := json.Unmarshal(line, &header); err != nil {
		t.Fatalf("decode moved session header: %v", err)
	}
	if header.CWD != target {
		t.Fatalf("moved session CWD = %q, want %q", header.CWD, target)
	}
	return movedSession
}

func TestWorkspaceMoveRehomesNativeSessionAndTopicBinding(t *testing.T) {
	w, _, trace := nativeWorker(t, "enabled")
	nativeCatalogReady(t, w)
	before := w.binding
	client := w.client
	target := filepath.Join(t.TempDir(), "workspace with spaces")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(before.Workspace, "preserved.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	requireStoreOK(t, w.b.db.SetPinnedSession(before.Bot, before.Chat, before.Thread, before.Workspace, before.SessionID, true))

	nativeInput(t, w, 100, "/move "+target, false)
	nativeUntil(t, w, func() bool { return queueState(t, w, 100) == "done" && !w.controlInProgress() })

	saved, err := w.b.db.Binding(before.Bot, before.Chat, before.Thread)
	requireStoreOK(t, err)
	if w.client != client || w.sessionID != before.SessionID || saved.SessionID != before.SessionID ||
		w.binding.Workspace != target || saved.Workspace != target || w.binding.Session != saved.Session ||
		w.binding.Generation != before.Generation+1 || saved.Generation != w.binding.Generation {
		t.Fatalf("move did not preserve native session and update topic binding: memory=%+v saved=%+v", w.binding, saved)
	}
	movedSession := assertMovedSessionFile(t, before.Session, target)
	if saved.Session != movedSession {
		t.Fatalf("moved binding session = %q, fixture moved session = %q", saved.Session, movedSession)
	}
	if markerData, err := os.ReadFile(marker); err != nil || string(markerData) != "keep" {
		t.Fatalf("project files changed during session move: %q, %v", markerData, err)
	}
	oldPins, err := w.b.db.PinnedSessions(before.Bot, before.Chat, before.Thread, before.Workspace)
	requireStoreOK(t, err)
	newPins, err := w.b.db.PinnedSessions(before.Bot, before.Chat, before.Thread, target)
	requireStoreOK(t, err)
	if len(oldPins) != 0 || len(newPins) != 1 {
		t.Fatalf("move favorites old=%v new=%v", oldPins, newPins)
	}
	info, err := client.SessionInfo(w.ctx)
	if err != nil || info.ID != before.SessionID || info.CWD != target || info.File != saved.Session {
		t.Fatalf("OMP runtime diverged from moved binding: info=%+v err=%v", info, err)
	}
	if w.taskActive() || len(w.queue) != 0 || w.controlInProgress() || !strings.Contains(nativeOutputs(t, w), "Topic workspace updated to "+target) {
		t.Fatal("move did not finish as a local session operation")
	}
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "/move "+target {
			return
		}
	}
	t.Fatal("native /move was not submitted exactly through the OMP prompt path")
}

func TestWorkspaceMoveRequiresIdleAndRejectsSharedSession(t *testing.T) {
	t.Run("active-task", func(t *testing.T) {
		w, _, trace := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		fastRoot(t, w)
		before, client := w.binding, w.client
		target := t.TempDir()
		nativeInput(t, w, 110, "/move "+target, false)
		if queueState(t, w, 110) != "cancelled" || w.client != client || w.binding != before || !w.taskActive() {
			t.Fatal("move interrupted or changed an active session")
		}
		if !strings.Contains(nativeOutputs(t, w), "Workspace move requires an idle topic") {
			t.Fatal("busy move did not explain that the topic must be idle")
		}
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "prompt" && entry.payload == "/move "+target {
				t.Fatal("move was submitted while a task was active")
			}
		}
	})

	t.Run("shared-session", func(t *testing.T) {
		w, _, trace := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		before := w.binding
		other := before
		other.Thread = 12
		other.Workspace = t.TempDir()
		other.Generation = 1
		other.Running = false
		requireStoreOK(t, w.b.db.Save(other))
		target := t.TempDir()
		nativeInput(t, w, 120, "/move "+target, false)
		if queueState(t, w, 120) != "cancelled" || w.binding != before {
			t.Fatal("move changed a session shared by another topic")
		}
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "prompt" && entry.payload == "/move "+target {
				t.Fatal("shared-session move reached OMP")
			}
		}
		if !strings.Contains(nativeOutputs(t, w), "bound to another topic") {
			t.Fatal("shared-session refusal was not explained")
		}
	})
}

func TestWorkspaceMoveRejectsInputsUntilCompletion(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	w.workspaceMove = &workspaceMoveRequest{phase: "catalog"}
	w.beginControlOperation(controlNative)
	inputs := []struct {
		id         int64
		text       string
		attachment bool
	}{
		{id: 100, text: "ordinary task"},
		{id: 101, attachment: true},
	}
	for _, input := range inputs {
		u := update(input.id, 11, input.text)
		if input.attachment {
			u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", FileSize: 1}}
		}
		raw, err := json.Marshal(u)
		requireStoreOK(t, err)
		requireStoreOK(t, w.b.db.Accept(input.id, raw))
		w.handle(incoming{id: input.id, msg: u.Message})
		if queueState(t, w, input.id) != "cancelled" || len(w.queue) != 0 || w.client != nil {
			t.Fatalf("input %d entered the queue or started media/runtime work: queue=%+v client=%v", input.id, w.queue, w.client)
		}
	}
	stop := update(102, 11, "/stop")
	raw, err := json.Marshal(stop)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(102, raw))
	w.handle(incoming{id: 102, msg: stop.Message})
	if queueState(t, w, 102) != "done" || w.workspaceMove != nil || w.controlInProgress() {
		t.Fatal("/stop was blocked instead of cancelling the pending move lookup")
	}
}

func TestWorkspaceMoveAnswersCallbacksWhilePending(t *testing.T) {
	w, fake, _ := setupWorkspaceWorker(t)
	w.workspaceMove = &workspaceMoveRequest{phase: "prompt", submitted: true}
	query := &telegram.CallbackQuery{ID: "move-pending", From: telegram.User{ID: 7}, Data: "old-menu"}
	u := telegram.Update{UpdateID: 103, CallbackQuery: query}
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(103, raw))
	w.handle(incoming{id: 103, callback: query})
	if queueState(t, w, 103) != "cancelled" {
		t.Fatal("Move callback inbox was not cancelled")
	}
	fake.mu.Lock()
	answered := len(fake.callbacks) == 1 && fake.callbacks[0] != ""
	fake.mu.Unlock()
	if !answered {
		t.Fatal("Move rejection left the Telegram callback unanswered")
	}
}

func TestWorkspaceMoveStopExplainsSubmittedMoveCannotBeStopped(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	request := &workspaceMoveRequest{phase: "prompt", submitted: true}
	w.workspaceMove = request
	w.beginControlOperation(controlNative)
	u := update(104, 11, "/stop")
	raw, err := json.Marshal(u)
	requireStoreOK(t, err)
	requireStoreOK(t, w.b.db.Accept(104, raw))
	w.handle(incoming{id: 104, msg: u.Message})
	if queueState(t, w, 104) != "done" || w.workspaceMove != request || w.controlOp != controlNative {
		t.Fatal("/stop interrupted or discarded the submitted Move")
	}
	if !strings.Contains(nativeOutputs(t, w), "cannot be stopped safely") {
		t.Fatal("/stop did not explain that the submitted Move cannot be stopped safely")
	}
}

func TestWorkspaceMoveRejectsNativeFailureAndDoesNotReplayUncertainResult(t *testing.T) {
	t.Run("native-rejection", func(t *testing.T) {
		t.Setenv("OMP_TELEGRAM_FIXTURE_MOVE_MODE", "reject")
		w, _, _ := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		before, client := w.binding, w.client
		nativeInput(t, w, 130, "/move "+t.TempDir(), false)
		nativeUntil(t, w, func() bool { return queueState(t, w, 130) == "done" && !w.controlInProgress() })
		if w.client != client || w.binding != before {
			t.Fatal("rejected native move changed the bridge binding or runtime")
		}
		if !strings.Contains(nativeOutputs(t, w), "Move rejected by fixture") {
			t.Fatal("native move rejection output was lost")
		}
	})
	t.Run("rpc-rejected-after-side-effect", func(t *testing.T) {
		t.Setenv("OMP_TELEGRAM_FIXTURE_MOVE_MODE", "reject-after-side-effect")
		w, _, trace := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		before := w.binding
		target := t.TempDir()
		nativeInput(t, w, 132, "/move "+target, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, 132) == "uncertain" && !w.controlInProgress() })
		if w.client != nil || w.binding != before {
			t.Fatal("rejected RPC after a successful native move retained the runtime or changed the old binding")
		}
		assertMovedSessionFile(t, before.Session, target)
		moves := 0
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "prompt" && entry.payload == "/move "+target {
				moves++
			}
		}
		if moves != 1 {
			t.Fatalf("native move submission count = %d, want exactly one", moves)
		}
	})

	t.Run("post-ack-session-info-rejection", func(t *testing.T) {
		t.Setenv("OMP_TELEGRAM_FIXTURE_MOVE_MODE", "session-info-reject")
		w, _, trace := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		before := w.binding
		target := t.TempDir()
		nativeInput(t, w, 135, "/move "+target, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, 135) == "uncertain" && !w.controlInProgress() })
		if w.client != nil || w.binding != before {
			t.Fatal("failed post-move verification retained the runtime or changed the old binding")
		}
		assertMovedSessionFile(t, before.Session, target)
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "prompt" && entry.payload == "/move "+target {
				return
			}
		}
		t.Fatal("native move did not receive its local success acknowledgment")
	})

	t.Run("unconfirmed", func(t *testing.T) {
		t.Setenv("OMP_TELEGRAM_FIXTURE_MOVE_MODE", "agent-invoked")
		w, _, trace := nativeWorker(t, "enabled")
		nativeCatalogReady(t, w)
		before := w.binding
		target := t.TempDir()
		nativeInput(t, w, 140, "/move "+target, false)
		nativeUntil(t, w, func() bool { return queueState(t, w, 140) == "uncertain" && !w.controlInProgress() })
		if w.client != nil || w.binding != before {
			t.Fatal("unconfirmed native move reused the runtime or changed its binding")
		}
		for _, entry := range fixtureRPCTrace(trace) {
			if entry.kind == "prompt" && entry.payload == "/move "+target {
				return
			}
		}
		t.Fatal("native move submission was not recorded")
	})
}

func TestWorkspaceMoveUsesReaderPublishedCatalog(t *testing.T) {
	w, _, trace := nativeWorker(t, "hold")
	waitFixtureRPCTrace(t, trace, "catalog_waiting", "")
	if w.client.CommandCatalog().State != omp.CatalogUnknown {
		t.Fatal("fixture did not leave the native command catalog unknown")
	}
	before := w.binding
	target := filepath.Join(filepath.Dir(before.Workspace), "renamed after catalog update")
	requireStoreOK(t, os.Rename(before.Workspace, target))
	nativeInput(t, w, 101, "/move "+target, false)
	if w.workspaceMove == nil || w.workspaceMove.phase != "catalog" {
		t.Fatal("move did not wait for native command catalog discovery")
	}
	nativeRPC(t, w, "fixture_catalog_update", nil)
	nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && w.workspaceMove == nil })
	saved, err := w.b.db.Binding(before.Bot, before.Chat, before.Thread)
	requireStoreOK(t, err)
	if w.client == nil || w.sessionID != before.SessionID || saved.SessionID != before.SessionID || saved.Workspace != target {
		t.Fatalf("reader-published catalog did not complete the native move: saved=%+v binding=%+v", saved, w.binding)
	}
	moves := 0
	for _, entry := range fixtureRPCTrace(trace) {
		if entry.kind == "prompt" && entry.payload == "/move "+target {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("native /move prompt count = %d, want exactly one", moves)
	}
}
