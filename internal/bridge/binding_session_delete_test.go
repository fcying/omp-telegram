package bridge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func TestBindingSessionDeleteClosed(t *testing.T) {
	for _, scenario := range []string{"success", "fail", "deleted_bad_ack", "missing-workspace", "new-workspace-file", "wrong-user", "stale-generation", "stale-epoch"} {
		t.Run(scenario, func(t *testing.T) {
			mode := "success"
			if scenario == "fail" || scenario == "deleted_bad_ack" {
				mode = scenario
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", mode)
			root := t.TempDir()
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", root)
			w, fake, _ := setupWorkspaceWorker(t)
			workspace := t.TempDir()
			missing := scenario == "missing-workspace" || scenario == "new-workspace-file"
			if missing {
				workspace = filepath.Join(workspace, "missing", "original")
			}
			binding := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 4, Workspace: workspace, SessionID: "abcd0000-0000-4000-8000-000000000000"}
			binding.Session = filepath.Join(root, binding.SessionID+".jsonl")
			header, err := json.Marshal(map[string]string{"type": "session", "id": binding.SessionID, "cwd": workspace})
			requireStoreOK(t, err)
			requireStoreOK(t, os.WriteFile(binding.Session, header, 0600))
			requireStoreOK(t, w.b.db.Save(binding))
			requireStoreOK(t, w.b.db.SetPinnedSession(99, -10, 22, workspace, binding.SessionID, true))
			w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &binding}}, user: 7}, 0, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, 22))
			c := onlyBindingConfirmation(t, w)
			data := bindingButton(t, fake, "Binding+Session")
			query := &telegram.CallbackQuery{ID: "combined", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: c.messageID}, Data: data}
			switch scenario {
			case "wrong-user":
				query.From.ID = 8
			case "stale-generation":
				binding.Generation++
				requireStoreOK(t, w.b.db.Save(binding))
			case "stale-epoch":
				w.b.bindingsEpoch.Add(1)
			}
			gate := filepath.Join(root, "release-delete")
			if missing {
				t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE_GATE", gate)
			}
			w.callback(query)
			rejected := scenario == "wrong-user" || scenario == "stale-generation" || scenario == "stale-epoch"
			if rejected {
				if w.deleteCancel != nil {
					t.Fatal("unauthorized or stale callback started native deletion")
				}
			} else {
				if w.deleteCancel == nil {
					t.Fatal("combined callback did not start native deletion")
				}
				assertFinishedMenu(t, fake, int(c.messageID), "Selected: Binding+Session")
				if missing {
					waitFor(t, func() bool { info, err := os.Stat(workspace); return err == nil && info.IsDir() })
					if scenario == "new-workspace-file" {
						requireStoreOK(t, os.WriteFile(filepath.Join(workspace, "keep.txt"), []byte("user data"), 0600))
					}
					requireStoreOK(t, os.WriteFile(gate, nil, 0600))
				}
				result := awaitSessionDelete(t, w)
				if (result.err != nil) != (mode != "success" || scenario == "new-workspace-file") {
					t.Fatalf("native deletion result = %v, mode = %s", result.err, mode)
				}
				if (scenario == "new-workspace-file" || mode == "deleted_bad_ack") && !errors.Is(result.err, omp.ErrSessionFileGone) {
					t.Fatalf("partial native deletion did not report missing session: %v", result.err)
				}
				w.sessionDeleteFinished(result)
			}
			saved, err := w.b.db.Binding(99, -10, 22)
			if mode != "success" || rejected || scenario == "new-workspace-file" {
				if err != nil || !sameBindingIdentity(saved, binding) {
					t.Fatalf("failed or rejected deletion lost binding: %+v, %v", saved, err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("successful native deletion retained binding: %v", err)
			}
			_, fileErr := os.Stat(binding.Session)
			gone := !rejected && mode != "fail"
			if os.IsNotExist(fileErr) != gone || fileErr != nil && !os.IsNotExist(fileErr) {
				t.Fatalf("native file outcome = %v, want gone %t", fileErr, gone)
			}
			pins, err := w.b.db.PinnedSessions(99, -10, 22, workspace)
			requireStoreOK(t, err)
			if (len(pins) == 0) != (!rejected && mode == "success" && scenario != "new-workspace-file") {
				t.Fatalf("pin cleanup did not follow confirmed native success: %+v", pins)
			}
			if scenario == "missing-workspace" {
				if _, err := os.Stat(filepath.Dir(workspace)); !os.IsNotExist(err) {
					t.Fatalf("temporary workspace components remained: %v", err)
				}
			} else if scenario == "new-workspace-file" {
				if data, err := os.ReadFile(filepath.Join(workspace, "keep.txt")); err != nil || string(data) != "user data" {
					t.Fatalf("workspace cleanup removed user data: %q, %v", data, err)
				}
			}
			if !rejected {
				w.callback(query)
				if w.deleteCancel != nil {
					t.Fatal("consumed combined callback replayed native deletion")
				}
			}
		})
	}
}

