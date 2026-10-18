package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/media"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

func configureTestArgPresets(b *Bridge, presets [][]string, fingerprint string, index int) {
	b.argsMu.Lock()
	b.argsPresets = cloneArgPresets(presets)
	b.argsFingerprint = fingerprint
	b.argsIndex = index
	b.argsMu.Unlock()
}

func newArgsTestPeer(t *testing.T, w *worker, thread int64) *worker {
	t.Helper()
	ctx, cancel := context.WithCancel(w.ctx)
	peer := testWorker(t, &worker{
		b: w.b, key: target{chat: w.key.chat, thread: thread}, ctx: ctx, cancel: cancel,
		confirms: make(map[string]confirmation),
	})
	t.Cleanup(func() {
		peer.teardownWorker(true)
		cancel()
		peer.background.Wait()
	})
	return peer
}

func readStringArgvTrace(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var trace [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatal(err)
		}
		trace = append(trace, args)
	}
	return trace
}

func writeProfileSessionFixtures(t *testing.T, profiles map[string][]resumeFixtureSession) {
	t.Helper()
	raw, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSIONS_BY_PROFILE", string(raw))
}

func writeFixtureSessionFile(t *testing.T, id, cwd string) {
	t.Helper()
	path := filepath.Join(os.Getenv("OMP_TELEGRAM_FIXTURE_SESSION_ROOT"), id+".jsonl")
	raw, err := json.Marshal(map[string]string{"type": "session", "id": id, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

func testConfigOverlay(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp-config.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func persistTestSelection(t *testing.T, b *Bridge, index int) {
	t.Helper()
	if err := b.db.SetOMPArgsSelection(index, b.argsFingerprint, len(b.argsPresets)); err != nil {
		t.Fatal(err)
	}
}

func TestArgsCommandMenuFallsBackToPresetNumbersAndPersistsSelection(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	first := "private-args-canary-one"
	second := "private-args-canary-two"
	configureTestArgPresets(w.b, [][]string{{"--model", first}, {"--model", second}}, strings.Repeat("a", 64), 1)
	command("/args")
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Current OMP argument preset: 1") || len(rows) != 3 || rows[0][0]["text"] != "* Preset 1" || rows[1][0]["text"] != "Preset 2" || rows[2][0]["text"] != "Cancel" {
		t.Fatalf("preset menu = %q, %v", text, rows)
	}
	messageID := int64(f.messageCount())
	var outboxBefore int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{
		ID: "args-menu", From: telegram.User{ID: 7},
		Message: &telegram.Message{MessageID: messageID, MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}},
		Data:    rows[1][0]["callback_data"].(string),
	})
	if index, _ := w.b.argsPresetSelection(); index != 2 {
		t.Fatalf("selected preset = %d, want 2", index)
	}
	requireFinishedSessionPicker(t, f, messageID, "Selected OMP argument preset 2.")
	var outboxAfter int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if outboxAfter != outboxBefore {
		t.Fatalf("menu selection queued a duplicate reply: outbox rows %d -> %d", outboxBefore, outboxAfter)
	}
	var storedIndex int
	var fingerprint string
	if err := w.b.db.DB.QueryRow("SELECT preset_index,args_fingerprint FROM omp_args_selection").Scan(&storedIndex, &fingerprint); err != nil {
		t.Fatal(err)
	}
	if storedIndex != 2 || fingerprint != strings.Repeat("a", 64) {
		t.Fatalf("persisted selection = %d, %q", storedIndex, fingerprint)
	}
}

func TestArgsCommandMenuUsesConfiguredLabels(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	first := "private-args-canary-plus"
	second := "private-args-canary-pro"
	configureTestArgPresets(w.b, [][]string{{"--profile", first}, {"--profile", second}}, strings.Repeat("d", 64), 1)
	w.b.cfg.OMPArgsLabels = []string{"Plus", "Pro"}
	command("/args")
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Current OMP argument preset: 1") || len(rows) != 3 || rows[0][0]["text"] != "1: * Plus" || rows[1][0]["text"] != "2: Pro" || rows[2][0]["text"] != "Cancel" {
		t.Fatalf("labeled preset menu = %q, %v", text, rows)
	}
	if strings.Contains(text, first) || strings.Contains(text, second) {
		t.Fatalf("raw argv appeared despite configured labels: %q", text)
	}
	messageID := int64(f.messageCount())
	var outboxBefore int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxBefore); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{
		ID: "args-labeled", From: telegram.User{ID: 7},
		Message: &telegram.Message{MessageID: messageID, MessageThreadID: w.key.thread, Chat: telegram.Chat{ID: w.key.chat}},
		Data:    rows[1][0]["callback_data"].(string),
	})
	requireFinishedSessionPicker(t, f, messageID, "Selected OMP argument preset 2: Pro.")
	var outboxAfter int
	if err := w.b.db.DB.QueryRow("SELECT COUNT(*) FROM outbox").Scan(&outboxAfter); err != nil {
		t.Fatal(err)
	}
	if outboxAfter != outboxBefore {
		t.Fatalf("labeled menu selection queued a duplicate reply: outbox rows %d -> %d", outboxBefore, outboxAfter)
	}
	command("/args 2")
	var directReply string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&directReply); err != nil {
		t.Fatal(err)
	}
	if directReply != "Selected OMP argument preset 2: Pro." {
		t.Fatalf("direct preset selection reply = %q", directReply)
	}
}

