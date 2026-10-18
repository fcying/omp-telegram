package bridge

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func deletePickerButton(t *testing.T, f *fakeHTTP, row int) map[string]any {
	t.Helper()
	_, rows := pickerView(t, f)
	if row >= len(rows)-1 || len(rows[row]) != 3 || rows[row][2]["text"] != "Delete" || rows[row][2]["style"] != "danger" {
		t.Fatalf("resume picker has no destructive delete for row %d: %v", row, rows)
	}
	return rows[row][2]
}

func clickSessionButton(w *worker, f *fakeHTTP, user int64, data string) {
	w.callback(&telegram.CallbackQuery{ID: "session-delete", From: telegram.User{ID: user}, Message: &telegram.Message{MessageID: int64(f.messageCount())}, Data: data})
}

func awaitSessionDelete(t *testing.T, w *worker) sessionDeleteResult {
	t.Helper()
	select {
	case result := <-w.deleteResults:
		return result
	case <-time.After(8 * time.Second):
		t.Fatal("native session deletion did not finish")
		return sessionDeleteResult{}
	}
}

func TestSessionDeleteConfirmsAndRefreshesWithoutChangingWorkspace(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	original := w.binding
	sessions := setResumeFixtures(t, original.Workspace, 2)
	saved := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), sessions[0].ID+".jsonl")
	marker := filepath.Join(original.Workspace, "keep.txt")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	requireStoreOK(t, w.b.db.SetPinnedSession(w.b.bot.ID, w.key.chat, w.key.thread, original.Workspace, sessions[0].ID, true))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	// Pinned entries sort first; the selected ID remains the native full ID.
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "ID: "+sessions[0].ID) || !strings.Contains(text, "artifacts") || !strings.Contains(text, "workspace files will not be deleted") || len(rows) != 1 || len(rows[0]) != 2 || rows[0][0]["style"] != "danger" || rows[0][1]["text"] != "Cancel" {
		t.Fatalf("delete confirmation omitted identity, warning, or destructive button: %q; %v", text, rows)
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	w.resumeListed(finishResumeList(t, w))
	text, rows = pickerView(t, f)
	if strings.Contains(text, sessions[0].ID) || !strings.Contains(text, sessions[1].ID) || len(rows) != 2 {
		t.Fatalf("refreshed picker still lists deleted session: %q; %v", text, rows)
	}
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatalf("native session file still exists: %v", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("workspace file changed: %q, %v", data, err)
	}
	pinned, err := w.b.db.PinnedSessions(w.b.bot.ID, w.key.chat, w.key.thread, original.Workspace)
	requireStoreOK(t, err)
	if _, found := pinned[pinnedSessionKey(sessions[0].ID)]; found || !sameBindingIdentity(w.binding, original) || w.deleteCancel != nil || w.b.sessionInUse(sessions[0].ID) {
		t.Fatal("deletion left pin or reservation, or changed Telegram binding")
	}
}