func TestBindingSessionDeleteOpen(t *testing.T) {
	for _, scenario := range []string{"success", "fail", "active"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", scenario)
			fake, db, send := setupBridge(t)
			workspace := t.TempDir()
			send(update(1, 22, "/new "+workspace))
			waitFor(t, func() bool { return fake.has(22, "omp is ready.") })
			bound, err := db.Binding(99, -10, 22)
			requireStoreOK(t, err)
			marker := filepath.Join(workspace, "keep.txt")
			requireStoreOK(t, os.WriteFile(marker, []byte("user data"), 0600))
			if scenario == "active" {
				send(update(2, 22, "wait"))
				waitFor(t, func() bool {
					var state string
					return db.DB.QueryRow("SELECT state FROM inbox WHERE id=2").Scan(&state) == nil && state == "submitted"
				})
			}
			before := fake.messageCount()
			send(update(3, 11, "/bindings"))
			listID, data := waitBindingAction(t, fake, before, "Saved bindings", "")
			sendBindingCallback(t, db, send, 4, listID, data)
			confirmID, data := waitBindingAction(t, fake, int(listID), "Topic 22?", "Binding+Session")
			sendBindingCallback(t, db, send, 5, confirmID, data)
			switch scenario {
			case "success":
				waitFor(t, func() bool { _, err := db.Binding(99, -10, 22); return errors.Is(err, sql.ErrNoRows) })
			case "fail":
				waitFor(t, func() bool {
					return fake.has(11, "Native session deletion or temporary directory cleanup failed or was not confirmed.")
				})
			case "active":
				waitFor(t, func() bool { return fake.has(11, "Binding is busy.") })
			}
			if scenario != "success" {
				saved, err := db.Binding(99, -10, 22)
				if err != nil || saved.Session != bound.Session || saved.SessionID != bound.SessionID || saved.Running != (scenario == "active") {
					t.Fatalf("failed deletion lost identity or incorrect logical state: %+v, %v", saved, err)
				}
			}
			_, fileErr := os.Stat(bound.Session)
			if os.IsNotExist(fileErr) != (scenario == "success") || fileErr != nil && !os.IsNotExist(fileErr) {
				t.Fatalf("native file outcome = %v for %s", fileErr, scenario)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "user data" {
				t.Fatalf("combined deletion changed workspace: %q, %v", data, err)
			}
		})
	}
}

func TestBindingSessionDeleteSharedSession(t *testing.T) {
	for _, state := range []string{"closed", "released"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
			root := t.TempDir()
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", root)
			w, fake, _ := setupWorkspaceWorker(t)
			first := store.Binding{Bot: 99, Chat: -10, Thread: 22, Workspace: t.TempDir(), SessionID: "abcdef00-0000-4000-8000-000000000000", Generation: 1}
			first.Session = filepath.Join(root, first.SessionID+".jsonl")
			header, err := json.Marshal(map[string]string{"type": "session", "id": first.SessionID, "cwd": first.Workspace})
			requireStoreOK(t, err)
			requireStoreOK(t, os.WriteFile(first.Session, header, 0600))
			second := first
			second.Thread = 33
			second.SessionID = "ABCDEF00-0000-4000-8000-000000000000"
			second.Running = state == "released"
			requireStoreOK(t, w.b.db.Save(first))
			requireStoreOK(t, w.b.db.Save(second))
			if second.Running {
				other := testWorker(t, &worker{b: w.b, key: target{chat: second.Chat, thread: second.Thread}, ctx: w.ctx, cancel: func() {}, binding: second, runtimeLifecycle: runtimeLifecycle{restoring: true}, confirms: make(map[string]confirmation)})
				t.Cleanup(func() { other.teardownWorker(true); other.background.Wait() })
				if !other.claimPersistedSession() {
					t.Fatal("could not restore the other topic's session claim")
				}
				other.restoreBinding()
				if other.runtime != runtimeReleased || other.client != nil {
					t.Fatal("other topic did not retain a released runtime")
				}
			}
			w.showBindingsPage(confirmation{bindings: []store.BindingListEntry{{Binding: &first}}, user: 7, epoch: w.b.bindingsEpoch.Load()}, 0, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, first.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			if w.deleteCancel == nil {
				t.Fatal("shared inactive reference prevented combined deletion")
			}
			result := awaitSessionDelete(t, w)
			requireStoreOK(t, result.err)
			w.sessionDeleteFinished(result)
			for _, binding := range []store.Binding{first, second} {
				if _, err := w.b.db.Binding(binding.Bot, binding.Chat, binding.Thread); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("combined deletion retained a session binding: %v", err)
				}
			}
			if _, err := os.Stat(first.Session); !os.IsNotExist(err) {
				t.Fatalf("combined deletion retained native history: %v", err)
			}
			if w.b.sessionInUse(first.SessionID) {
				t.Fatal("combined deletion retained a session claim")
			}
		})
	}
}

