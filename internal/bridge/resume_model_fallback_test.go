package bridge

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"omp-telegram/internal/i18n"
)

func TestResumeModelFallbackPreservesSessionAndPendingWork(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		for _, unavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("lazy=%t/unavailable=%t", lazy, unavailable), func(t *testing.T) {
				t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
				t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", "")
				trace := filepath.Join(t.TempDir(), "rpc.trace")
				argvTrace := filepath.Join(t.TempDir(), "argv.trace")
				t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
				t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", argvTrace)
				w, _, command := setupWorkspaceWorker(t)
				w.b.cfg.QueueCapacity = 4
				command("/new " + t.TempDir())
				command("historical prompt")
				w.dispatch()
				nativeUntil(t, w, func() bool { return !w.taskActive() && len(w.queue) == 0 })
				before, oldClient := w.binding, w.client
				original, err := os.ReadFile(before.Session)
				if err != nil {
					t.Fatal(err)
				}
				if lazy {
					command("/followup first preserved followup")
					command("/followup second preserved followup")
					w.releaseRuntime(true)
					w.b.setLanguage(i18n.Chinese)
				} else {
					command("/close")
				}
				pending := append([]queued(nil), w.queue...)
				if unavailable {
					t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", "missing")
				}
				if lazy {
					if _, err := w.ensureRuntime(); err != nil {
						t.Fatal(err)
					}
				} else {
					command("/resume " + before.SessionID)
				}
				if w.client == nil || w.client == oldClient || w.runtime != runtimeConnected || !w.binding.Running ||
					w.binding.Session != before.Session || w.binding.SessionID != before.SessionID || w.binding.Workspace != before.Workspace {
					t.Fatalf("resume changed native identity or failed to connect: before=%+v after=%+v", before, w.binding)
				}
				if w.client.UsedDefaultModelFallback() != unavailable || len(w.queue) != len(pending) || len(pending) > 0 && !reflect.DeepEqual(w.queue, pending) {
					t.Fatal("resume changed pending work or reported an incorrect fallback outcome")
				}
				stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
				if err != nil || stored != w.binding || w.startIntent != nil {
					t.Fatalf("restored runtime did not have a committed binding: %+v, %v", stored, err)
				}
				if lazy && w.binding.Generation != before.Generation {
					t.Fatal("lazy fallback changed the binding generation")
				}
				notice := w.tr(i18n.Msg.Session.DefaultModelFallback)
				wantNotices := 0
				if unavailable {
					wantNotices = 1
				}
				if output := nativeOutputs(t, w); strings.Count(output, notice) != wantNotices || strings.Contains(output, "PRIVATE") {
					t.Fatalf("resume emitted the wrong fallback notice: %q", output)
				}
				launches := readStringArgvTrace(t, argvTrace)
				wantLaunches := 2
				if unavailable {
					wantLaunches++
				}
				if len(launches) != wantLaunches {
					t.Fatalf("resume attempted the wrong number of launches: %v", launches)
				}
				if !lazy {
					command("/followup first preserved followup")
					command("/followup second preserved followup")
				}
				w.dispatch()
				nativeUntil(t, w, func() bool { return !w.taskActive() && len(w.queue) == 0 })
				for _, text := range []string{"historical prompt", "first preserved followup", "second preserved followup"} {
					if count := researchTraceCount(trace, "prompt", text); count != 1 {
						t.Fatalf("resume lost or replayed model work %q: count=%d", text, count)
					}
				}
				if got, err := os.ReadFile(before.Session); err != nil || !bytes.Equal(got, original) {
					t.Fatalf("fallback rewrote the saved session: %v", err)
				}
			})
		}
	}
}