func TestSessionDeleteAfterCloseReleasesAliasedSessionClaims(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	sessionID, sessionFile, workspace := w.sessionID, w.binding.Session, w.binding.Workspace
	alias := sessionFile + ".claim-alias"
	if err := os.Symlink(sessionFile, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	if !sameSessionFile(sessionFile, alias) || !w.claimSession(alias, sessionID) {
		t.Fatal("could not reproduce a second path claim for the same native session")
	}
	command("/close")

	data, err := json.Marshal([]resumeFixtureSession{{ID: sessionID, CWD: workspace, Title: "closed-session"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	button := deletePickerButton(t, f, 0)
	clickSessionButton(w, f, 7, button["callback_data"].(string))
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Delete omp session?") || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("closed session was not offered for deletion: %q; %v", text, rows)
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("closed native session was not deleted: %v", err)
	}
}

func TestSessionDeleteRemovesReleasedBindingFromAnotherTopic(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	workspace := w.binding.Workspace
	other := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, cancel: func() {}, confirms: make(map[string]confirmation)})
	t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
	other.start(false, workspace, "", false)
	if other.client == nil {
		t.Fatal("old topic session did not start")
	}
	sessionID, sessionFile := other.sessionID, other.binding.Session
	other.releaseRuntime(true)
	if other.client != nil || other.runtime != runtimeReleased || !other.binding.Running {
		t.Fatal("old topic runtime was not released with its binding preserved")
	}

	data, err := json.Marshal([]resumeFixtureSession{{ID: sessionID, CWD: workspace, Title: "released-session"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	listText, _ := pickerView(t, f)
	if strings.Contains(listText, "[in use elsewhere]") {
		t.Fatalf("released session was still marked in use elsewhere: %q", listText)
	}
	button := deletePickerButton(t, f, 0)
	clickSessionButton(w, f, 7, button["callback_data"].(string))
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Delete omp session?") || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("released session in another topic was not offered for deletion: %q; %v", text, rows)
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if _, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, 22); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session binding remained on the released topic: %v", err)
	}
	if w.b.sessionInUse(sessionID) {
		t.Fatal("released session claim remained after deletion")
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("released session was not deleted: %v", err)
	}
	if _, err := other.ensureRuntime(); err == nil || !strings.Contains(err.Error(), "Cannot restore the saved omp session") {
		t.Fatalf("released topic did not report its missing session on resume: %v", err)
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("failed resume recreated deleted session: %v", err)
	}
	w.b.fatal = make(chan error, 1)
	if !other.closeLogicalSession() {
		t.Fatal("/close failed after another topic deleted the released session binding")
	}
	if other.binding.Running {
		t.Fatal("close retained running state for a deleted binding")
	}
	select {
	case err := <-w.b.fatal:
		t.Fatalf("closing a stale binding failed the bridge: %v", err)
	default:
	}

}

func TestLazyResumeDoesNotStartWhileSessionDeleteIsReserved(t *testing.T) {
	sessionRoot := t.TempDir()
	startMarker := filepath.Join(t.TempDir(), "omp-started")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", sessionRoot)
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_START_MARKER", startMarker)
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	if _, err := os.Stat(startMarker); err != nil {
		t.Fatalf("initial OMP process did not start: %v", err)
	}
	if err := os.Remove(startMarker); err != nil {
		t.Fatal(err)
	}
	sessionID := w.binding.SessionID
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased || !w.binding.Running {
		t.Fatal("idle release did not retain the logical session")
	}
	deleter := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, cancel: func() {}, confirms: make(map[string]confirmation)})
	t.Cleanup(func() {
		w.b.releaseDelete(deleter)
		deleter.teardownWorker(true)
		deleter.background.Wait()
	})
	if !w.b.reserveDelete(deleter, sessionID) {
		t.Fatal("delete reservation was not acquired for the released session")
	}
	if _, err := w.ensureRuntime(); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("lazy resume ignored the delete reservation: %v", err)
	}
	if _, err := os.Stat(startMarker); !os.IsNotExist(err) {
		t.Fatalf("lazy resume launched OMP while deletion was reserved: %v", err)
	}
	if w.client != nil || w.runtime != runtimeReleased || w.sessionRuntimeStarting.Load() {
		t.Fatal("rejected lazy resume changed runtime state")
	}
}

func TestLazyResumeStartFailureReleasesSessionClaim(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 1)
	command("/new test")
	sessionID := w.binding.SessionID
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	w.b.cfg.OMP = filepath.Join(t.TempDir(), "missing-omp")
	if _, err := w.ensureRuntime(); err == nil {
		t.Fatal("lazy resume succeeded with a missing OMP executable")
	}
	if w.b.sessionInUse(sessionID) || w.sessionRuntimeStarting.Load() || w.sessionRuntimeActive.Load() {
		t.Fatal("failed lazy resume retained its session claim or runtime state")
	}
}