func TestArgsMenuKeepsCurrentMarkerBeforeLongLabel(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--model", "first"}, {"--model", "second"}}, strings.Repeat("d", 64), 1)
	w.b.cfg.OMPArgsLabels = []string{strings.Repeat("😀", 100), "Second"}
	command("/args")
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Asterisk marks the current preset.") || len(rows) != 3 {
		t.Fatalf("long-label menu = %q, %v", text, rows)
	}
	if got := rows[0][0]["text"].(string); !strings.HasPrefix(got, "1: * ") || utf16Length(got) > 64 {
		t.Fatalf("current marker was lost or button is too long: %q (%d UTF-16 units)", got, utf16Length(got))
	}
	if rows[1][0]["text"] != "2: Second" || rows[2][0]["text"] != "Cancel" {
		t.Fatalf("noncurrent label changed: %v", rows)
	}
}

func TestArgsCommandMenuUsesPresetNumbersWhenLabelsMismatch(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	first := "private-args-canary-one"
	second := "private-args-canary-two"
	configureTestArgPresets(w.b, [][]string{{"--model", first}, {"--model", second}}, strings.Repeat("e", 64), 1)
	w.b.cfg.OMPArgsLabels = []string{"Only one label"}
	command("/args")
	text, rows := pickerView(t, f)
	if len(rows) != 3 || rows[0][0]["text"] != "* Preset 1" || rows[1][0]["text"] != "Preset 2" || rows[2][0]["text"] != "Cancel" {
		t.Fatalf("mismatched labels did not fall back to preset numbers: %q, %v", text, rows)
	}
	if strings.Contains(text, first) || strings.Contains(text, second) {
		t.Fatalf("raw argv appeared after a label-count mismatch: %q", text)
	}
}

func TestArgsMenuKeepsOwnerGenerationRulesAcrossTopics(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "one"}, {"--profile", "two"}}, strings.Repeat("b", 64), 1)
	persistTestSelection(t, w.b, 1)
	command("/args")
	_, rows := pickerView(t, f)
	data := rows[0][0]["callback_data"].(string)
	messageID := int64(f.messageCount())
	peer := newArgsTestPeer(t, w, 12)
	peer.handleArgsCommand("2", 7)
	if index, _ := w.b.argsPresetSelection(); index != 2 {
		t.Fatalf("cross-topic selection = %d, want 2", index)
	}
	query := &telegram.CallbackQuery{ID: "wrong-owner", From: telegram.User{ID: 8}, Message: &telegram.Message{MessageID: messageID}, Data: data}
	w.callback(query)
	if index, _ := w.b.argsPresetSelection(); index != 2 {
		t.Fatalf("wrong-owner callback changed selection to %d", index)
	}
	w.binding.Generation++
	query.ID = "stale-generation"
	query.From.ID = 7
	w.callback(query)
	if index, _ := w.b.argsPresetSelection(); index != 2 {
		t.Fatalf("stale-generation callback changed selection to %d", index)
	}
	command("/args")
	_, rows = pickerView(t, f)
	if rows[1][0]["text"] != "* Preset 2" {
		t.Fatalf("global current preset was not shown in the menu: %v", rows)
	}
	messageID = int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "owner-selection", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: rows[0][0]["callback_data"].(string)})
	var storedIndex int
	if err := w.b.db.DB.QueryRow("SELECT preset_index FROM omp_args_selection").Scan(&storedIndex); err != nil || storedIndex != 1 {
		t.Fatalf("cross-topic persisted preset = %d, error %v; want 1", storedIndex, err)
	}
}

