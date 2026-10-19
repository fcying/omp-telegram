package omp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture exercises the actual RPC transport without requiring a model.
func fixtureMigrationCommand(command map[string]any) bool {
	file := os.Getenv("OMP_TEST_MIGRATION_FILE")
	if file == "" {
		return false
	}
	mode := os.Getenv("OMP_TEST_MIGRATION_MODE")
	emit := func(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
	reply := func(value any) {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": value})
	}
	if mode == "reject" && command["type"] != "negotiate_protocol" {
		emit(map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": false, "error": rpcSensitiveCanary})
		return true
	}
	switch command["type"] {
	case "get_state":
		state := map[string]any{"sessionId": "target", "sessionFile": file, "model": map[string]any{"provider": "fixture", "id": "model"}, "fastModeEnabled": true, "isSettled": true, "hasPendingAsyncWork": false, "isStreaming": false, "isCompacting": false, "queuedMessageCount": 0}
		switch mode {
		case "busy":
			state["isSettled"] = false
		case "async":
			state["hasPendingAsyncWork"] = true
		case "queued":
			state["queuedMessageCount"] = 1
		case "streaming":
			state["isStreaming"] = true
		case "compacting":
			state["isCompacting"] = true
		case "fast":
			state["fastModeEnabled"] = false
		case "model":
			state["model"] = map[string]any{"provider": "fixture", "id": "changed"}
		case "missing-settled":
			delete(state, "isSettled")
		case "identity":
			state["sessionId"] = "another"
		}
		reply(state)
	case "get_entries", "get_messages":
		input, err := os.Open(file)
		if err != nil {
			os.Exit(3)
		}
		defer input.Close()
		decoder := json.NewDecoder(input)
		entries := []map[string]any{}
		messages := []any{}
		var leaf any
		for {
			var entry map[string]any
			if err := decoder.Decode(&entry); err == io.EOF {
				break
			} else if err != nil {
				os.Exit(3)
			}
			if entry["type"] == "session" || entry["type"] == "title" {
				continue
			}
			entries = append(entries, entry)
			leaf = entry["id"]
			if entry["type"] == "message" {
				messages = append(messages, entry["message"])
			}
		}
		if command["type"] == "get_entries" {
			if mode == "rpc-conflict" {
				entries[0]["timestamp"] = "changed"
			}
			if mode == "branch" {
				leaf = "seed"
			}
			if mode == "leaf-marker" {
				leaf = "marker"
			}
			reply(map[string]any{"entries": entries, "leafId": leaf})
		} else {
			if mode == "live-missing" {
				messages = []any{}
			}
			if mode == "live-conflict" {
				messages = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "changed"}}}}
			}
			if mode == "live-duplicate" {
				messages = append(messages, messages[0])
			}
			for _, role := range []string{"user", "assistant", "system", "toolResult"} {
				extra := map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": "extra"}}}
				if mode == "live-extra-"+role+"-before" {
					messages = append([]any{extra}, messages...)
				}
				if mode == "live-extra-"+role+"-after" {
					messages = append(messages, extra)
				}
			}
			if mode == "replace-live" {
				data, err := os.ReadFile(file)
				if err != nil {
					os.Exit(3)
				}
				if err := os.WriteFile(file+".replacement", data, 0600); err != nil {
					os.Exit(3)
				}
				if err := os.Rename(file+".replacement", file); err != nil {
					os.Exit(3)
				}
			}
			if mode == "truncate-live" {
				if err := os.Truncate(file, 1); err != nil {
					os.Exit(3)
				}
			}
			reply(map[string]any{"messages": messages})
		}
	case "prompt":
		if command["message"] != "/effort" {
			os.Exit(3)
		}
		thinking := "auto"
		if mode == "thinking" {
			thinking = "high"
		}
		emit(map[string]any{"type": "command_output", "text": "Thinking: " + thinking + "\nAvailable: low, high, auto"})
		reply(map[string]any{"agentInvoked": false})
	default:
		return false
	}
	return true
}