func TestSessionDeleteAllowsRestoredReleasedBinding(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	sessionFile := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")
	binding := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: 22, Workspace: w.binding.Workspace, Session: sessionFile, SessionID: session.ID, Generation: 1, Running: true}
	requireStoreOK(t, w.b.db.Save(binding))
	other := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, cancel: func() {}, binding: binding, runtimeLifecycle: runtimeLifecycle{restoring: true}, confirms: make(map[string]confirmation)})
	t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
	if !other.claimPersistedSession() {
		t.Fatal("startup could not restore the old topic's logical session claim")
	}
	other.restoreBinding()
	if other.sessionRuntimeActive.Load() || other.sessionRuntimeStarting.Load() || other.runtime != runtimeReleased {
		t.Fatal("restored logical binding was treated as an open OMP runtime")
	}

	data, err := json.Marshal([]resumeFixtureSession{{ID: session.ID, CWD: w.binding.Workspace, Title: "restored-released-session"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	button := deletePickerButton(t, f, 0)
	clickSessionButton(w, f, 7, button["callback_data"].(string))
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Delete omp session?") || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("restored released session was not offered for deletion: %q; %v", text, rows)
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if _, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, 22); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session binding remained after restored-topic deletion: %v", err)
	}
	if w.b.sessionInUse(session.ID) {
		t.Fatal("restored session claim remained after deletion")
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("restored released session was not deleted: %v", err)
	}
}

func TestIdleReleasedSessionCanBeDeleted(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	sessionID, sessionFile, workspace := w.sessionID, w.binding.Session, w.binding.Workspace
	w.b.cfg.IdleTimeout = time.Minute
	w.lastActivity = time.Now().Add(-2 * time.Minute)
	w.releaseIdleRuntime(time.Now())
	if w.client != nil || w.runtime != runtimeReleased || !w.binding.Running {
		t.Fatal("idle release did not preserve the logical session")
	}
	data, err := json.Marshal([]resumeFixtureSession{{ID: sessionID, CWD: workspace, Title: "idle-session"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	button := deletePickerButton(t, f, 0)
	clickSessionButton(w, f, 7, button["callback_data"].(string))
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Delete omp session?") || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("idle-released session was not offered for deletion: %q; %v", text, rows)
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("native session file remains after deletion: %v", err)
	}
	if _, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session binding remained: %v", err)
	}
	if w.b.sessionInUse(sessionID) {
		t.Fatal("deleted session identity remains claimed")
	}
}

func TestSessionDeleteCancelAndRejectedTargetsKeepNativeFile(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	for _, scenario := range []string{"cancel", "wrong-user", "current", "stale-epoch", "wrong-workspace", "other-active"} {
		t.Run(scenario, func(t *testing.T) {
			w, f, command := setupWorkspaceWorker(t)
			w.b.slots = make(chan struct{}, 2)
			command("/new test")
			sessions := setResumeFixtures(t, w.binding.Workspace, 1)
			selected := sessions[0]
			if scenario == "current" {
				selected.ID = w.sessionID
			} else if scenario == "other-active" {
				other := testWorker(t, &worker{b: w.b, key: target{chat: -10, thread: 22}, ctx: w.ctx, cancel: func() {}, confirms: make(map[string]confirmation)})
				other.start(false, w.binding.Workspace, "", false)
				if other.client == nil {
					t.Fatal("other topic did not start")
				}
				t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
				selected.ID = other.sessionID
			}
			if scenario == "current" || scenario == "other-active" {
				data, _ := json.Marshal([]resumeFixtureSession{{ID: selected.ID, CWD: w.binding.Workspace, Title: "in-use", UpdatedAt: selected.UpdatedAt}})
				t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS", string(data))
			}
			saved := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), selected.ID+".jsonl")
			command("/resume")
			w.resumeListed(finishResumeList(t, w))
			if scenario == "other-active" {
				text, _ := pickerView(t, f)
				if !strings.Contains(text, "[in use elsewhere]") {
					t.Fatalf("picker did not mark the session used by another topic: %q", text)
				}
			}
			button := deletePickerButton(t, f, 0)
			if scenario == "wrong-workspace" {
				for token, c := range w.confirms {
					c.sessions[0].CWD = t.TempDir()
					w.confirms[token] = c
				}
			}
			if scenario == "wrong-user" {
				clickSessionButton(w, f, 8, button["callback_data"].(string))
				if _, err := os.Stat(saved); err != nil || w.deleteCancel != nil {
					t.Fatal("unauthorized callback changed native session")
				}
				return
			}
			clickSessionButton(w, f, 7, button["callback_data"].(string))
			if scenario == "current" || scenario == "current-research" || scenario == "other-active" || scenario == "wrong-workspace" {
				if _, err := os.Stat(saved); err != nil || w.deleteCancel != nil {
					t.Fatal("ineligible native session was deleted")
				}
				if scenario == "other-active" {
					var reply string
					if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&reply); err != nil || !strings.Contains(reply, "another conversation") {
						t.Fatalf("cross-conversation rejection was unclear: %q, %v", reply, err)
					}
				}
				return
			}
			_, rows := pickerView(t, f)
			if scenario == "stale-epoch" {
				w.b.bindingsEpoch.Add(1)
			}
			index := 0
			if scenario == "cancel" {
				index = 1
			}
			clickSessionButton(w, f, 7, rows[0][index]["callback_data"].(string))
			if scenario == "cancel" {
				text, rows := pickerView(t, f)
				if text != "Cancel" || len(rows) != 0 {
					t.Fatalf("cancelled delete retained its question or choices: %q; %v", text, rows)
				}
			}
			if _, err := os.Stat(saved); err != nil || w.deleteCancel != nil || w.b.sessionInUse(selected.ID) {
				t.Fatal("cancelled or stale confirmation deleted or reserved a session")
			}
		})
	}
}