func TestArgsSelectionSerializesConcurrentTopics(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "one"}, {"--profile", "two"}, {"--profile", "three"}}, strings.Repeat("c", 64), 1)
	persistTestSelection(t, w.b, 1)
	peer := newArgsTestPeer(t, w, 12)
	var group sync.WaitGroup
	for range 10 {
		group.Add(2)
		go func() { defer group.Done(); w.handleArgsCommand("2", 7) }()
		go func() { defer group.Done(); peer.handleArgsCommand("3", 7) }()
	}
	group.Wait()
	index, _ := w.b.argsPresetSelection()
	var stored int
	if err := w.b.db.DB.QueryRow("SELECT preset_index FROM omp_args_selection").Scan(&stored); err != nil || stored != index {
		t.Fatalf("global preset=%d, stored=%d, error=%v", index, stored, err)
	}
}

func TestArgsInvalidIndexAndFailedWriteDoNotChangeSelection(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "one"}, {"--profile", "two"}}, strings.Repeat("d", 64), 1)
	persistTestSelection(t, w.b, 1)
	for _, arg := range []string{"bad", "0", "3", "1 2"} {
		command("/args " + arg)
		if index, _ := w.b.argsPresetSelection(); index != 1 {
			t.Fatalf("invalid /args %q changed selection to %d", arg, index)
		}
	}
	if _, err := w.b.db.DB.Exec(`CREATE TRIGGER fail_omp_args_selection BEFORE INSERT ON omp_args_selection BEGIN SELECT RAISE(ABORT, 'injected selection failure'); END`); err != nil {
		t.Fatal(err)
	}
	command("/args 2")
	if index, _ := w.b.argsPresetSelection(); index != 1 {
		t.Fatalf("failed persistence changed memory to preset %d", index)
	}
	var stored int
	if err := w.b.db.DB.QueryRow("SELECT preset_index FROM omp_args_selection").Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("failed transaction changed persisted preset to %d, error %v", stored, err)
	}
}

func TestBridgeStartupLoadsDurableArgsSelectionAndResetsFingerprint(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	firstConfig := config.Config{OMPArgs: [][]string{{"--profile", "one"}, {"--profile", "two"}}, OMPArgsFingerprint: strings.Repeat("e", 64)}
	first := testBridge(t, &Bridge{cfg: firstConfig, db: db})
	if err := first.initializeArgsSelection(); err != nil {
		t.Fatal(err)
	}
	if err := first.selectArgsPreset(2); err != nil {
		t.Fatal(err)
	}
	restarted := testBridge(t, &Bridge{cfg: firstConfig, db: db})
	if err := restarted.initializeArgsSelection(); err != nil {
		t.Fatal(err)
	}
	if index, count := restarted.argsPresetSelection(); index != 2 || count != 2 || strings.Join(restarted.selectedArgsSnapshot(), " ") != "--profile two" {
		t.Fatalf("same-config restart selection = %d/%d, args %v", index, count, restarted.selectedArgsSnapshot())
	}
	changed := testBridge(t, &Bridge{cfg: config.Config{OMPArgs: [][]string{{"--profile", "changed-one"}, {"--profile", "changed-two"}}, OMPArgsFingerprint: strings.Repeat("f", 64)}, db: db})
	if err := changed.initializeArgsSelection(); err != nil {
		t.Fatal(err)
	}
	if index, _ := changed.argsPresetSelection(); index != 1 || strings.Join(changed.selectedArgsSnapshot(), " ") != "--profile changed-one" {
		t.Fatalf("changed-config recovery = preset %d, args %v", index, changed.selectedArgsSnapshot())
	}
	var stored int
	var fingerprint string
	if err := db.DB.QueryRow("SELECT preset_index,args_fingerprint FROM omp_args_selection").Scan(&stored, &fingerprint); err != nil || stored != 1 || fingerprint != strings.Repeat("f", 64) {
		t.Fatalf("reset selection = %d, %q, error %v", stored, fingerprint, err)
	}
	empty := &Bridge{}
	if args := empty.selectedArgsSnapshot(); len(args) != 0 {
		t.Fatalf("empty direct config args = %v, want an empty singleton preset", args)
	}
	if presets := empty.argsPresetsSnapshot(); len(presets) != 1 || len(presets[0]) != 0 {
		t.Fatalf("empty direct config presets = %#v", presets)
	}
}