func migrationFixture(t *testing.T) (MigrationRequest, SessionInfo, MigrationSettings, []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	r := MigrationRequest{Version: 1, MigrationID: "migration", SourceSessionID: "source", SourceGeneration: 3, Workspace: dir, Summary: "  exact 中文 summary\nsecond line\n"}
	r.SummarySHA256 = migrationHash(r.Summary)
	target := SessionInfo{ID: "target", File: filepath.Join(dir, "target.jsonl"), CWD: dir}
	entries := []map[string]any{
		migrationTitleFixture(t),
		{"type": "session", "version": 3, "id": target.ID, "cwd": dir},
		{"type": "model_change", "id": "model", "parentId": nil, "model": "fixture/model"},
		{"type": "message", "id": "seed", "parentId": "model", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": r.Summary}}, "timestamp": 1}},
		{"type": "custom", "id": "marker", "parentId": "seed", "customType": MigrationMarkerType, "data": map[string]any{"version": 1, "migrationId": r.MigrationID, "sourceSessionId": r.SourceSessionID, "sourceGeneration": r.SourceGeneration, "targetSessionId": target.ID, "workspace": dir, "summaryEntryId": "seed", "summarySha256": r.SummarySHA256, "reloadCompleted": true}},
	}
	writeMigrationFixture(t, target.File, entries)
	t.Setenv("OMP_TEST_MIGRATION_FILE", target.File)
	return r, target, MigrationSettings{Provider: "fixture", Model: "model", Thinking: "auto", Fast: true}, entries
}

func migrationTitleFixture(t *testing.T) map[string]any {
	t.Helper()
	slot := map[string]any{"type": "title", "v": 1, "title": "", "updatedAt": "2026-10-19T00:00:00.000Z", "pad": ""}
	raw, err := json.Marshal(slot)
	if err != nil {
		t.Fatal(err)
	}
	slot["pad"] = strings.Repeat(" ", 255-len(raw))
	return slot
}

func writeMigrationFixture(t *testing.T, path string, entries []map[string]any) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := json.NewEncoder(file).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPromptExactContentAndBounds(t *testing.T) {
	r, _, _, _ := migrationFixture(t)
	prompt, err := MigrationPrompt(r)
	if err != nil || !strings.HasPrefix(prompt, "/"+MigrationCommand+" ") {
		t.Fatalf("prompt failed: %v", err)
	}
	var decoded MigrationRequest
	if json.Unmarshal([]byte(strings.TrimPrefix(prompt, "/"+MigrationCommand+" ")), &decoded) != nil || decoded != r {
		t.Fatal("summary was not preserved exactly")
	}
	for _, change := range []func(*MigrationRequest){
		func(r *MigrationRequest) { r.Version = 2 },
		func(r *MigrationRequest) { r.Workspace = "relative" },
		func(r *MigrationRequest) { r.MigrationID = "private\nvalue" },
		func(r *MigrationRequest) { r.SourceGeneration = -1 },
		func(r *MigrationRequest) { r.Summary += " " },
		func(r *MigrationRequest) {
			r.Summary = strings.Repeat("x", maxMigrationSummary+1)
			r.SummarySHA256 = migrationHash(r.Summary)
		},
	} {
		bad := r
		change(&bad)
		if _, err := MigrationPrompt(bad); !errors.Is(err, ErrMigrationInvalid) {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
	r.Summary = strings.Repeat("\x00", maxMigrationSummary)
	r.SummarySHA256 = migrationHash(r.Summary)
	prompt, err = MigrationPrompt(r)
	if err != nil {
		t.Fatalf("bounded escaped request rejected: %v", err)
	}
	frame, err := json.Marshal(map[string]any{"type": "prompt", "id": strings.Repeat("0", 20), "message": prompt})
	if err != nil || len(frame)+1 >= maxFrame {
		t.Fatal("escaped request exceeded frame")
	}
}

func TestCaptureHandoffDoesNotScanOldHistory(t *testing.T) {
	r, _, _, _ := migrationFixture(t)
	path := filepath.Join(r.Workspace, "source.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(map[string]any{"type": "session", "id": "source", "cwd": r.Workspace}); err != nil {
		t.Fatal(err)
	}
	// Sparse historical bytes are deliberately not JSON. They must never be scanned.
	if _, err := file.Seek(400<<20, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("old-history\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	cursor, err := CaptureHandoff(path, "source", r.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadHandoff(cursor); !errors.Is(err, ErrHandoffNotReady) {
		t.Fatalf("empty tail = %v", err)
	}
	appendHandoff(t, path, map[string]any{"type": "compaction", "id": "handoff", "method": "handoff", "summary": r.Summary})
	summary, entryID, hash, err := ReadHandoff(cursor)
	if err != nil || summary != r.Summary || entryID != "handoff" || hash != r.SummarySHA256 {
		t.Fatalf("handoff mismatch: %v", err)
	}
}

func appendHandoff(t *testing.T, path string, entry any) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(entry); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadHandoffRejectsChangesAndBounds(t *testing.T) {
	for _, mode := range []string{"truncate", "replace", "rewrite", "header", "tail-limit", "summary-limit", "duplicate", "wrong-method", "partial", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _, _ := migrationFixture(t)
			path := filepath.Join(r.Workspace, "source.jsonl")
			base := []map[string]any{{"type": "session", "id": "source", "cwd": r.Workspace}, {"type": "message", "id": "old", "parentId": nil}}
			writeMigrationFixture(t, path, base)
			cursor, err := CaptureHandoff(path, "source", r.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			entry := map[string]any{"type": "compaction", "id": "handoff", "method": "handoff", "summary": r.Summary}
			want := ErrMigrationInvalid
			switch mode {
			case "truncate":
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			case "replace":
				replacement := path + ".new"
				writeMigrationFixture(t, replacement, base)
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			case "rewrite":
				base[1]["id"] = "new"
				writeMigrationFixture(t, path, base)
			case "header":
				base[0]["id"] = "change"
				writeMigrationFixture(t, path, base)
			case "tail-limit":
				entry["summary"] = strings.Repeat("x", maxMigrationJournal)
				appendHandoff(t, path, entry)
			case "summary-limit":
				entry["summary"] = strings.Repeat("x", maxMigrationSummary+1)
				appendHandoff(t, path, entry)
			case "duplicate":
				appendHandoff(t, path, entry)
				appendHandoff(t, path, entry)
			case "wrong-method":
				entry["method"] = "normal"
				appendHandoff(t, path, entry)
				want = ErrHandoffNotReady
			case "partial":
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(`{"type":"compaction"`)
				if err != nil {
					t.Fatal(err)
				}
				file.Close()
				want = ErrHandoffNotReady
			case "malformed":
				appendHandoff(t, path, "private non-object")
			}
			if _, _, _, err := ReadHandoff(cursor); !errors.Is(err, want) {
				t.Fatalf("unsafe handoff accepted: %v", err)
			}
		})
	}
}

func TestVerifyMigrationChecksLiveContentAndConfiguredSettings(t *testing.T) {
	for _, mode := range []string{"", "busy", "async", "queued", "streaming", "compacting", "fast", "model", "thinking", "missing-settled", "identity", "rpc-conflict", "branch", "live-missing", "live-conflict", "live-duplicate", "live-extra-user-before", "live-extra-user-after", "live-extra-assistant-before", "live-extra-assistant-after", "live-extra-system-before", "live-extra-system-after", "live-extra-toolResult-before", "live-extra-toolResult-after", "replace-live", "truncate-live", "reject"} {
		t.Run(mode, func(t *testing.T) {
			r, target, settings, _ := migrationFixture(t)
			t.Setenv("OMP_TEST_MIGRATION_MODE", mode)
			client := fixtureClient(t)
			err := client.VerifyMigration(context.Background(), r, target, settings)
			if mode == "" {
				if err != nil {
					t.Fatalf("valid migration failed: %v", err)
				}
				r.Summary = ""
				if err := client.VerifyMigration(context.Background(), r, target, settings); err != nil {
					t.Fatalf("hash-only recovery failed: %v", err)
				}
				return
			}
			want := ErrMigrationInvalid
			if mode == "busy" || mode == "async" || mode == "queued" || mode == "streaming" || mode == "compacting" {
				want = ErrMigrationNotSettled
			}
			if mode == "reject" {
				want = ErrMigrationUnavailable
			}
			if !errors.Is(err, want) {
				t.Fatalf("unsafe migration accepted: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), r.Summary) {
				t.Fatal("error leaked sensitive data")
			}
		})
	}
}

func TestVerifyMigrationRejectsConflictingJournalEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "partial", "marker-missing", "duplicate-marker", "conflicting-marker", "orphan", "cycle", "duplicate-id", "marker-off-chain", "seed-off-chain", "seed-role", "seed-blocks", "seed-content", "seed-trimmed", "header-id", "header-cwd", "journal-limit", "reload", "marker-source", "marker-generation", "marker-target", "marker-workspace", "marker-hash", "marker-version"} {
		t.Run(mode, func(t *testing.T) {
			r, target, settings, entries := migrationFixture(t)
			marker := entries[4]["data"].(map[string]any)
			message := entries[3]["message"].(map[string]any)
			want := ErrMigrationInvalid
			switch mode {
			case "marker-missing":
				entries = entries[:4]
				want = ErrMigrationMarkerNotReady
			case "duplicate-marker", "conflicting-marker":
				copy := map[string]any{"type": "custom", "id": "second", "parentId": "marker", "customType": MigrationMarkerType, "data": marker}
				if mode == "conflicting-marker" {
					copy["data"] = map[string]any{"migrationId": "another"}
				}
				entries = append(entries, copy)
			case "orphan":
				entries = append(entries, map[string]any{"type": "custom", "id": "orphan", "parentId": "missing"})
			case "cycle":
				entries[3]["parentId"] = "marker"
			case "duplicate-id":
				entries[4]["id"] = "seed"
			case "marker-off-chain":
				entries = append(entries, map[string]any{"type": "custom", "id": "other", "parentId": "seed"})
			case "seed-off-chain":
				entries[4]["parentId"] = nil
			case "seed-role":
				message["role"] = "assistant"
			case "seed-blocks":
				message["content"] = []any{map[string]any{"type": "text", "text": r.Summary}, map[string]any{"type": "text", "text": ""}}
			case "seed-content":
				message["content"] = []any{map[string]any{"type": "text", "text": r.Summary + " "}}
			case "seed-trimmed":
				message["content"] = []any{map[string]any{"type": "text", "text": strings.TrimSpace(r.Summary)}}
			case "header-id":
				entries[1]["id"] = "other"
			case "header-cwd":
				entries[1]["cwd"] = "/other"
			case "journal-limit":
				entries[0]["pad"] = strings.Repeat("x", maxMigrationJournal)
			case "reload":
				marker["reloadCompleted"] = false
			case "marker-source":
				marker["sourceSessionId"] = "other"
			case "marker-generation":
				marker["sourceGeneration"] = 4
			case "marker-target":
				marker["targetSessionId"] = "other"
			case "marker-workspace":
				marker["workspace"] = "/other"
			case "marker-hash":
				marker["summarySha256"] = strings.Repeat("0", 64)
			case "marker-version":
				marker["version"] = 2
			}
			writeMigrationFixture(t, target.File, entries)
			switch mode {
			case "missing":
				if err := os.Remove(target.File); err != nil {
					t.Fatal(err)
				}
				want = ErrMigrationMarkerNotReady
			case "empty":
				if err := os.Truncate(target.File, 0); err != nil {
					t.Fatal(err)
				}
				want = ErrMigrationMarkerNotReady
			case "partial":
				file, err := os.OpenFile(target.File, os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString("{")
				if err != nil {
					t.Fatal(err)
				}
				file.Close()
				want = ErrMigrationMarkerNotReady
			}
			if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); !errors.Is(err, want) {
				t.Fatalf("unsafe journal accepted: %v", err)
			}
		})
	}
}

func TestCaptureHandoffRequiresCompleteIdentity(t *testing.T) {
	for _, mode := range []string{"id", "workspace", "partial-header", "partial-tail", "missing"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _, _ := migrationFixture(t)
			path := filepath.Join(r.Workspace, "source.jsonl")
			header := map[string]any{"type": "session", "id": "source", "cwd": r.Workspace}
			if mode == "id" {
				header["id"] = "other"
			}
			if mode == "workspace" {
				header["cwd"] = "/other"
			}
			writeMigrationFixture(t, path, []map[string]any{header})
			switch mode {
			case "partial-header":
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			case "partial-tail":
				file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString("{"); err != nil {
					t.Fatal(err)
				}
				file.Close()
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := CaptureHandoff(path, "source", r.Workspace); err == nil {
				t.Fatal("unsafe capture accepted")
			}
		})
	}
}

func TestMigrationAcceptsNativeTitleFirstJournal(t *testing.T) {
	r, target, settings, entries := migrationFixture(t)
	raw, err := json.Marshal(entries[0])
	if err != nil || len(raw)+1 != 256 {
		t.Fatal("fixture did not preserve the native fixed title slot")
	}
	if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); err != nil {
		t.Fatalf("title-first verification failed: %v", err)
	}
	path := filepath.Join(r.Workspace, "source.jsonl")
	writeMigrationFixture(t, path, []map[string]any{migrationTitleFixture(t), {"type": "session", "id": "source", "cwd": r.Workspace}})
	cursor, err := CaptureHandoff(path, "source", r.Workspace)
	if err != nil {
		t.Fatalf("title-first capture failed: %v", err)
	}
	appendHandoff(t, path, map[string]any{"type": "compaction", "id": "handoff", "method": "handoff", "summary": r.Summary})
	if summary, _, _, err := ReadHandoff(cursor); err != nil || summary != r.Summary {
		t.Fatalf("title-first handoff failed: %v", err)
	}
}