func TestBindingSessionDeleteMissingFileReturnsToList(t *testing.T) {
	w, fake, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	w.b.cfg.OMP = filepath.Join(root, "must-not-start")
	binding := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 4, Workspace: filepath.Join(root, "missing-workspace"), Session: filepath.Join(root, "missing.jsonl"), SessionID: "abcd0000-0000-4000-8000-000000000000"}
	requireStoreOK(t, w.b.db.Save(binding))
	other := binding
	other.Thread = 33
	requireStoreOK(t, w.b.db.Save(other))
	requireStoreOK(t, w.b.db.SetPinnedSession(99, -10, 22, binding.Workspace, binding.SessionID, true))
	requireStoreOK(t, w.b.db.SetPinnedSession(99, -10, 33, binding.Workspace, binding.SessionID, true))
	unrelated := binding
	unrelated.Thread = 44
	unrelated.SessionID = "ffff0000-0000-4000-8000-000000000000"
	unrelated.Session = filepath.Join(root, "unrelated.jsonl")
	requireStoreOK(t, w.b.db.Save(unrelated))
	w.showBindings(7, true, 0)
	clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, binding.Thread))
	clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
	result := awaitSessionDelete(t, w)
	if !errors.Is(result.err, omp.ErrBoundSessionMissing) {
		t.Fatalf("missing session was not distinguished from native deletion failure: %v", result.err)
	}
	w.sessionDeleteFinished(result)
	clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, unrelated.Thread))
	c := onlyBindingConfirmation(t, w)
	if c.deleteThread != unrelated.Thread {
		t.Fatalf("returned list did not offer the remaining binding: %+v", c)
	}
	clickSessionButton(w, fake, 7, bindingButton(t, fake, "Cancel"))
	for _, expected := range []store.Binding{binding, other} {
		if _, err := w.b.db.Binding(expected.Bot, expected.Chat, expected.Thread); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("missing session retained a binding: %v", err)
		}
	}
	for _, thread := range []int64{22, 33} {
		pins, err := w.b.db.PinnedSessions(99, -10, thread, binding.Workspace)
		requireStoreOK(t, err)
		if _, ok := pins[binding.SessionID]; ok {
			t.Fatal("missing session retained a pin")
		}
	}
	if _, err := os.Stat(binding.Workspace); !os.IsNotExist(err) {
		t.Fatalf("missing session recreated the workspace: %v", err)
	}
	if w.b.sessionInUse(binding.SessionID) || w.deleteCancel != nil {
		t.Fatal("missing session retained the delete reservation")
	}
}