func TestArgsLabelsFallbackWarnsWithoutBlockingStartup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		labels      []string
		wantWarning bool
		wantLabels  int
	}{
		{name: "missing", wantWarning: true},
		{name: "mismatched", labels: []string{"Only one"}, wantWarning: true},
		{name: "blank", labels: []string{"First", "  "}, wantWarning: true},
		{name: "matching", labels: []string{"First", "Second"}, wantLabels: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := store.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var output bytes.Buffer
			b := &Bridge{
				cfg: config.Config{
					OMPArgs:            [][]string{{"--model", "private-canary-one"}, {"--model", "private-canary-two"}},
					OMPArgsFingerprint: strings.Repeat("f", 64),
					OMPArgsLabels:      tc.labels,
				},
				db:  db,
				log: slog.New(slog.NewTextHandler(&output, nil)),
			}
			if err := b.initializeArgsSelection(); err != nil {
				t.Fatalf("label issue blocked startup: %v", err)
			}
			if warned := strings.Contains(output.String(), "omp_args_labels_fallback"); warned != tc.wantWarning {
				t.Fatalf("warning = %t, want %t: %s", warned, tc.wantWarning, output.String())
			}
			if strings.Contains(output.String(), "private-canary") {
				t.Fatal("startup warning exposed raw arguments")
			}
			_, count, labels := b.argsMenuSnapshot()
			if count != 2 || len(labels) != tc.wantLabels {
				t.Fatalf("menu labels = %v for %d presets, want %d labels", labels, count, tc.wantLabels)
			}
		})
	}
}

func TestActiveArgsSwitchLeavesClientAndFutureStartUsesNewPreset(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", tracePath)
	t.Setenv("OMP_TELEGRAM_FIXTURE_HOLD_RPC", "prompt")
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 4
	one, two := testConfigOverlay(t), testConfigOverlay(t)
	configureTestArgPresets(w.b, [][]string{{"--config", one}, {"--config", two}}, strings.Repeat("1", 64), 1)
	w.b.slots = make(chan struct{}, 2)
	command("/new " + t.TempDir())
	oldClient := w.client
	command("active task")
	w.dispatch()
	if !w.taskActive() {
		t.Fatal("fixture did not keep the original client busy")
	}
	command("/args 2")
	if w.client != oldClient || strings.Join(w.launchArgs, "\x00") != strings.Join([]string{"--config", one}, "\x00") {
		t.Fatal("global preset change restarted or mutated the active client's launch arguments")
	}
	peer := newArgsTestPeer(t, w, 12)
	if failure := peer.start(false, t.TempDir(), "", false); failure != "" {
		t.Fatalf("future start failed: %s", failure)
	}
	if strings.Join(peer.launchArgs, "\x00") != strings.Join([]string{"--config", two}, "\x00") {
		t.Fatalf("future client launch arguments = %v", peer.launchArgs)
	}
	trace := readStringArgvTrace(t, tracePath)
	if len(trace) != 2 || !strings.Contains(strings.Join(trace[0], "\x00"), one) || !strings.Contains(strings.Join(trace[1], "\x00"), two) {
		t.Fatalf("fixture launch argv does not show old then selected args: %v", trace)
	}
}