func TestSessionDeleteRechecksLiveSessionAtConfirmation(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	saved := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")
	other := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, cancel: func() {}, confirms: make(map[string]confirmation)})
	t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
	other.start(true, session.ID, w.binding.Workspace, false)
	if other.client == nil || !other.sessionRuntimeActive.Load() {
		t.Fatal("second topic did not open the selected native session")
	}
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	if _, err := os.Stat(saved); err != nil || w.deleteCancel != nil {
		t.Fatal("confirmed delete ignored newly active session")
	}
	var reply string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&reply); err != nil || !strings.Contains(reply, "currently active or being processed") {
		t.Fatalf("active-session rejection was unclear: %q, %v", reply, err)
	}
}

func TestSessionDeleteUnknownRunningBindingOnlyBlocksItsWorkspace(t *testing.T) {
	for _, sameWorkspace := range []bool{false, true} {
		name := "different-workspace"
		if sameWorkspace {
			name = "same-workspace"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
			w, f, command := setupWorkspaceWorker(t)
			command("/new test")
			session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
			workspace := t.TempDir()
			if sameWorkspace {
				workspace = w.binding.Workspace
			}
			legacy := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: 22, Workspace: workspace, Session: filepath.Join(workspace, "legacy-session.jsonl"), Generation: 1, Running: true}
			requireStoreOK(t, w.b.db.Save(legacy))
			command("/resume")
			w.resumeListed(finishResumeList(t, w))
			clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
			_, rows := pickerView(t, f)
			if sameWorkspace {
				if len(rows) != 2 || w.deleteCancel != nil {
					t.Fatal("unknown running binding in the same workspace allowed deletion")
				}
				return
			}
			if len(rows) != 1 || len(rows[0]) != 2 {
				t.Fatal("unrelated unknown running binding blocked deletion confirmation")
			}
			clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
			result := awaitSessionDelete(t, w)
			if result.err != nil {
				t.Fatal(result.err)
			}
			w.sessionDeleteFinished(result)
			if _, err := os.Stat(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")); !os.IsNotExist(err) {
				t.Fatalf("unrelated unknown binding prevented native deletion: %v", err)
			}
			stored, err := w.b.db.Binding(legacy.Bot, legacy.Chat, legacy.Thread)
			requireStoreOK(t, err)
			if stored != legacy {
				t.Fatalf("native deletion changed unrelated running binding: %+v", stored)
			}
		})
	}
}