func TestMigrationRejectsMisplacedHeaderAndTitleMetadata(t *testing.T) {
	for _, mode := range []string{"duplicate-header", "misplaced-header", "duplicate-title", "late-title", "malformed-title", "forged-title"} {
		t.Run(mode, func(t *testing.T) {
			r, target, settings, entries := migrationFixture(t)
			switch mode {
			case "duplicate-header":
				entries = append(entries, entries[1])
			case "misplaced-header":
				entries[1], entries[2] = entries[2], entries[1]
			case "duplicate-title":
				entries = append([]map[string]any{entries[0]}, entries...)
			case "late-title":
				entries = append(entries, entries[0])
			case "malformed-title":
				entries[0]["v"] = 2
			case "forged-title":
				entries[0]["customType"] = MigrationMarkerType
			}
			writeMigrationFixture(t, target.File, entries)
			if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); !errors.Is(err, ErrMigrationInvalid) {
				t.Fatalf("unsafe prefix accepted: %v", err)
			}
		})
	}
}

func TestInspectMigrationCandidateValidatesDurableEvidenceOnly(t *testing.T) {
	for _, mode := range []string{"title-first", "legacy", "other-leaf"} {
		t.Run(mode, func(t *testing.T) {
			r, target, _, entries := migrationFixture(t)
			if mode == "legacy" {
				entries = entries[1:]
			}
			if mode == "other-leaf" {
				// Inspection must not pretend the last append is the native leaf.
				entries = append(entries, map[string]any{"type": "custom", "id": "branch", "parentId": "seed"})
			}
			writeMigrationFixture(t, target.File, entries)
			for _, summary := range []string{r.Summary, ""} {
				r.Summary = summary
				got, err := InspectMigrationCandidate(r, target.File)
				if err != nil || got != target {
					t.Fatalf("valid offline candidate rejected: %#v, %v", got, err)
				}
			}
		})
	}
}