func TestConnectedModelQueryUsesLaunchPresetAfterSelectionChanges(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "model.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_MODEL_TRACE", tracePath)
	w, _, command := setupWorkspaceWorker(t)
	one, two := testConfigOverlay(t), testConfigOverlay(t)
	configureTestArgPresets(w.b, [][]string{{"--config", one}, {"--config", two}}, strings.Repeat("2", 64), 1)
	command("/new " + t.TempDir())
	modelUpdate := update(9000, w.key.thread, "/model")
	modelRaw, err := json.Marshal(modelUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(9000, modelRaw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: 9000, msg: modelUpdate.Message})
	u := update(9001, w.key.thread, "/args 2")
	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.b.db.Accept(9001, raw); err != nil {
		t.Fatal(err)
	}
	w.handle(incoming{id: 9001, msg: u.Message})
	drainControlOperations(t, w)
	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record map[string]string
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 {
		t.Fatalf("model lookup trace = %v", records)
	}
	for _, record := range records {
		if !strings.Contains(record["config_files"], one) || strings.Contains(record["config_files"], two) {
			t.Fatalf("old connected model query used changed args: %v", records)
		}
	}
}

func TestReleasedLazyResumeUsesCurrentArgsPreset(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", tracePath)
	w, _, command := setupWorkspaceWorker(t)
	one, two := testConfigOverlay(t), testConfigOverlay(t)
	configureTestArgPresets(w.b, [][]string{{"--config", one}, {"--config", two}}, strings.Repeat("3", 64), 1)
	command("/new " + t.TempDir())
	oldClient := w.client
	w.releaseRuntime(true)
	if w.launchArgs != nil || w.client != nil {
		t.Fatal("release retained a connected-runtime argument snapshot")
	}
	command("/args 2")
	if _, err := w.ensureRuntime(); err != nil {
		t.Fatal(err)
	}
	if w.client == oldClient || strings.Join(w.launchArgs, "\x00") != strings.Join([]string{"--config", two}, "\x00") {
		t.Fatalf("lazy resume did not use the current preset: client changed=%t, args=%v", w.client != oldClient, w.launchArgs)
	}
	trace := readStringArgvTrace(t, tracePath)
	if len(trace) != 2 || !strings.Contains(strings.Join(trace[1], "\x00"), two) {
		t.Fatalf("lazy resume fixture argv = %v", trace)
	}
}

func TestBindingNameLookupKeepsItsStartingPreset(t *testing.T) {
	w, _, _ := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "old"}, {"--profile", "current"}}, strings.Repeat("4", 64), 1)
	workspace := t.TempDir()
	sessionID := "abcd0000-0000-4000-8000-000000000000"
	writeProfileSessionFixtures(t, map[string][]resumeFixtureSession{
		"old":     {{ID: sessionID, CWD: workspace, Title: "Old profile name", UpdatedAt: "2026-10-01T00:00:00Z"}},
		"current": {{ID: sessionID, CWD: workspace, Title: "Current profile name", UpdatedAt: "2026-10-01T00:00:00Z"}},
	})
	gate := filepath.Join(t.TempDir(), "list-gate")
	release := filepath.Join(t.TempDir(), "list-release")
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_LIST_GATE", gate)
	t.Setenv("OMP_TELEGRAM_FIXTURE_SESSION_LIST_RELEASE", release)
	entry := store.BindingListEntry{Binding: &store.Binding{Bot: w.b.bot.ID, Chat: w.key.chat, Thread: w.key.thread, Workspace: workspace, SessionID: sessionID, Generation: 1}}
	w.lookupBindingNames([]store.BindingListEntry{entry}, 1)
	waitFor(t, func() bool { _, err := os.Stat(gate); return err == nil })
	if err := w.b.selectArgsPreset(2); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	var result bindingNamesResult
	select {
	case result = <-w.bindingNameResults:
	case <-time.After(5 * time.Second):
		t.Fatal("binding-name lookup did not finish")
	}
	if result.names[sessionID] != "Old profile name" {
		t.Fatalf("binding-name metadata used the changed preset: %+v", result.names)
	}
}

