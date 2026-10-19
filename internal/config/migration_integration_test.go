package config

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/omp"
)

// TestRealOMPMigrationCompatibility is opt-in: it never discovers omp on PATH.
func TestRealOMPMigrationCompatibility(t *testing.T) {
	binary := os.Getenv("OMP_TELEGRAM_TEST_OMP")
	if binary == "" {
		t.Skip("set OMP_TELEGRAM_TEST_OMP to an actual omp executable to exercise native migration compatibility")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("explicit omp executable: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		t.Fatalf("OMP_TELEGRAM_TEST_OMP is not an executable regular file: %s", binary)
	}

	path, source := configFixture(t)
	root := filepath.Dir(path)
	home := filepath.Join(root, "home")
	agentDir := filepath.Join(root, "agent")
	workspace := filepath.Join(root, "workspace")
	sessions := filepath.Join(root, "sessions")
	for _, dir := range []string{home, agentDir, workspace, sessions} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Chdir(workspace)

	// A closed loopback endpoint and static credentials require no provider access.
	// Reasoning support makes /effort expose the configured auto selector.
	const provider = "migration-compat"
	models := `{"providers":{"migration-compat":{"baseUrl":"http://127.0.0.1:9/v1","apiKey":"local-compatibility-placeholder","api":"openai-completions","models":[{"id":"b","name":"Compatibility B","reasoning":true,"input":["text"],"contextWindow":128000,"maxTokens":4096,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"compat":{"supportsReasoningEffort":true}},{"id":"a","name":"Compatibility A","reasoning":true,"input":["text"],"contextWindow":128000,"maxTokens":4096,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0},"compat":{"supportsReasoningEffort":true}}]}}}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.yml"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	nativePath := filepath.Join(root, "native.yml")
	writeDefaults := func() {
		t.Helper()
		const native = `{"modelRoles":{"default":"migration-compat/b"},"defaultThinkingLevel":"auto","prewalk":{"enabled":false}}`
		if err := os.WriteFile(nativePath, []byte(native), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeDefaults()
	args := "--config " + strconv.Quote(nativePath) + " --session-dir " + strconv.Quote(sessions) +
		" --no-title --no-tools --no-lsp --no-extensions --no-skills --no-rules --no-ui"
	source = setTOMLField(source, "omp", "binary", strconv.Quote(binary))
	source = setTOMLField(source, "omp", "args", strconv.Quote(args))
	source = setTOMLField(source, "omp", "migration_extension", "true")
	source = setTOMLField(source, "storage", "workspace_root", strconv.Quote(workspace))
	source += "\n[omp.environment]\nmode = \"allowlist\"\nallow = [\"HOME\", \"PI_CODING_AGENT_DIR\", \"PATH\"]\n"
	cfg, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	version := exec.CommandContext(ctx, cfg.OMP, "--session-dir", sessions, "--version")
	version.Dir = workspace
	version.Env = omp.ChildEnv(cfg.OMPEnvironment)
	output, err := version.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) == "" {
		t.Fatalf("explicit omp --version: %v, output=%q", err, output)
	}
	t.Logf("native omp --version: %s", strings.TrimSpace(string(output)))

	start := func(resume string) *omp.Client {
		t.Helper()
		client, err := omp.Start(ctx, omp.Config{
			Binary: cfg.OMP, CWD: workspace, Resume: resume,
			Args: cfg.OMPArgs[0], Environment: cfg.OMPEnvironment,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("start native omp (resume=%t): %v", resume != "", err)
		}
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for range client.Events() {
			}
		}()
		t.Cleanup(func() {
			_ = client.Close()
			<-drained
		})
		return client
	}
	settings := func(client *omp.Client) omp.MigrationSettings {
		t.Helper()
		var actual omp.MigrationSettings
		realOMPMigrationAwait(t, ctx, func() error {
			var err error
			actual, err = client.MigrationSettings(ctx)
			return err
		})
		return actual
	}
	wantDefault := omp.MigrationSettings{Provider: provider, Model: "b", Thinking: "auto", Fast: false}
	client := start("")
	if actual := settings(client); actual != wantDefault {
		t.Fatalf("source defaults = %+v, want %+v", actual, wantDefault)
	}
	if _, err := client.Call(ctx, "set_model", map[string]any{"provider": provider, "modelId": "a"}); err != nil {
		t.Fatalf("select source model A: %v", err)
	}
	want := wantDefault
	want.Model = "a"
	if actual := settings(client); actual != want {
		t.Fatalf("selected source settings = %+v, want %+v", actual, want)
	}
	sourceInfo, err := client.SessionInfo(ctx)
	if err != nil {
		t.Fatalf("source session identity: %v", err)
	}
	// An empty source rejects handoff. Its temporary budget must not block later initialization.
	if _, err := client.MigrationHandoff(ctx, "rejected-"+sourceInfo.ID, sourceInfo); err == nil {
		t.Fatal("empty native source unexpectedly completed a handoff")
	}
	const summary = "  Native compatibility summary: preserve decision A.\n继续任务 B; retain exact whitespace.\n"
	digest := sha256.Sum256([]byte(summary))
	request := omp.MigrationRequest{
		Version: 1, MigrationID: "compatibility-" + sourceInfo.ID,
		SourceSessionID: sourceInfo.ID, SourceGeneration: 1,
		Workspace: workspace, Summary: summary, SummarySHA256: hex.EncodeToString(digest[:]),
	}
	prompt, err := omp.MigrationPrompt(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, "prompt", map[string]any{"message": prompt}); err != nil {
		t.Fatalf("submit native migration command: %v", err)
	}
	var target omp.SessionInfo
	realOMPMigrationAwait(t, ctx, func() error {
		info, err := client.SessionInfo(ctx)
		if err != nil {
			return err
		}
		if info.ID == sourceInfo.ID {
			return omp.ErrMigrationMarkerNotReady
		}
		if info.File == sourceInfo.File || info.CWD != workspace {
			return omp.ErrMigrationInvalid
		}
		target = info
		return client.VerifyMigration(ctx, request, target, want)
	})
	realOMPMigrationJournal(t, "initialized", target.File, provider+"/a", summary)
	_ = client.Close()

	// Restore B explicitly even if a native version writes model selection settings.
	// A fresh process must prove B is still the default before resume proves A.
	writeDefaults()
	fresh := start("")
	if actual := settings(fresh); actual != wantDefault {
		t.Fatalf("fresh process defaults = %+v, want %+v", actual, wantDefault)
	}
	_ = fresh.Close()
	writeDefaults()
	resumed := start(target.File)
	resumedInfo, err := resumed.SessionInfo(ctx)
	if err != nil {
		t.Fatalf("resumed session identity: %v", err)
	}
	if resumedInfo != target {
		t.Fatalf("resume selected %+v, want %+v", resumedInfo, target)
	}
	if actual := settings(resumed); actual != want {
		t.Fatalf("resumed settings = %+v, want %+v", actual, want)
	}
	realOMPMigrationAwait(t, ctx, func() error {
		return resumed.VerifyMigration(ctx, request, resumedInfo, want)
	})
	realOMPMigrationJournal(t, "resumed", resumedInfo.File, provider+"/a", summary)
}

func realOMPMigrationAwait(t *testing.T, ctx context.Context, operation func() error) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := operation()
		if err == nil {
			return
		}
		if !errors.Is(err, omp.ErrMigrationMarkerNotReady) && !errors.Is(err, omp.ErrMigrationNotSettled) {
			t.Fatalf("native migration compatibility: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native migration readiness deadline: %v (last result: %v)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func realOMPMigrationJournal(t *testing.T, phase, path, model, summary string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 2<<20))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var types []string
	messages, markers, modelChanges := 0, 0, 0
	for scanner.Scan() {
		var entry struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Model      string `json:"model"`
			Message    struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("decode native journal: %v", err)
		}
		types = append(types, entry.Type)
		if entry.Type == "message" {
			messages++
			if entry.Message.Role != "user" || len(entry.Message.Content) != 1 ||
				entry.Message.Content[0].Type != "text" || entry.Message.Content[0].Text != summary {
				t.Fatal("native journal contains a conversation turn other than the exact migration summary")
			}
		}
		if entry.Type == "custom" && entry.CustomType == omp.MigrationMarkerType {
			markers++
		}
		if entry.Type == "model_change" && entry.Model == model {
			modelChanges++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s native journal entry types (append order): %s", phase, strings.Join(types, " -> "))
	if messages != 1 || markers != 1 || modelChanges == 0 {
		t.Fatalf("native journal evidence: summary messages=%d, completion markers=%d, persisted model A entries=%d", messages, markers, modelChanges)
	}
}