func TestBindingSessionDeleteMissingFilePreservesConflictingReferences(t *testing.T) {
	for _, pathState := range []string{"existing", "missing", "empty"} {
		t.Run(pathState, func(t *testing.T) {
			w, fake, _ := setupWorkspaceWorker(t)
			root := t.TempDir()
			w.b.cfg.OMP = filepath.Join(root, "must-not-start")
			first := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Workspace: root, Session: filepath.Join(root, "missing.jsonl"), SessionID: "abcd0000-0000-4000-8000-000000000000"}
			second := first
			second.Chat = -20
			second.Thread = 33
			second.SessionID = "ABCD0000-0000-4000-8000-000000000000"
			for _, binding := range []store.Binding{first, second} {
				requireStoreOK(t, w.b.db.Save(binding))
				requireStoreOK(t, w.b.db.SetPinnedSession(binding.Bot, binding.Chat, binding.Thread, root, binding.SessionID, true))
				_, err := w.b.db.DB.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(?,?,?,?,?,?)", binding.Bot, binding.Chat, binding.Thread, root, binding.Session, binding.Generation)
				requireStoreOK(t, err)
			}
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, first.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			result := awaitSessionDelete(t, w)
			// A conflicting path published after the filesystem check must still block cleanup.
			second.Session = ""
			if pathState != "empty" {
				second.Session = filepath.Join(root, "other.jsonl")
			}
			if pathState == "existing" {
				requireStoreOK(t, os.WriteFile(second.Session, []byte("valid native history"), 0600))
			}
			requireStoreOK(t, w.b.db.Save(second))
			var historyBefore int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history").Scan(&historyBefore))
			w.sessionDeleteFinished(result)
			for _, expected := range []store.Binding{first, second} {
				saved, err := w.b.db.Binding(expected.Bot, expected.Chat, expected.Thread)
				if err != nil || !sameBindingIdentity(saved, expected) {
					t.Fatalf("conflicting path lost a binding: %+v, %v", saved, err)
				}
				pins, err := w.b.db.PinnedSessions(expected.Bot, expected.Chat, expected.Thread, root)
				requireStoreOK(t, err)
				if _, ok := pins[first.SessionID]; !ok {
					t.Fatal("conflicting path lost a pin")
				}
			}
			var historyAfter int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history").Scan(&historyAfter))
			if historyAfter != historyBefore {
				t.Fatal("conflicting path changed binding history")
			}
			if pathState == "existing" {
				if data, err := os.ReadFile(second.Session); err != nil || string(data) != "valid native history" {
					t.Fatalf("conflicting path changed native history: %q, %v", data, err)
				}
			}
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, first.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Cancel"))
			if w.b.sessionInUse(first.SessionID) || w.deleteCancel != nil {
				t.Fatal("conflicting path retained the delete reservation")
			}
		})
	}
}

func TestBindingSessionDeleteMissingFilePreservesReferencesOnRecheckOrCleanupFailure(t *testing.T) {
	for _, scenario := range []string{"restored", "lookup-error", "cleanup-error"} {
		t.Run(scenario, func(t *testing.T) {
			w, fake, _ := setupWorkspaceWorker(t)
			root := t.TempDir()
			w.b.cfg.OMP = filepath.Join(root, "must-not-start")
			binding := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Workspace: root, Session: filepath.Join(root, "sessions", "session.jsonl"), SessionID: "abcd0000-0000-4000-8000-000000000000"}
			requireStoreOK(t, w.b.db.Save(binding))
			requireStoreOK(t, w.b.db.SetPinnedSession(99, -10, 22, root, binding.SessionID, true))
			_, err := w.b.db.DB.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(?,?,?,?,?,?)", binding.Bot, binding.Chat, binding.Thread, root, binding.Session, binding.Generation)
			requireStoreOK(t, err)
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, binding.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			result := awaitSessionDelete(t, w)
			if !errors.Is(result.err, omp.ErrBoundSessionMissing) {
				t.Fatalf("expected initial missing-file result: %v", result.err)
			}
			switch scenario {
			case "restored":
				requireStoreOK(t, os.Mkdir(filepath.Dir(binding.Session), 0700))
				requireStoreOK(t, os.WriteFile(binding.Session, []byte("restored native history"), 0600))
			case "lookup-error":
				parent := filepath.Dir(binding.Session)
				requireStoreOK(t, os.Symlink(parent, parent))
			case "cleanup-error":
				_, err := w.b.db.DB.Exec("CREATE TRIGGER reject_reference_cleanup BEFORE DELETE ON bindings BEGIN SELECT RAISE(ABORT, 'cleanup failed'); END")
				requireStoreOK(t, err)
			}
			w.sessionDeleteFinished(result)
			saved, err := w.b.db.Binding(binding.Bot, binding.Chat, binding.Thread)
			if err != nil || !sameBindingIdentity(saved, binding) {
				t.Fatalf("failed missing-file cleanup changed binding: %+v, %v", saved, err)
			}
			pins, err := w.b.db.PinnedSessions(99, -10, 22, root)
			requireStoreOK(t, err)
			if _, ok := pins[binding.SessionID]; !ok {
				t.Fatal("failed missing-file cleanup removed pin")
			}
			var history int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=? AND chat=? AND thread=? AND session=?", binding.Bot, binding.Chat, binding.Thread, binding.Session).Scan(&history))
			if history != 1 {
				t.Fatal("failed missing-file cleanup removed history")
			}
			if scenario == "restored" {
				if data, err := os.ReadFile(binding.Session); err != nil || string(data) != "restored native history" {
					t.Fatalf("restored history changed: %q, %v", data, err)
				}
			}
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, binding.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Cancel"))
			if w.b.sessionInUse(binding.SessionID) || w.deleteCancel != nil {
				t.Fatal("failed missing-file cleanup retained delete reservation")
			}
		})
	}
}