func TestResumeModelFallbackFailureKeepsStartupCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name string
		lazy bool
	}{
		{name: "explicit"},
		{name: "lazy", lazy: true},
	} {
		for _, failure := range []string{"fail", "identity", "host-tools", "commit"} {
			if scenario.lazy && failure == "commit" {
				continue // Lazy restoration preserves the existing committed binding.
			}
			t.Run(scenario.name+"/"+failure, func(t *testing.T) {
				t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
				t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", "")
				argvTrace := filepath.Join(t.TempDir(), "argv.trace")
				trace := filepath.Join(t.TempDir(), "rpc.trace")
				t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", argvTrace)
				t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
				w, _, command := setupWorkspaceWorker(t)
				w.b.cfg.QueueCapacity = 2
				command("/new " + t.TempDir())
				before := w.binding
				if scenario.lazy {
					command("/followup preserved pending work")
					w.releaseRuntime(true)
				} else {
					command("/close")
					before.Running = false
				}
				pending := append([]queued(nil), w.queue...)
				t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", "missing")
				switch failure {
				case "fail", "identity":
					t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", failure)
				case "host-tools":
					t.Setenv("OMP_TELEGRAM_FIXTURE_REJECT_RPC", "set_host_tools")
				case "commit":
					_, err := w.b.db.DB.Exec("CREATE TRIGGER reject_fallback_commit BEFORE INSERT ON bindings BEGIN SELECT RAISE(FAIL, 'injected startup commit failure'); END")
					requireStoreOK(t, err)
				}
				if scenario.lazy {
					if _, err := w.ensureRuntime(); err == nil {
						t.Fatal("failed lazy fallback reported a successful restore")
					}
				} else {
					command("/resume " + before.SessionID)
				}
				if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 || w.sessionRuntimeStarting.Load() || w.sessionRuntimeActive.Load() ||
					len(w.b.sessionClaims) != 0 || w.claimedSession != "" || w.sessionID != "" {
					t.Fatal("failed fallback retained a process, slot or session claim")
				}
				stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
				if err != nil || stored != before || w.binding != before || len(w.queue) != len(pending) || len(pending) > 0 && !reflect.DeepEqual(w.queue, pending) {
					t.Fatalf("failed fallback changed the saved binding or pending work: before=%+v stored=%+v worker=%+v error=%v", before, stored, w.binding, err)
				}
				intents, err := w.b.db.PendingStarts(w.b.bot.ID)
				if err != nil {
					t.Fatal(err)
				}
				if failure == "fail" && !scenario.lazy {
					if w.startIntent == nil || len(intents) != 1 || intents[0] != *w.startIntent {
						t.Fatal("failed native fallback discarded the uncertain startup intent")
					}
				} else if w.startIntent != nil || len(intents) != 0 {
					t.Fatal("validated startup failure retained a settled startup intent")
				}
				if output := nativeOutputs(t, w); strings.Contains(output, w.tr(i18n.Msg.Session.DefaultModelFallback)) || strings.Contains(output, "PRIVATE") {
					t.Fatalf("failed fallback emitted a success notice or private diagnostic: %q", output)
				}
				if launches := readStringArgvTrace(t, argvTrace); len(launches) != 3 {
					t.Fatalf("failed fallback was not attempted exactly once: %v", launches)
				}
				for _, entry := range fixtureRPCTrace(trace) {
					if entry.kind == "prompt" && entry.payload != "/session info" {
						t.Fatalf("failed fallback submitted model work: %q", entry.payload)
					}
				}
			})
		}
	}
}

func TestResearchControlResumeModelFallbackDoesNotAnnounceUnvalidatedScope(t *testing.T) {
	w, _, _ := researchWorker(t, "autoresearch")
	before := w.binding
	w.releaseRuntime(true)
	output := nativeOutputs(t, w)
	t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", "missing")
	client, err := w.prepareResearchControlRuntime()
	if err != nil || client == nil || !client.UsedDefaultModelFallback() {
		t.Fatalf("research control could not restore the saved session with the native default: %v", err)
	}
	if w.binding != before || nativeOutputs(t, w) != output {
		t.Fatal("research control announced fallback or changed the binding before validating its scope")
	}
}

func TestResearchControlResumeModelFallbackNoticeAfterValidation(t *testing.T) {
	for _, mode := range []string{"missing", "identity"} {
		t.Run(mode, func(t *testing.T) {
			w, _, _ := researchWorker(t, "autoresearch")
			w.releaseRuntime(true)
			t.Setenv("OMP_TELEGRAM_FIXTURE_RESUME_MODEL", mode)
			nativeInput(t, w, 100, "/autoresearch off", false)
			nativeUntil(t, w, func() bool {
				state := queueState(t, w, 100)
				return state != "pending" && state != "submitted" && !w.controlInProgress()
			})
			wantState, wantNotices := "done", 1
			if mode == "identity" {
				wantState, wantNotices = "uncertain", 0
			}
			notice := w.tr(i18n.Msg.Session.DefaultModelFallback)
			if state := queueState(t, w, 100); state != wantState {
				t.Fatalf("control state = %s, want %s", state, wantState)
			}
			if got := strings.Count(nativeOutputs(t, w), notice); got != wantNotices {
				t.Fatalf("fallback notices = %d, want %d", got, wantNotices)
			}
			if mode == "identity" {
				return
			}
			client := w.client
			if client == nil || !client.UsedDefaultModelFallback() {
				t.Fatal("research control did not restore with the default model")
			}
			nativeInput(t, w, 101, "/autoresearch off", false)
			nativeUntil(t, w, func() bool { return queueState(t, w, 101) == "done" && !w.controlInProgress() })
			if w.client != client || strings.Count(nativeOutputs(t, w), notice) != 1 {
				t.Fatal("reused research runtime repeated the model fallback notice")
			}
		})
	}
}