func TestSessionDeleteReservationBlocksResumeExportAndSecondDelete(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	trace := filepath.Join(t.TempDir(), "rpc.log")
	gate := filepath.Join(t.TempDir(), "allow-delete")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE_GATE", gate)
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	waitFixtureRPCTrace(t, trace, "session_delete", session.ID)
	other := testWorker(t, &worker{b: w.b, key: target{chat: -10, thread: 22}, ctx: w.ctx, confirms: make(map[string]confirmation)})
	t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
	if !w.b.sessionInUse(session.ID) || w.b.reserveDelete(other, session.ID) || w.b.reserveExport(other, session.ID) || other.claimSession(filepath.Join(w.binding.Workspace, "other.jsonl"), session.ID) {
		t.Fatal("native session became claimable while deletion was in progress")
	}
	if message := other.startInternal(true, session.ID, "", false, 0, false, 0); message == "" || other.client != nil {
		t.Fatal("delete reservation allowed another conversation to resume")
	}
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil || len(intents) != 0 {
		t.Fatalf("blocked resume persisted a startup intent: %+v, %v", intents, err)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if w.b.sessionInUse(session.ID) {
		t.Fatal("delete reservation survived completion")
	}
	w.resumeListed(finishResumeList(t, w))
}

func TestSessionDeleteFailureRetainsPinAndReleasesReservation(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "fail")
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	requireStoreOK(t, w.b.db.SetPinnedSession(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Workspace, session.ID, true))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	before := w.b.bindingsEpoch.Load()
	result := awaitSessionDelete(t, w)
	if result.err == nil {
		t.Fatal("rejected native command was treated as deletion")
	}
	w.sessionDeleteFinished(result)
	if got := w.b.bindingsEpoch.Load(); got != before {
		t.Fatalf("failed native deletion invalidated picker snapshots: epoch %d, want %d", got, before)
	}
	pinned, err := w.b.db.PinnedSessions(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Workspace)
	requireStoreOK(t, err)
	if _, exists := pinned[pinnedSessionKey(session.ID)]; !exists || w.b.sessionInUse(session.ID) || w.deleteCancel != nil {
		t.Fatal("failed deletion cleared pin or leaked reservation")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")); err != nil {
		t.Fatal("failed deletion removed native file")
	}
}