func TestBindingSessionDeletePendingCloseCannotSurviveSourceReplacement(t *testing.T) {
	for _, action := range []string{"close", "new"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
			w, fake, command := setupWorkspaceWorker(t)
			w.b.slots = make(chan struct{}, 2)
			w.b.forgetRequests = make(chan bindingForgetRequest, 1)
			w.forgetResults = make(chan bindingForgetResult, 1)
			command("/new " + t.TempDir())
			peerCtx, stopPeer := context.WithCancel(w.ctx)
			peer := testWorker(t, &worker{b: w.b, key: target{chat: -10, thread: 22}, ctx: peerCtx, cancel: stopPeer, confirms: make(map[string]confirmation)})
			t.Cleanup(func() { peer.teardownWorker(true); stopPeer(); peer.background.Wait() })
			peer.start(false, t.TempDir(), "", false)
			if peer.client == nil {
				t.Fatal("target did not start")
			}
			bound := peer.binding
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, bound.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			request := <-w.b.forgetRequests
			if action == "close" {
				command("/close")
			} else {
				command("/new " + t.TempDir())
				clickSessionButton(w, fake, 7, bindingButton(t, fake, "Confirm"))
				drainControlOperations(t, w)
			}
			if !w.forgetPending {
				t.Fatal("source replacement released the pending reply slot early")
			}
			peer.forgetBinding(request)
			w.forgetFinished(<-w.forgetResults)
			if w.deleteCancel != nil || w.forgetPending || w.b.sessionInUse(bound.SessionID) {
				t.Fatal("stale closure reply started deletion or retained its reservation")
			}
			saved, err := w.b.db.Binding(bound.Bot, bound.Chat, bound.Thread)
			closed := bound
			closed.Running = false
			if err != nil || !sameBindingIdentity(saved, closed) {
				t.Fatalf("stale closure reply removed the closed binding: %+v, %v", saved, err)
			}
			if _, err := os.Stat(bound.Session); err != nil {
				t.Fatalf("stale closure reply removed native history: %v", err)
			}
			// A new explicit request must remain possible after the old reply is drained.
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, bound.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			result := awaitSessionDelete(t, w)
			requireStoreOK(t, result.err)
			w.sessionDeleteFinished(result)
			if _, err := w.b.db.Binding(bound.Bot, bound.Chat, bound.Thread); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("fresh explicit deletion failed: %v", err)
			}
		})
	}
}