func TestInspectMigrationCandidateRejectsUntrustedFiles(t *testing.T) {
	for _, mode := range []string{"migration", "hash", "source", "generation", "workspace", "target", "seed-content", "seed-ancestry", "missing-marker", "duplicate-marker", "orphan", "cycle", "oversized", "symlink", "misplaced-header", "partial"} {
		t.Run(mode, func(t *testing.T) {
			r, target, _, entries := migrationFixture(t)
			r.Summary = ""
			marker := entries[4]["data"].(map[string]any)
			switch mode {
			case "migration":
				marker["migrationId"] = "other"
			case "hash":
				marker["summarySha256"] = strings.Repeat("0", 64)
			case "source":
				marker["sourceSessionId"] = "other"
			case "generation":
				marker["sourceGeneration"] = 4
			case "workspace":
				entries[1]["cwd"] = "/other"
			case "target":
				marker["targetSessionId"] = "other"
			case "seed-content":
				entries[3]["message"] = map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "changed"}}}
			case "seed-ancestry":
				entries[4]["parentId"] = nil
			case "missing-marker":
				entries = entries[:4]
			case "duplicate-marker":
				entries = append(entries, map[string]any{"type": "custom", "id": "duplicate", "parentId": "marker", "customType": MigrationMarkerType, "data": marker})
			case "orphan":
				entries = append(entries, map[string]any{"type": "custom", "id": "orphan", "parentId": "missing"})
			case "cycle":
				entries[3]["parentId"] = "marker"
			case "oversized":
				entries[0]["pad"] = strings.Repeat("x", maxMigrationJournal)
			case "misplaced-header":
				entries[1], entries[2] = entries[2], entries[1]
			}
			writeMigrationFixture(t, target.File, entries)
			path := target.File
			if mode == "symlink" {
				path += ".link"
				if err := os.Symlink(target.File, path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "partial" {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(path, info.Size()-1); err != nil {
					t.Fatal(err)
				}
			}
			got, err := InspectMigrationCandidate(r, path)
			if !errors.Is(err, ErrMigrationInvalid) || got != (SessionInfo{}) {
				t.Fatalf("unsafe offline candidate accepted: %#v, %v", got, err)
			}
		})
	}
}