func TestSessionDeleteMissingFileAfterFailedRPCInvalidatesOldPicker(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "deleted_bad_ack")
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	requireStoreOK(t, w.b.db.SetPinnedSession(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Workspace, session.ID, true))
	peerBinding := w.binding
	peerBinding.Thread = 22
	peerBinding.Running = false
	peerBinding.Session = ""
	peerBinding.SessionID = ""
	requireStoreOK(t, w.b.db.Save(peerBinding))
	peer := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, confirms: make(map[string]confirmation), binding: peerBinding, resumeResults: make(chan resumeListResult, 1)})
	t.Cleanup(func() { peer.teardownWorker(true); peer.background.Wait() })
	peer.requestResumeList(7)
	peer.resumeListed(finishResumeList(t, peer))
	_, peerRows := pickerView(t, f)
	oldButton := peerRows[0][0]["callback_data"].(string)
	oldMessageID := int64(f.messageCount())

	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err == nil {
		t.Fatal("failed RPC acknowledgement was treated as confirmed deletion")
	}
	file := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("native session file was not removed: %v", err)
	}
	epoch := w.b.bindingsEpoch.Load()
	w.sessionDeleteFinished(result)
	if w.b.bindingsEpoch.Load() != epoch+1 || w.b.sessionInUse(session.ID) {
		t.Fatal("missing native file did not invalidate old menus and release reservation")
	}
	pinned, err := w.b.db.PinnedSessions(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Workspace)
	requireStoreOK(t, err)
	if _, exists := pinned[pinnedSessionKey(session.ID)]; !exists {
		t.Fatal("uncertain deletion was treated as confirmed pin cleanup")
	}
	marker := filepath.Join(t.TempDir(), "unexpected-rpc-start")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_START_MARKER", marker)
	peer.callback(&telegram.CallbackQuery{ID: "old-resume-after-failed-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: oldMessageID}, Data: oldButton})
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil || len(intents) != 0 || peer.startIntent != nil || peer.client != nil {
		t.Fatalf("old picker resumed missing native session: %+v, %v", intents, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("old picker launched OMP after uncertain deletion: %v", err)
	}
}

func TestSessionDeleteInvalidatesPickersWhenPinCleanupFails(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	requireStoreOK(t, w.b.db.SetPinnedSession(w.b.bot.ID, w.key.chat, w.key.thread, w.binding.Workspace, session.ID, true))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	_, err := w.b.db.DB.Exec("CREATE TRIGGER reject_pin_cleanup BEFORE DELETE ON session_favorites BEGIN SELECT RAISE(FAIL, 'blocked'); END")
	requireStoreOK(t, err)
	before := w.b.bindingsEpoch.Load()
	w.sessionDeleteFinished(result)
	if got := w.b.bindingsEpoch.Load(); got != before+1 {
		t.Fatalf("pin cleanup failure suppressed deletion invalidation: epoch %d, want %d", got, before+1)
	}
	if w.resumeCancel == nil {
		t.Fatal("owner picker was not refreshed after native deletion")
	}
	w.resumeListed(finishResumeList(t, w))
}

func TestSessionDeleteInvalidatesOtherConversationPickersAndPendingLists(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	trace := filepath.Join(t.TempDir(), "rpc.log")
	gate := filepath.Join(t.TempDir(), "allow-delete")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE_GATE", gate)
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	other := func(thread int64) *worker {
		binding := w.binding
		binding.Thread = thread
		binding.Running = false
		binding.Session = ""
		binding.SessionID = ""
		requireStoreOK(t, w.b.db.Save(binding))
		peer := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: thread}, ctx: w.ctx, confirms: make(map[string]confirmation), binding: binding, resumeResults: make(chan resumeListResult, 1)})
		t.Cleanup(func() { peer.teardownWorker(true); peer.background.Wait() })
		return peer
	}
	stale := other(22)
	listing := other(33)
	stale.requestResumeList(7)
	stale.resumeListed(finishResumeList(t, stale))
	_, staleRows := pickerView(t, f)
	stalePin := staleRows[0][1]["callback_data"].(string)
	staleMessageID := int64(f.messageCount())
	listing.requestResumeList(7)
	pending := finishResumeList(t, listing)
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	waitFixtureRPCTrace(t, trace, "session_delete", session.ID)
	before := w.b.bindingsEpoch.Load()
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	if w.b.bindingsEpoch.Load() != before+1 {
		t.Fatal("successful native delete did not invalidate global picker snapshots")
	}
	count := f.messageCount()
	listing.resumeListed(pending)
	if len(listing.confirms) != 0 || f.messageCount() != count {
		t.Fatal("pre-deletion native list published a picker in another conversation")
	}
	stale.callback(&telegram.CallbackQuery{ID: "stale-pin", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: staleMessageID}, Data: stalePin})
	pinned, err := w.b.db.PinnedSessions(w.b.bot.ID, stale.key.chat, stale.key.thread, w.binding.Workspace)
	requireStoreOK(t, err)
	if _, exists := pinned[pinnedSessionKey(session.ID)]; exists {
		t.Fatal("pre-deletion picker re-pinned a deleted session in another conversation")
	}
	w.resumeListed(finishResumeList(t, w))
}