func TestBindingSessionDeleteClosePreservesMissingReferencesButSettlesNativeSuccess(t *testing.T) {
	for _, outcome := range []string{"missing", "deleted"} {
		t.Run(outcome, func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
			root := t.TempDir()
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", root)
			w, fake, command := setupWorkspaceWorker(t)
			command("/new " + t.TempDir())
			bound := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Workspace: t.TempDir(), SessionID: "abcd0000-0000-4000-8000-000000000000"}
			bound.Session = filepath.Join(root, bound.SessionID+".jsonl")
			if outcome == "deleted" {
				header, err := json.Marshal(map[string]string{"type": "session", "id": bound.SessionID, "cwd": bound.Workspace})
				requireStoreOK(t, err)
				requireStoreOK(t, os.WriteFile(bound.Session, header, 0600))
			}
			requireStoreOK(t, w.b.db.Save(bound))
			requireStoreOK(t, w.b.db.SetPinnedSession(bound.Bot, bound.Chat, bound.Thread, bound.Workspace, bound.SessionID, true))
			_, err := w.b.db.DB.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(?,?,?,?,?,?)", bound.Bot, bound.Chat, bound.Thread, bound.Workspace, bound.Session, bound.Generation)
			requireStoreOK(t, err)
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, bound.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			result := awaitSessionDelete(t, w)
			if outcome == "deleted" {
				requireStoreOK(t, result.err)
			} else if !errors.Is(result.err, omp.ErrBoundSessionMissing) {
				t.Fatalf("expected actual missing-file result: %v", result.err)
			}
			operationContext := w.deleteContext
			command("/close")
			if operationContext.Err() != context.Canceled || !w.b.sessionInUse(bound.SessionID) {
				t.Fatal("close did not cancel deletion while retaining its reservation")
			}
			w.sessionDeleteFinished(result)
			saved, err := w.b.db.Binding(bound.Bot, bound.Chat, bound.Thread)
			if outcome == "missing" {
				if err != nil || !sameBindingIdentity(saved, bound) {
					t.Fatalf("cancelled missing-file cleanup changed binding: %+v, %v", saved, err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("confirmed native deletion was not settled after close: %v", err)
			}
			pins, err := w.b.db.PinnedSessions(bound.Bot, bound.Chat, bound.Thread, bound.Workspace)
			requireStoreOK(t, err)
			_, pinned := pins[bound.SessionID]
			var history int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=? AND chat=? AND thread=? AND session=?", bound.Bot, bound.Chat, bound.Thread, bound.Session).Scan(&history))
			if pinned != (outcome == "missing") || (history == 1) != (outcome == "missing") {
				t.Fatalf("incorrect settlement for %s: pinned=%t history=%d", outcome, pinned, history)
			}
			if w.deleteCancel != nil || w.deleteContext != nil || w.b.sessionInUse(bound.SessionID) {
				t.Fatal("delete settlement retained operation state or reservation")
			}
		})
	}
}

func TestBindingSessionDeleteMissingCancelledWhileWaitingForDatabase(t *testing.T) {
	w, fake, _ := setupWorkspaceWorker(t)
	root := t.TempDir()
	w.b.cfg.OMP = filepath.Join(root, "must-not-start")
	bound := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Workspace: root, Session: filepath.Join(root, "missing.jsonl"), SessionID: "abcd0000-0000-4000-8000-000000000000"}
	requireStoreOK(t, w.b.db.Save(bound))
	requireStoreOK(t, w.b.db.SetPinnedSession(bound.Bot, bound.Chat, bound.Thread, root, bound.SessionID, true))
	_, err := w.b.db.DB.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(?,?,?,?,?,?)", bound.Bot, bound.Chat, bound.Thread, root, bound.Session, bound.Generation)
	requireStoreOK(t, err)
	w.showBindings(7, true, 0)
	clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, bound.Thread))
	clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
	result := awaitSessionDelete(t, w)
	if !errors.Is(result.err, omp.ErrBoundSessionMissing) {
		t.Fatalf("expected actual missing-file result: %v", result.err)
	}
	cancel := w.deleteCancel
	conn, err := w.b.db.DB.Conn(context.Background())
	requireStoreOK(t, err)
	waitCount := w.b.db.DB.Stats().WaitCount
	messages := fake.messageCount()
	done := make(chan struct{})
	defer func() {
		conn.Close()
		<-done
	}()
	go func() {
		defer close(done)
		w.sessionDeleteFinished(result)
	}()
	waitFor(t, func() bool { return w.b.db.DB.Stats().WaitCount > waitCount })
	cancel()
	// Cancellation must finish settlement without waiting for the held connection.
	waitFor(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	requireStoreOK(t, conn.Close())
	saved, err := w.b.db.Binding(bound.Bot, bound.Chat, bound.Thread)
	if err != nil || !sameBindingIdentity(saved, bound) {
		t.Fatalf("cancelled database wait changed binding: %+v, %v", saved, err)
	}
	pins, err := w.b.db.PinnedSessions(bound.Bot, bound.Chat, bound.Thread, root)
	requireStoreOK(t, err)
	if _, ok := pins[bound.SessionID]; !ok {
		t.Fatal("cancelled database wait removed pin")
	}
	var history int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=? AND chat=? AND thread=? AND session=?", bound.Bot, bound.Chat, bound.Thread, bound.Session).Scan(&history))
	if history != 1 || fake.messageCount() != messages {
		t.Fatal("cancelled database wait changed history or reopened the menu")
	}
	if w.deleteCancel != nil || w.deleteContext != nil || w.b.sessionInUse(bound.SessionID) {
		t.Fatal("cancelled database wait retained operation state or reservation")
	}
}