func insertMigrationFixtureEntry(entries []map[string]any, position int, entry map[string]any) []map[string]any {
	if position == 2 {
		entry["parentId"] = nil
	} else {
		entry["parentId"] = entries[position-1]["id"]
	}
	entries = append(entries, nil)
	copy(entries[position+1:], entries[position:])
	entries[position] = entry
	if position+1 < len(entries) {
		entries[position+1]["parentId"] = entry["id"]
	}
	return entries
}

func TestMigrationRejectsUnexpectedInitializationEntries(t *testing.T) {
	for _, position := range []string{"before-model", "before-seed", "between-seed-marker", "after-marker", "after-marker-metadata"} {
		for _, kind := range []string{"user", "assistant", "duplicate-seed", "unknown", "custom", "compaction", "reset_boundary"} {
			t.Run(position+"/"+kind, func(t *testing.T) {
				r, target, settings, entries := migrationFixture(t)
				extra := map[string]any{"id": "extra", "type": kind}
				if kind == "user" || kind == "assistant" || kind == "duplicate-seed" {
					role, text := kind, "extra"
					if kind == "duplicate-seed" {
						role, text = "user", r.Summary
					}
					extra["type"] = "message"
					extra["message"] = map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}}
				}
				if kind == "custom" {
					extra["customType"] = "unrelated"
				}
				slot := 2
				switch position {
				case "before-seed":
					slot = 3
				case "between-seed-marker":
					slot = 4
				case "after-marker":
					slot = 5
				case "after-marker-metadata":
					entries = insertMigrationFixtureEntry(entries, 5, map[string]any{"id": "thinking", "type": "thinking_level_change", "thinkingLevel": "auto"})
					slot = 6
				}
				entries = insertMigrationFixtureEntry(entries, slot, extra)
				writeMigrationFixture(t, target.File, entries)
				if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); !errors.Is(err, ErrMigrationInvalid) {
					t.Fatalf("unexpected initialization entry accepted live: %v", err)
				}
				r.Summary = ""
				if _, err := InspectMigrationCandidate(r, target.File); !errors.Is(err, ErrMigrationInvalid) {
					t.Fatalf("unexpected initialization entry accepted offline: %v", err)
				}
			})
		}
	}
}

