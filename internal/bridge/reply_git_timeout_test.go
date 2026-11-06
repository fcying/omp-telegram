package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
)

type gitTimeoutGate struct {
	started string
	release string
	body    string
}

func newGitTimeoutGate(t *testing.T) gitTimeoutGate {
	t.Helper()
	root := t.TempDir()
	gate := gitTimeoutGate{started: filepath.Join(root, "started"), release: filepath.Join(root, "release")}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	gate.body = "printf started > " + quote(gate.started) + "\nwhile [ ! -f " + quote(gate.release) + " ]; do sleep 0.01; done\n"
	t.Cleanup(func() { requireStoreOK(t, os.WriteFile(gate.release, nil, 0600)) })
	return gate
}

func (gate gitTimeoutGate) open(t *testing.T) {
	t.Helper()
	requireStoreOK(t, os.WriteFile(gate.release, nil, 0600))
}

func (gate gitTimeoutGate) ready() bool {
	_, err := os.Stat(gate.started)
	return err == nil
}

// Delay the first real Git invocation without replacing any probe output. All
// subsequent calls still execute the real binary against an isolated repository.
func (gate gitTimeoutGate) builtin(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	requireStoreOK(t, err)
	root := t.TempDir()
	body := "#!/bin/sh\nif [ ! -f '" + gate.started + "' ]; then\n" + gate.body + "fi\nexec '" + git + "' \"$@\"\n"
	requireStoreOK(t, os.WriteFile(filepath.Join(root, "git"), []byte(body), 0700))
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func gitTimeoutIsolateWorker(t *testing.T, w *worker) replyGitRepository {
	t.Helper()
	repo := newReplyGitRepository(t, true)
	w.binding.Workspace = repo.path
	for _, entry := range repo.env {
		name, value, _ := strings.Cut(entry, "=")
		if name == "HOME" || name == "XDG_CONFIG_HOME" || name == "GIT_CONFIG_GLOBAL" || name == "GIT_CONFIG_SYSTEM" {
			t.Setenv(name, value)
		}
	}
	return repo
}

// Research's normal five-second fixture pump is deliberately too short for
// these deadline-boundary regressions. Keep processing the same actor inputs.
func gitTimeoutUntil(t *testing.T, w *worker, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		var events <-chan json.RawMessage
		if w.client != nil {
			events = w.client.Events()
		}
		select {
		case result := <-w.nativeCatalog.results:
			w.commandCatalogFinished(result)
		case result := <-w.operations:
			w.operationReturned(result)
		case raw, ok := <-events:
			if !ok {
				w.failed()
			} else {
				w.event(raw)
			}
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("Git timeout actor did not reach the expected state")
		}
		w.dispatch()
	}
}

func TestGitTimeoutBuiltinConfiguredBudget(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo is unavailable")
	}
	for _, timeout := range []time.Duration{70 * time.Millisecond, 700 * time.Millisecond, 0} {
		t.Run(timeout.String(), func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			head := filepath.Join(repo.path, ".git", "HEAD")
			requireStoreOK(t, os.Remove(head))
			output, err := exec.Command(mkfifo, head).CombinedOutput()
			if err != nil {
				t.Fatalf("create blocking HEAD: %v, %s", err, output)
			}
			budget := timeout
			if budget <= 0 {
				budget = config.DefaultGitTimeout
			}
			began := time.Now()
			got := readReplyGit(context.Background(), repo.path, repo.env, timeout)
			elapsed := time.Since(began)
			if got != "" || elapsed < budget/2 || elapsed > budget+time.Second {
				t.Fatalf("blocked builtin returned %q after %s with budget %s", got, elapsed, budget)
			}
			if timeout > config.DefaultGitTimeout && elapsed <= config.DefaultGitTimeout {
				t.Fatalf("configured builtin budget was capped at the old default: %s", elapsed)
			}
		})
	}
}