func TestSessionDeleteFencesResumeAfterPickerCallbackPassedEpochCheck(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	trace := filepath.Join(t.TempDir(), "rpc.log")
	deleteGate := filepath.Join(t.TempDir(), "allow-delete")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE_GATE", deleteGate)
	w, f, command := setupWorkspaceWorker(t)
	w.b.slots = make(chan struct{}, 2)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	binding := w.binding
	binding.Thread = 22
	binding.Running = false
	binding.Session = ""
	binding.SessionID = ""
	requireStoreOK(t, w.b.db.Save(binding))
	peer := testWorker(t, &worker{b: w.b, key: target{chat: w.key.chat, thread: 22}, ctx: w.ctx, confirms: make(map[string]confirmation), binding: binding, resumeResults: make(chan resumeListResult, 1)})
	t.Cleanup(func() { peer.teardownWorker(true); peer.background.Wait() })
	peer.requestResumeList(7)
	peer.resumeListed(finishResumeList(t, peer))
	_, peerRows := pickerView(t, f)
	oldButton := peerRows[0][0]["callback_data"].(string)
	oldMessageID := int64(f.messageCount())

	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	_, rows := pickerView(t, f)
	clickSessionButton(w, f, 7, rows[0][0]["callback_data"].(string))
	waitFixtureRPCTrace(t, trace, "session_delete", session.ID)

	pickerGate := make(chan struct{})
	defer func() {
		select {
		case <-pickerGate:
		default:
			close(pickerGate)
		}
	}()
	pickerEntered := make(chan struct{})
	var enteredOnce sync.Once
	http.DefaultTransport = logTestTransport(func(r *http.Request) (*http.Response, error) {
		if filepath.Base(r.URL.Path) == "editMessageText" {
			enteredOnce.Do(func() { close(pickerEntered) })
			select {
			case <-pickerGate:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return f.RoundTrip(r)
	})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		peer.callback(&telegram.CallbackQuery{ID: "stale-resume", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: oldMessageID}, Data: oldButton})
	}()
	select {
	case <-pickerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("picker callback did not reach its decision edit")
	}
	if err := os.WriteFile(deleteGate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	result := awaitSessionDelete(t, w)
	if result.err != nil {
		t.Fatal(result.err)
	}
	w.sessionDeleteFinished(result)
	marker := filepath.Join(t.TempDir(), "unexpected-rpc-start")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_START_MARKER", marker)
	close(pickerGate)
	select {
	case <-callbackDone:
	case <-time.After(8 * time.Second):
		t.Fatal("stale picker callback did not complete")
	}
	intents, err := w.b.db.PendingStarts(w.b.bot.ID)
	if err != nil || len(intents) != 0 || peer.startIntent != nil {
		t.Fatalf("deleted session was prepared for resume: %+v, %v", intents, err)
	}
	if peer.client != nil || peer.runtime == runtimeStarting {
		t.Fatal("stale picker started a runtime")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale picker spawned OMP: %v", err)
	}
}

func TestSessionDeletePendingStartPreventsExecution(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	command("/new test")
	session := setResumeFixtures(t, w.binding.Workspace, 1)[0]
	previous := store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: 22}
	intent := store.StartIntent{Bot: previous.Bot, Chat: previous.Chat, Thread: previous.Thread, Kind: "resume", Workspace: w.binding.Workspace, Session: session.ID, Generation: 1}
	requireStoreOK(t, w.b.db.PrepareStart(previous, intent))
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	clickSessionButton(w, f, 7, deletePickerButton(t, f, 0)["callback_data"].(string))
	for _, c := range w.confirms {
		if c.action == "session_delete" {
			t.Fatal("pending resume intent allowed a deletion confirmation")
		}
	}
	if w.deleteCancel != nil {
		t.Fatal("delete started against a pending resume intent")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), session.ID+".jsonl")); err != nil {
		t.Fatalf("native file changed during pending resume: %v", err)
	}
}