func TestMigrationAcceptsNativeInitializationMetadata(t *testing.T) {
	for _, position := range []string{"before-model", "before-seed", "between-seed-marker", "after-marker", "reversed"} {
		t.Run(position, func(t *testing.T) {
			r, target, settings, entries := migrationFixture(t)
			slot := 2
			switch position {
			case "before-seed":
				slot = 3
			case "between-seed-marker":
				slot = 4
			case "after-marker":
				slot = 5
			}
			thinking := map[string]any{"id": "thinking", "type": "thinking_level_change", "thinkingLevel": "auto"}
			service := map[string]any{"id": "service", "type": "service_tier_change", "serviceTier": "priority"}
			if position == "reversed" {
				thinking, service = service, thinking
			}
			entries = insertMigrationFixtureEntry(entries, slot, thinking)
			entries = insertMigrationFixtureEntry(entries, slot+1, service)
			writeMigrationFixture(t, target.File, entries)
			if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); err != nil {
				t.Fatalf("native initialization metadata rejected live: %v", err)
			}
			r.Summary = ""
			if got, err := InspectMigrationCandidate(r, target.File); err != nil || got != target {
				t.Fatalf("native initialization metadata rejected offline: %#v, %v", got, err)
			}
		})
	}
}

func TestMigrationRequiresPersistedModelOnInitializationChain(t *testing.T) {
	for _, mode := range []string{"missing", "off-chain-only", "unqualified", "empty-provider", "empty-model", "whitespace", "wrong-provider", "wrong-model"} {
		t.Run(mode, func(t *testing.T) {
			r, target, settings, entries := migrationFixture(t)
			switch mode {
			case "missing", "off-chain-only":
				entries[2] = map[string]any{"id": "model", "type": "thinking_level_change", "parentId": nil, "thinkingLevel": "auto"}
				if mode == "off-chain-only" {
					entries = append(entries, map[string]any{"id": "other-model", "type": "model_change", "parentId": nil, "model": "fixture/model"})
					t.Setenv("OMP_TEST_MIGRATION_MODE", "leaf-marker")
				}
			case "unqualified":
				entries[2]["model"] = "model"
			case "empty-provider":
				entries[2]["model"] = "/model"
			case "empty-model":
				entries[2]["model"] = "fixture/"
			case "whitespace":
				entries[2]["model"] = "fixture/model\n"
			case "wrong-provider":
				entries[2]["model"] = "other/model"
			case "wrong-model":
				entries[2]["model"] = "fixture/other"
			}
			writeMigrationFixture(t, target.File, entries)
			if err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings); !errors.Is(err, ErrMigrationInvalid) {
				t.Fatalf("missing or mismatched persisted model accepted live: %v", err)
			}
			r.Summary = ""
			got, err := InspectMigrationCandidate(r, target.File)
			if mode == "wrong-provider" || mode == "wrong-model" {
				if err != nil || got != target {
					t.Fatalf("offline inspection incorrectly required runtime settings: %#v, %v", got, err)
				}
			} else if !errors.Is(err, ErrMigrationInvalid) {
				t.Fatalf("missing or malformed persisted model accepted offline: %v", err)
			}
		})
	}
}