func TestExportPickerUsesOriginatingPresetAfterGlobalSwitch(t *testing.T) {
	renderTrace := filepath.Join(t.TempDir(), "render.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_RENDER_TRACE", renderTrace)
	w, f, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "old"}, {"--profile", "current"}}, strings.Repeat("5", 64), 1)
	command("/new " + t.TempDir())
	selectedID := "abcd0000-0000-4000-8000-000000000000"
	writeFixtureSessionFile(t, selectedID, w.binding.Workspace)
	writeProfileSessionFixtures(t, map[string][]resumeFixtureSession{
		"old":     {{ID: selectedID, CWD: w.binding.Workspace, Title: "Old profile session", UpdatedAt: "2026-10-01T00:00:00Z"}},
		"current": {},
	})
	command("/export")
	w.resumeListed(finishResumeList(t, w))
	if err := w.b.selectArgsPreset(2); err != nil {
		t.Fatal(err)
	}
	buttons := pickerButtons(t, f)
	messageID := int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "old-export-picker", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: buttons[0]["callback_data"].(string)})
	result := finishExport(t, w)
	if result.err != nil {
		t.Fatalf("originating-profile export failed: %v", result.err)
	}
	w.exportFinished(result)
	trace := readStringArgvTrace(t, renderTrace)
	if len(trace) != 1 {
		t.Fatalf("export renderer trace = %v", trace)
	}
	joined := strings.Join(trace[0], "\x00")
	if !strings.Contains(joined, "--profile\x00old") || strings.Contains(joined, "--profile\x00current") {
		t.Fatalf("export renderer did not keep the picker profile: %v", trace)
	}
}

func TestSessionDeleteUsesOriginatingPickerPreset(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_DELETE", "success")
	tracePath := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", tracePath)
	w, f, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "old"}, {"--profile", "current"}}, strings.Repeat("6", 64), 1)
	command("/new " + t.TempDir())
	selectedID := "abcd0000-0000-4000-8000-000000000000"
	writeFixtureSessionFile(t, selectedID, w.binding.Workspace)
	writeProfileSessionFixtures(t, map[string][]resumeFixtureSession{
		"old":     {{ID: selectedID, CWD: w.binding.Workspace, Title: "Old profile session", UpdatedAt: "2026-10-01T00:00:00Z"}},
		"current": {},
	})
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	deleteButton := deletePickerButton(t, f, 0)
	pickerMessageID := int64(f.messageCount())
	if err := w.b.selectArgsPreset(2); err != nil {
		t.Fatal(err)
	}
	w.callback(&telegram.CallbackQuery{ID: "old-delete-picker", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: pickerMessageID}, Data: deleteButton["callback_data"].(string)})
	var confirmationToken string
	var deletion confirmation
	for token, c := range w.confirms {
		if c.action == "session_delete" {
			confirmationToken, deletion = token, c
			break
		}
	}
	if confirmationToken == "" || strings.Join(deletion.ompArgs, " ") != "--profile old" {
		t.Fatalf("delete confirmation lost originating args: %+v", deletion)
	}
	buttons := pickerButtons(t, f)
	w.callback(&telegram.CallbackQuery{ID: "confirm-delete", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: deletion.messageID}, Data: buttons[0]["callback_data"].(string)})
	result := awaitSessionDelete(t, w)
	w.sessionDeleteFinished(result)
	if result.err != nil {
		t.Fatalf("fixture session deletion failed: %v", result.err)
	}
	trace := readStringArgvTrace(t, tracePath)
	matched := false
	for _, args := range trace {
		joined := strings.Join(args, "\x00")
		if strings.Contains(joined, selectedID) && strings.Contains(joined, "--profile\x00old") && !strings.Contains(joined, "--profile\x00current") {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("native deletion did not use the originating profile: %v", trace)
	}
}

