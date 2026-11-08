package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/i18n"
)

func TestStartupUIHintsPreserveRecoveryState(t *testing.T) {
	cases := []struct {
		language    i18n.Language
		entry, mode string
	}{
		{i18n.English, "new", "confirm"},
		{i18n.Chinese, "new", "confirm"},
		{i18n.English, "new", "select"},
		{i18n.Chinese, "new", "select"},
		{i18n.English, "resume", "confirm"},
		{i18n.English, "lazy", "confirm"},
		{i18n.English, "new", "cancel"},
		{i18n.English, "new", "plain"},
		{i18n.English, "new", "reject"},
		{i18n.English, "new", "disconnect"},
	}
	for _, test := range cases {
		language, entry, mode := test.language, test.entry, test.mode
		t.Run(fmt.Sprintf("%s/%s/%s", language, entry, mode), func(t *testing.T) {
			t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
			trace := filepath.Join(t.TempDir(), "rpc.trace")
			argv := filepath.Join(t.TempDir(), "argv.trace")
			t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
			t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", argv)
			capture := &logCapture{}
			w, command := setupLoggingWorker(t, capture, "json")
			workspace := t.TempDir()
			before := w.binding
			launches := 1
			if entry != "new" {
				command("/new " + workspace)
				before = w.binding
				launches++
				if entry == "lazy" {
					w.releaseRuntime(true)
				} else {
					command("/close")
					before.Running = false
				}
			}
			w.b.setLanguage(language)
			t.Setenv("OMP_TELEGRAM_FIXTURE_STARTUP_UI", mode)
			// Only this worker's test context shortens startup; production deadlines stay unchanged.
			parentCtx := w.ctx
			ctx, cancel := context.WithTimeout(w.ctx, 1500*time.Millisecond)
			defer cancel()
			w.ctx = ctx
			switch entry {
			case "new":
				command("/new " + workspace)
			case "resume":
				command("/resume " + before.SessionID)
			case "lazy":
				command("/name must-not-be-submitted")
			}

			var text string
			requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text))
			reason := ""
			var hint string
			switch mode {
			case "confirm":
				reason, hint = "startup_timeout_pending_confirm", w.tr(i18n.Msg.StartupTimeoutPendingConfirm)
			case "select":
				reason, hint = "startup_timeout_pending_ui", w.tr(i18n.Msg.StartupTimeoutPendingUI)
			}
			var want string
			if hint != "" {
				want = hint + "\n" + w.tr(i18n.Msg.StartupOutcomeUnconfirmedUseClose)
				if entry == "lazy" {
					want = hint + "\n" + w.tr(i18n.Msg.FailedResumeReleasedOMPSessionNoPromptSubmitted)
				}
			} else {
				switch entry {
				case "new":
					want = w.tr(i18n.Msg.FailedStartOmpStartupOutcomeUncertainUseCloseBefore)
				case "resume":
					want = w.tr(i18n.Msg.FailedResumeOmpStartupOutcomeUncertainUseCloseBefore)
				case "lazy":
					want = w.tr(i18n.Msg.FailedResumeReleasedOMPSessionNoPromptSubmitted)
				}
			}
			if text != want {
				t.Fatalf("startup notice = %q, want %q", text, want)
			}
			if entry == "lazy" && strings.Contains(text, "/close") || entry != "lazy" && !strings.Contains(text, "/close") {
				t.Fatalf("startup notice changed entry-specific recovery: %q", text)
			}
			if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 || w.sessionRuntimeStarting.Load() || w.sessionRuntimeActive.Load() || len(w.b.sessionClaims) != 0 || w.claimedSession != "" || w.sessionID != "" {
				t.Fatal("failed startup retained a process, slot, starting flag or session claim")
			}
			intents, err := w.b.db.PendingStarts(w.b.bot.ID)
			requireStoreOK(t, err)
			if entry == "lazy" {
				if w.startIntent != nil || len(intents) != 0 {
					t.Fatal("lazy resume created an uncertain startup intent")
				}
			} else if w.startIntent == nil || len(intents) != 1 || intents[0] != *w.startIntent || w.startIntent.Kind != entry || w.startIntent.Generation != before.Generation+1 {
				t.Fatalf("failed startup lost or confirmed its intent: %+v, %+v", w.startIntent, intents)
			}
			stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
			if entry == "new" {
				if err != sql.ErrNoRows {
					t.Fatalf("unconfirmed new startup acquired a binding: %+v, %v", stored, err)
				}
			} else if err != nil || stored != before || w.binding != before {
				t.Fatalf("startup failure changed existing binding: stored=%+v worker=%+v before=%+v error=%v", stored, w.binding, before, err)
			}
			if len(w.queue) != 0 || w.taskActive() {
				t.Fatal("startup failure accepted model work")
			}
			if got := readStringArgvTrace(t, argv); len(got) != launches {
				t.Fatalf("startup automatically retried: %v", got)
			}
			var startupCalls []string
			waiting := false
			for _, event := range fixtureRPCTrace(trace) {
				if event.kind == "startup_rpc" {
					startupCalls = append(startupCalls, event.payload)
				}
				if event.kind == "startup_waiting" && event.payload == mode {
					waiting = true
				}
			}
			if !waiting || strings.Join(startupCalls, ",") != "set_ask_dialog,negotiate_protocol" {
				t.Fatalf("startup skipped negotiation, submitted a prompt/answer or downgraded: %v, waiting=%t", startupCalls, waiting)
			}
			record := capturedEvent(t, capture, "runtime_start_failed")
			generation := before.Generation
			if entry != "lazy" {
				generation++
			}
			if record == nil || record["component"] != "bridge" || record["level"] != "ERROR" || record["chat_id"] != float64(w.key.chat) || record["thread_id"] != float64(w.key.thread) || record["generation"] != float64(generation) {
				t.Fatalf("startup failure omitted lifecycle correlation: %+v", record)
			}
			if reason != "" && record["reason"] != reason || (mode == "cancel" || mode == "plain") && record["reason"] != "timeout" {
				t.Fatalf("startup failure reason = %+v", record)
			}
			if hint == "" && strings.HasPrefix(fmt.Sprint(record["reason"]), "startup_timeout_pending_") {
				t.Fatalf("non-timeout/cancelled UI borrowed stale evidence: %+v", record)
			}
			for _, secret := range []string{"PRIVATE_STARTUP", "must-not-be-submitted", workspace, "targetId", "options", "default", "\"title\"", "\"value\""} {
				if strings.Contains(capture.String(), secret) || strings.Contains(text, secret) {
					t.Fatalf("startup failure exposed private UI data %q", secret)
				}
			}
			if entry != "lazy" {
				w.ctx = parentCtx
				command("/new " + workspace)
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text))
				if text != w.tr(i18n.Msg.PreviousSessionStartStillUncertainUseCloseBeforeStarting) || len(readStringArgvTrace(t, argv)) != launches {
					t.Fatal("unconfirmed startup allowed retry before /close")
				}
				command("/close")
				intents, err = w.b.db.PendingStarts(w.b.bot.ID)
				requireStoreOK(t, err)
				if w.startIntent != nil || len(intents) != 0 || w.binding.Running || len(w.b.slots) != 0 || len(readStringArgvTrace(t, argv)) != launches {
					t.Fatal("/close did not clear startup uncertainty without launching OMP")
				}
			}
		})
	}
}