func TestGitTimeoutCustomBudgetAndCallerCancellation(t *testing.T) {
	for _, scenario := range []string{"short", "long", "zero", "caller-cancel"} {
		t.Run(scenario, func(t *testing.T) {
			repo := newReplyGitRepository(t, true)
			gate := newGitTimeoutGate(t)
			script := replyGitCommandScript(t, gate.body+"printf custom")
			timeout := 70 * time.Millisecond
			switch scenario {
			case "long", "caller-cancel":
				timeout = 3 * time.Second
			case "zero":
				timeout = 0
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan string, 1)
			began := time.Now()
			go func() { result <- readReplyGitField(ctx, repo.path, repo.env, []string{script}, timeout) }()
			if scenario == "long" || scenario == "caller-cancel" {
				waitFor(t, gate.ready)
				if scenario == "long" {
					time.Sleep(config.DefaultGitTimeout + 150*time.Millisecond)
					gate.open(t)
				} else {
					cancel()
				}
			}
			select {
			case got := <-result:
				want := ""
				if scenario == "long" {
					want = "custom"
				}
				elapsed := time.Since(began)
				if got != want || elapsed > 2*time.Second {
					t.Fatalf("custom result = %q after %s, want %q without builtin fallback", got, elapsed, want)
				}
				budget := timeout
				if budget <= 0 {
					budget = config.DefaultGitTimeout
				}
				if scenario != "long" && scenario != "caller-cancel" && elapsed < budget/2 {
					t.Fatalf("custom command skipped its configured deadline: %s", elapsed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("custom command ignored its timeout or caller cancellation")
			}
		})
	}
}

func TestGitTimeoutOrdinaryLongGitKeepsNativeLimitsAndBodyDelivery(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			w, f, _, trace := replyStatusWorker(t)
			gitTimeoutIsolateWorker(t, w)
			gate := newGitTimeoutGate(t)
			w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git", "context", "quota"}, GitTimeout: 4 * time.Second}
			phase := "get_state"
			want := "main"
			if custom {
				phase = "get_logout_accounts"
				want = "custom"
				w.b.cfg.StatusBar.GitCommand = []string{replyGitCommandScript(t, gate.body+"printf custom")}
				nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
			} else {
				gate.builtin(t)
			}
			nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": phase})
			began := time.Now()
			replyStatusMessage(t, w, 100, 1000, 1100)
			endReplyStatus(w)
			waitFor(t, gate.ready)
			waitFixtureRPCTrace(t, trace, "research_control_waiting", phase)
			if w.replyStatus.pending == nil {
				t.Fatal("Git sampling did not remain asynchronous")
			}
			deliverReplies(t, w)
			if f.messageCount() == 0 {
				t.Fatal("confirmed body was not deliverable while Git was pending")
			}
			time.Sleep(1200 * time.Millisecond)
			gate.open(t)
			result := receiveReplyStatus(t, w)
			sample := result.meta.(replyStatusResult)
			if sample.git != want || custom && sample.account != ([32]byte{}) || !custom && sample.valid || time.Since(began) > 2500*time.Millisecond {
				t.Fatalf("Git/native budgets were not independent: git=%q valid=%t account=%x elapsed=%s", sample.git, sample.valid, sample.account, time.Since(began))
			}
			w.operationReturned(result)
			if header := finalReplyStatus(t, w, 2, "answer"); !strings.Contains(header, "`"+want+"`") || queueState(t, w, 2) != "done" {
				t.Fatalf("long Git sample did not attach to the intact completed body: %q", header)
			}
			nativeRPC(t, w, "fixture_research_control_release", nil)
		})
	}
}

func TestGitTimeoutResearchLongGitAndIndependentAccountLimit(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%t", custom), func(t *testing.T) {
			w, _, trace := researchWorker(t, "autoresearch")
			w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git"}, GitTimeout: 8 * time.Second}
			researchRoot(t, w, 100, "/autoresearch long Git budget")
			gitTimeoutIsolateWorker(t, w)
			gate := newGitTimeoutGate(t)
			want := "main"
			if custom {
				want = "custom"
				w.b.cfg.StatusBar.Final = []string{"git", "quota"}
				w.b.cfg.StatusBar.GitCommand = []string{replyGitCommandScript(t, gate.body+"printf custom")}
				nativeRPC(t, w, "set_model", map[string]any{"provider": "openai-codex", "modelId": "safe"})
				nativeRPC(t, w, "fixture_research_control_hold", map[string]any{"phase": "get_logout_accounts"})
			} else {
				gate.builtin(t)
			}
			began := time.Now()
			researchFinishNaturally(t, w, "research answer")
			gitTimeoutUntil(t, w, 7*time.Second, gate.ready)
			if custom {
				waitFixtureRPCTrace(t, trace, "research_control_waiting", "get_logout_accounts")
				if elapsed := time.Since(began); elapsed < 4500*time.Millisecond || elapsed > 7*time.Second {
					t.Fatalf("research account lookup lost its independent five-second bound: %s", elapsed)
				}
				nativeRPC(t, w, "fixture_research_control_release", nil)
			}
			if rows := researchReplies(t, w, 100); len(rows) != 1 || rows[0] != "research answer" || queueState(t, w, 100) != "submitted" {
				t.Fatalf("pending research Git changed or withheld the durable body: %q", rows)
			}
			deliverReplies(t, w)
			// Only this real delay proves that Git survives the former five-second cap.
			time.Sleep(5200 * time.Millisecond)
			gate.open(t)
			gitTimeoutUntil(t, w, 4*time.Second, func() bool { return queueState(t, w, 100) == "done" })
			rows := researchReplies(t, w, 100)
			if len(rows) != 1 || replyBody(t, rows[0]) != "research answer" || !strings.Contains(rows[0], "`"+want+"`") {
				t.Fatalf("research did not preserve the long Git snapshot and single body: %q", rows)
			}
		})
	}
}