func TestResumePickerRejectsChangedPresetWithoutReplacingRuntime(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	configureTestArgPresets(w.b, [][]string{{"--profile", "old"}, {"--profile", "current"}}, strings.Repeat("7", 64), 1)
	command("/new " + t.TempDir())
	before := w.binding
	oldClient := w.client
	if oldClient == nil {
		t.Fatal("fixture session did not start")
	}

	selectedID := "abcd0000-0000-4000-8000-000000000000"
	writeFixtureSessionFile(t, selectedID, before.Workspace)
	writeProfileSessionFixtures(t, map[string][]resumeFixtureSession{
		"old":     {{ID: selectedID, CWD: before.Workspace, Title: "Old profile session", UpdatedAt: "2026-10-01T00:00:00Z"}},
		"current": {},
	})
	command("/resume")
	w.resumeListed(finishResumeList(t, w))
	text, _ := pickerView(t, f)
	buttons := resumeButtons(t, f)
	if !strings.Contains(text, "Old profile session") || len(buttons) == 0 {
		t.Fatalf("old-profile picker did not show its session: %q, %v", text, buttons)
	}

	peer := newArgsTestPeer(t, w, 12)
	peer.handleArgsCommand("2", 7)
	if index, _ := w.b.argsPresetSelection(); index != 2 {
		t.Fatalf("other topic selected preset %d, want 2", index)
	}
	clickResume(w, 7, buttons[0]["callback_data"].(string))
	requireFinishedSessionPicker(t, f, 1, "OMP argument preset changed. Use /resume again.")
	requireNoSessionSelectionReceipt(t, f)
	if w.client != oldClient || w.runtime != runtimeConnected || w.startIntent != nil || !sameBindingIdentity(w.binding, before) {
		t.Fatal("stale Resume picker closed or replaced the active runtime")
	}
	stored, err := w.b.db.Binding(w.b.bot.ID, w.key.chat, w.key.thread)
	if err != nil || !sameBindingIdentity(stored, before) {
		t.Fatalf("stale Resume picker changed the stored binding: %+v, error %v", stored, err)
	}
}

func TestResumeStartUsesValidatedArgsSnapshotAfterGlobalSwitch(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv("OMP_TELEGRAM_FIXTURE_ARGV_TRACE", tracePath)
	w, _, command := setupWorkspaceWorker(t)
	fingerprint := strings.Repeat("8", 64)
	configureTestArgPresets(w.b, [][]string{{"--profile", "old"}, {"--profile", "current"}}, fingerprint, 1)
	command("/new " + t.TempDir())
	before := w.binding
	args, current := w.b.argsPresetSnapshotIfCurrent(1, fingerprint)
	if !current {
		t.Fatal("initial resume preset was not current")
	}

	selectedID := "abcd0000-0000-4000-8000-000000000000"
	writeFixtureSessionFile(t, selectedID, before.Workspace)
	peer := newArgsTestPeer(t, w, 12)
	peer.handleArgsCommand("2", 7)
	w.startFencedWithArgs(true, selectedID, before.Workspace, true, before.Generation, w.b.bindingsEpoch.Load(), args)
	if w.client == nil || w.sessionID != selectedID || strings.Join(w.launchArgs, "\x00") != "--profile\x00old" {
		t.Fatalf("resume did not use its verified preset snapshot: client=%v session=%q args=%v", w.client != nil, w.sessionID, w.launchArgs)
	}
	trace := readStringArgvTrace(t, tracePath)
	if len(trace) != 2 || !strings.Contains(strings.Join(trace[1], "\x00"), "old") || strings.Contains(strings.Join(trace[1], "\x00"), "current") {
		t.Fatalf("resume launch reread the changed preset: %v", trace)
	}
}