func TestRuntimeStartupContextCancellationDoesNotLogFailure(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_STARTUP_UI", "confirm")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
	trace := filepath.Join(t.TempDir(), "rpc.trace")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
	capture := &logCapture{}
	w, _ := setupLoggingWorker(t, capture, "json")
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	w.ctx = ctx
	workspace := t.TempDir()
	done := make(chan string, 1)
	go func() { done <- w.start(false, workspace, "", false) }()
	waitFixtureRPCTrace(t, trace, "startup_waiting", "confirm")
	cancel()
	select {
	case failure := <-done:
		if failure != w.tr(i18n.Msg.FailedStartOmpStartupOutcomeUncertainUseCloseBefore) {
			t.Fatalf("normal cancellation borrowed timeout evidence: %q", failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled startup did not stop")
	}
	for _, event := range []string{"runtime_start_failed", "rpc_startup_failed", "restore_runtime_failed"} {
		if record := capturedEvent(t, capture, event); record != nil {
			t.Fatalf("normal context cancellation logged a startup failure: %+v", record)
		}
	}
	if len(w.b.slots) != 0 || w.client != nil || w.runtime != runtimeReleased || w.sessionRuntimeStarting.Load() || w.sessionRuntimeActive.Load() {
		t.Fatal("cancelled startup retained runtime resources")
	}
}