func TestBindingSessionDeleteShutdownSettlesOnlyConfirmedSuccess(t *testing.T) {
	for _, outcome := range []string{"deleted", "missing", "fail", "deleted_bad_ack", "reference-failure"} {
		t.Run(outcome, func(t *testing.T) {
			mode := "success"
			if outcome == "fail" || outcome == "deleted_bad_ack" {
				mode = outcome
			}
			t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", mode)
			root := t.TempDir()
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", root)
			w, fake, _ := setupWorkspaceWorker(t)
			bound := store.Binding{Bot: 99, Chat: -10, Thread: 22, Generation: 1, Workspace: t.TempDir(), Session: filepath.Join(root, "abcd0000-0000-4000-8000-000000000000.jsonl"), SessionID: "abcd0000-0000-4000-8000-000000000000"}
			if outcome != "missing" {
				header, err := json.Marshal(map[string]string{"type": "session", "id": bound.SessionID, "cwd": bound.Workspace})
				requireStoreOK(t, err)
				requireStoreOK(t, os.WriteFile(bound.Session, header, 0600))
			}
			requireStoreOK(t, w.b.db.Save(bound))
			requireStoreOK(t, w.b.db.SetPinnedSession(bound.Bot, bound.Chat, bound.Thread, bound.Workspace, bound.SessionID, true))
			_, err := w.b.db.DB.Exec("INSERT INTO history(bot,chat,thread,workspace,session,generation) VALUES(?,?,?,?,?,?)", bound.Bot, bound.Chat, bound.Thread, bound.Workspace, bound.Session, bound.Generation)
			requireStoreOK(t, err)
			w.showBindings(7, true, 0)
			clickSessionButton(w, fake, 7, bindingEntryButton(t, fake, bound.Thread))
			clickSessionButton(w, fake, 7, bindingButton(t, fake, "Binding+Session"))
			result := awaitSessionDelete(t, w)
			w.background.Wait()
			switch outcome {
			case "deleted", "reference-failure":
				requireStoreOK(t, result.err)
			case "missing":
				if !errors.Is(result.err, omp.ErrBoundSessionMissing) {
					t.Fatalf("expected missing-file result: %v", result.err)
				}
			default:
				if result.err == nil {
					t.Fatal("unconfirmed native deletion returned success")
				}
			}
			if outcome == "reference-failure" {
				_, err := w.b.db.DB.Exec("CREATE TRIGGER reject_shutdown_cleanup BEFORE DELETE ON bindings BEGIN SELECT RAISE(ABORT, 'cleanup failed'); END")
				requireStoreOK(t, err)
			}
			messages := fake.messageCount()
			var repliesBefore int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&repliesBefore))
			w.deleteResults <- result
			w.cancel()
			w.drainDeleteResults()
			preserved := outcome != "deleted"
			saved, err := w.b.db.Binding(bound.Bot, bound.Chat, bound.Thread)
			if preserved {
				if err != nil || !sameBindingIdentity(saved, bound) {
					t.Fatalf("shutdown changed unconfirmed references: %+v, %v", saved, err)
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("shutdown did not settle confirmed deletion: %v", err)
			}
			pins, err := w.b.db.PinnedSessions(bound.Bot, bound.Chat, bound.Thread, bound.Workspace)
			requireStoreOK(t, err)
			_, pinned := pins[bound.SessionID]
			wantHistory := 0
			if preserved {
				wantHistory = 1
			}
			var history, replies int
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM history WHERE bot=? AND session=?", bound.Bot, bound.Session).Scan(&history))
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&replies))
			if pinned != preserved || history != wantHistory {
				t.Fatalf("incorrect shutdown settlement: pinned=%t history=%d", pinned, history)
			}
			if replies != repliesBefore || fake.messageCount() != messages {
				t.Fatal("shutdown sent a notice or reopened the menu")
			}
			if w.deleteCancel != nil || w.deleteContext != nil || w.b.sessionInUse(bound.SessionID) {
				t.Fatal("shutdown retained deletion state or reservation")
			}
		})
	}
}