func TestVerifyMigrationUsesFinalActiveChainModel(t *testing.T) {
	for _, mode := range []string{"earlier-mismatch", "latest-mismatch", "after-marker-match", "after-marker-mismatch", "last-match", "unrelated-branch"} {
		t.Run(mode, func(t *testing.T) {
			r, target, settings, entries := migrationFixture(t)
			model := "fixture/model"
			slot := 4
			switch mode {
			case "earlier-mismatch":
				entries[2]["model"] = "fixture/other"
			case "latest-mismatch":
				model = "fixture/other"
			case "after-marker-match":
				slot = 5
			case "after-marker-mismatch":
				slot, model = 5, "fixture/other"
			case "last-match":
				entries = insertMigrationFixtureEntry(entries, 5, map[string]any{"id": "intermediate-model", "type": "model_change", "model": "fixture/other"})
				slot = 6
			case "unrelated-branch":
				t.Setenv("OMP_TEST_MIGRATION_MODE", "leaf-marker")
			}
			if mode == "unrelated-branch" {
				entries = append(entries, map[string]any{"id": "other-model", "type": "model_change", "parentId": "model", "model": "fixture/other"})
			} else {
				entries = insertMigrationFixtureEntry(entries, slot, map[string]any{"id": "final-model", "type": "model_change", "model": model})
			}
			writeMigrationFixture(t, target.File, entries)
			err := fixtureClient(t).VerifyMigration(context.Background(), r, target, settings)
			if mode == "latest-mismatch" || mode == "after-marker-mismatch" {
				if !errors.Is(err, ErrMigrationInvalid) {
					t.Fatalf("final persisted model mismatch accepted: %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid final active-chain model rejected: %v", err)
			}
		})
	}
}