func TestGitTimeoutResearchNativeLookupRemainsFiveSeconds(t *testing.T) {
	w, _, trace := researchWorker(t, "autoresearch")
	w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git"}, GitTimeout: 20 * time.Second}
	researchRoot(t, w, 100, "/autoresearch bounded native evidence")
	gate := newGitTimeoutGate(t)
	w.b.cfg.StatusBar.GitCommand = []string{replyGitCommandScript(t, gate.body+"printf custom")}
	nativeRPC(t, w, "fixture_autoresearch_metadata", map[string]any{"entriesMode": "hold"})
	began := time.Now()
	researchFinishNaturally(t, w, "durable answer")
	gitTimeoutUntil(t, w, 7*time.Second, func() bool { return queueState(t, w, 100) == "uncertain" })
	if elapsed := time.Since(began); elapsed < 4500*time.Millisecond || elapsed > 7*time.Second {
		t.Fatalf("research native query lost its five-second deadline: %s", elapsed)
	}
	waitFixtureRPCTrace(t, trace, "entries_waiting", "")
	if gate.ready() || !w.resumeFailed {
		t.Fatal("unconfirmed native settlement started Git or completed the root")
	}
	var bodies int
	requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=100 AND text='durable answer'").Scan(&bodies))
	if bodies != 1 {
		t.Fatalf("native deadline lost or duplicated the durable research body: %d", bodies)
	}
}

func TestGitTimeoutPendingGitDoesNotBlockControls(t *testing.T) {
	for _, research := range []bool{false, true} {
		for _, control := range []string{"/stop", "/close"} {
			t.Run(fmt.Sprintf("research=%t/%s", research, control), func(t *testing.T) {
				var w *worker
				id := int64(2)
				if research {
					w, _, _ = researchWorker(t, "autoresearch")
					w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git"}, GitTimeout: 20 * time.Second}
					researchRoot(t, w, 100, "/autoresearch responsive controls")
					id = 100
				} else {
					w, _, _, _ = replyStatusWorker(t)
					w.b.cfg.StatusBar = config.StatusBar{Progress: []string{}, Intermediate: []string{}, Final: []string{"git"}, GitTimeout: 20 * time.Second}
				}
				gate := newGitTimeoutGate(t)
				w.b.cfg.StatusBar.GitCommand = []string{replyGitCommandScript(t, gate.body+"printf stale")}
				if research {
					researchFinishNaturally(t, w, "answer")
					gitTimeoutUntil(t, w, 3*time.Second, gate.ready)
				} else {
					endStatusBarBody(t, w, "answer", "fixture")
					waitFor(t, gate.ready)
				}
				began := time.Now()
				nativeInput(t, w, 102, control, false)
				gitTimeoutUntil(t, w, 2*time.Second, func() bool { return !w.controlInProgress() && !w.taskActive() })
				if elapsed := time.Since(began); elapsed > 2*time.Second {
					t.Fatalf("control waited for configured Git budget: %s", elapsed)
				}
				if state := queueState(t, w, id); state == "submitted" || state == "running" {
					t.Fatalf("control retained the old root: %s", state)
				}
				rows := researchReplies(t, w, id)
				var bodies int
				requireStoreOK(t, w.b.db.DB.QueryRow("SELECT count(*) FROM outbox WHERE inbox_id=? AND text='answer'", id).Scan(&bodies))
				if bodies != 1 || strings.Contains(strings.Join(rows, "\n"), "stale") {
					t.Fatalf("control lost the body or accepted a pending Git snapshot: %q", rows)
				}
				// Do not release the script: both the mutation lane and the child
				// must be canceled by the control, not by a cooperative test gate.
				gitTimeoutUntil(t, w, 2*time.Second, func() bool { return !w.rpcOperationActive && len(w.rpcOperations) == 0 && w.replyStatus.pending == nil })
				done := make(chan struct{})
				go func() { w.background.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("control left the long-running Git child alive")
				}
				for len(w.operations) != 0 {
					w.operationReturned(<-w.operations)
				}
				if got := researchReplies(t, w, id); strings.Join(got, "\x00") != strings.Join(rows, "\x00") {
					t.Fatalf("late Git completion rewrote a controlled reply: before=%q after=%q", rows, got)
				}
			})
		}
	}
}
