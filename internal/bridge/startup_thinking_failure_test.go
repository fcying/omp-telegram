package bridge

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omp-telegram/internal/store"
)

func TestStartupThinkingFailureOutcomes(t *testing.T) {
	for _, scenario := range []string{"new", "replace-new", "resume", "lazy-resume"} {
		for _, mode := range []string{"reject", "missing", "malformed", "hold", "unknown-ack"} {
			t.Run(scenario+"/"+mode, func(t *testing.T) {
				t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT", t.TempDir())
				t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", "")
				t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_AFTER", "1")
				trace := filepath.Join(t.TempDir(), "rpc.trace")
				t.Setenv("OMP_TELEGRAM_FIXTURE_RPC_TRACE", trace)
				w, f, command := setupWorkspaceWorker(t)
				var before store.Binding
				savedFiles := make(map[string][]byte)
				saveFile := func(path string) {
					t.Helper()
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					savedFiles[path] = data
				}
				initialQueries := 0
				resumeID := ""
				if scenario != "new" {
					command("/new " + t.TempDir())
					before = w.binding
					if w.client == nil || !before.Running {
						t.Fatal("initial session did not start")
					}
					saveFile(before.Session)
					initialQueries = 1
					if scenario == "resume" {
						selected := setResumeFixtures(t, before.Workspace, 1)[0]
						resumeID = selected.ID
						saveFile(filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), selected.ID+".jsonl"))
					}
					if scenario == "lazy-resume" {
						w.releaseRuntime(true)
					}
				}
				// Change the environment before the attempted child starts; the old
				// process must not be expected to observe these failure settings.
				t.Setenv("OMP_TELEGRAM_FIXTURE_EFFORT_MODE", mode)
				switch scenario {
				case "new":
					command("/new " + t.TempDir())
				case "replace-new":
					command("/new " + t.TempDir())
					clickKeyboard(w, 7, resumeButtons(t, f)[0]["callback_data"].(string))
				case "resume":
					command("/resume " + resumeID)
				case "lazy-resume":
					command("/thinking")
				}

				stopped := mode == "hold" || mode == "unknown-ack"
				if stopped {
					if w.client != nil || w.runtime != runtimeReleased || len(w.b.slots) != 0 || w.sessionRuntimeStarting.Load() || w.sessionRuntimeActive.Load() {
						t.Fatal("failed thinking query retained a started runtime or capacity slot")
					}
					if len(w.b.sessionClaims) != 0 || w.claimedSession != "" || w.sessionID != "" {
						t.Fatal("failed thinking query retained a session claim")
					}
					notices := nativeOutputs(t, w)
					if !strings.Contains(notices, "Cannot query omp thinking configuration") || strings.Contains(notices, "Cannot register Telegram attachment delivery") {
						t.Fatalf("startup failure identified the wrong stage: %q", notices)
					}
					stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
					if scenario == "new" {
						if !errors.Is(err, sql.ErrNoRows) {
							t.Fatalf("failed new session committed a binding: %+v, %v", stored, err)
						}
					} else {
						want := before
						want.Running = scenario == "lazy-resume"
						if err != nil || stored != want || w.binding != want {
							t.Fatalf("failed start changed saved binding: stored=%+v worker=%+v want=%+v error=%v", stored, w.binding, want, err)
						}
					}
				} else {
					if w.client == nil || w.client.Stopping() || w.runtime != runtimeConnected || len(w.b.slots) != 1 || !w.binding.Running {
						t.Fatal("usable thinking-query failure prevented startup")
					}
					if w.configuredThinking != "" {
						t.Fatalf("failed query retained configured thinking: %q", w.configuredThinking)
					}
					if scenario == "lazy-resume" && !sameBindingIdentity(w.binding, before) {
						t.Fatal("lazy resume changed the saved session identity")
					}
					if scenario == "resume" && w.binding.SessionID != resumeID {
						t.Fatal("explicit resume did not bind the selected session")
					}
					stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
					if err != nil || stored != w.binding {
						t.Fatalf("successful startup did not commit its binding: %+v, %v", stored, err)
					}
					command("/status")
					if fields := statusFields(nativeOutputs(t, w)); fields["Thinking"] != "n/a" {
						t.Fatalf("failed optional query exposed effective thinking as configured: %v", fields)
					}
				}

				if w.startIntent != nil {
					t.Fatal("settled startup retained its in-memory startup intent")
				}
				intents, err := w.b.db.PendingStarts(w.b.bot.ID)
				if err != nil || len(intents) != 0 {
					t.Fatalf("settled startup retained a durable startup intent: %+v, %v", intents, err)
				}
				for path, want := range savedFiles {
					got, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("startup changed a saved session file %q: %v", path, err)
					}
				}
				if notices := nativeOutputs(t, w); strings.Contains(notices, "PRIVATE") {
					t.Fatalf("startup exposed private RPC diagnostics: %q", notices)
				}
				queries, failures, registrations := 0, 0, 0
				for _, entry := range fixtureRPCTrace(trace) {
					switch entry.kind {
					case "effort_query":
						queries++
					case "effort_failure":
						if entry.payload != mode {
							t.Fatalf("fixture failed with the wrong mode: %q", entry.payload)
						}
						failures++
					case "set_host_tools":
						registrations++
					case "prompt":
						if entry.payload != "/session info" {
							t.Fatalf("startup submitted or replayed model work: %q", entry.payload)
						}
					}
				}
				wantRegistrations := initialQueries
				if !stopped {
					wantRegistrations++
				}
				if queries != initialQueries+1 || failures != 1 || registrations != wantRegistrations {
					t.Fatalf("startup retried the query or registered tools at the wrong stage: queries=%d failures=%d registrations=%d want=%d", queries, failures, registrations, wantRegistrations)
				}
			})
		}
	}
}