func TestIncomingCleanupUnionsAllPresetVisibilityAndRetainsOnAnyFailure(t *testing.T) {
	dataDir := t.TempDir()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	workspaceWithSession, workspaceFailure := t.TempDir(), t.TempDir()
	retainedID := "abcd0000-0000-4000-8000-000000000000"
	cutoff := time.Now().AddDate(0, 0, -90)
	updated := cutoff.Add(time.Hour).Format(time.RFC3339Nano)
	writeProfileSessionFixtures(t, map[string][]resumeFixtureSession{
		"current": {},
		"legacy":  {{ID: retainedID, CWD: workspaceWithSession, Title: "legacy-visible", UpdatedAt: updated}},
	})
	b := testBridge(t, &Bridge{
		cfg: config.Config{DataDir: dataDir, OMP: binary}, db: db, bot: telegram.User{ID: 99},
		resumeSlots: make(chan struct{}, 2),
	})
	configureTestArgPresets(b, [][]string{{"--profile", "current"}, {"--profile", "legacy"}}, strings.Repeat("7", 64), 1)
	inbox := filepath.Join(dataDir, "attachments", "inbox")
	addOwned := func(name, sessionID, workspace string) string {
		t.Helper()
		path := filepath.Join(inbox, "incoming-"+name)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := media.WriteIncomingOwner(dataDir, path, media.IncomingOwner{SessionID: sessionID, Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		old := cutoff.Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	visible := addOwned("visible-only-in-legacy", retainedID, workspaceWithSession)
	if removed, err := b.cleanupIncomingAttachments(context.Background(), cutoff); err != nil || removed != 0 {
		t.Fatalf("union cleanup removed %d directories, error %v", removed, err)
	}
	if _, err := os.Stat(visible); err != nil {
		t.Fatalf("session visible only in the old preset lost its attachment: %v", err)
	}
	failed := addOwned("list-failure", "missing-session", workspaceFailure)
	t.Setenv("OMP_TELEGRAM_FIXTURE_FAIL_PROFILE", "legacy")
	if removed, err := b.cleanupIncomingAttachments(context.Background(), cutoff); err != nil || removed != 0 {
		t.Fatalf("failed-preset cleanup removed %d directories, error %v", removed, err)
	}
	if _, err := os.Stat(failed); err != nil {
		t.Fatalf("list failure did not conservatively retain attachment: %v", err)
	}
}

func TestArgsMenuPagesLargePresetSets(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	presets := make([][]string, 100)
	for i := range presets {
		presets[i] = []string{"--profile", fmt.Sprintf("profile-%d", i+1)}
	}
	configureTestArgPresets(w.b, presets, strings.Repeat("9", 64), 1)
	command("/args")
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Page 1/2") || !strings.Contains(text, "Current OMP argument preset: 1") || len(rows) != argsMenuPageSize+2 || rows[0][0]["text"] != "* Preset 1" || rows[argsMenuPageSize][0]["text"] != "Next" || rows[len(rows)-1][0]["text"] != "Cancel" {
		t.Fatalf("first args page has invalid bounds: %q, rows=%d", text, len(rows))
	}
	messageID := int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "args-next-page", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: rows[argsMenuPageSize][0]["callback_data"].(string)})
	text, rows = pickerView(t, f)
	if !strings.Contains(text, "Page 2/2") || len(rows) != 12 || rows[0][0]["text"] != "Preset 91" || rows[9][0]["text"] != "Preset 100" || rows[10][0]["text"] != "Previous" || rows[11][0]["text"] != "Cancel" {
		t.Fatalf("second args page has invalid bounds: %q, rows=%v", text, rows)
	}
	messageID = int64(f.messageCount())
	w.callback(&telegram.CallbackQuery{ID: "args-last-preset", From: telegram.User{ID: 7}, Message: &telegram.Message{MessageID: messageID}, Data: rows[9][0]["callback_data"].(string)})
	if index, _ := w.b.argsPresetSelection(); index != 100 {
		t.Fatalf("paged args selection = %d, want 100", index)
	}
}

func TestArgsMenuDoesNotDisplayLongRawArgs(t *testing.T) {
	w, f, command := setupWorkspaceWorker(t)
	first := strings.Repeat("a", 2000)
	second := strings.Repeat("b", 2000)
	configureTestArgPresets(w.b, [][]string{{"--profile", first}, {"--profile", second}}, strings.Repeat("c", 64), 1)
	command("/args")
	text, rows := pickerView(t, f)
	if !strings.Contains(text, "Current OMP argument preset: 1") || len(rows) != 3 || rows[0][0]["text"] != "* Preset 1" || rows[1][0]["text"] != "Preset 2" || rows[2][0]["text"] != "Cancel" {
		t.Fatalf("long-args menu = %q, %v", text, rows)
	}
	if strings.Contains(text, first) || strings.Contains(text, second) {
		t.Fatal("long raw arguments appeared in the menu")
	}
}
