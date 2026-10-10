package omp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

var fixtureCatalogPending map[string]any
var fixtureCatalogQueryCount int

func fixtureCatalogCommand(command map[string]any) bool {
	mode := os.Getenv("OMP_TEST_CATALOG_MODE")
	if mode == "" {
		return false
	}
	emit := func(value any) {
		data, _ := json.Marshal(value)
		fmt.Println(string(data))
	}
	response := func(data any) map[string]any {
		return map[string]any{"type": "response", "id": command["id"], "command": command["type"], "success": true, "data": data}
	}
	commands := func(name string) []map[string]any {
		return []map[string]any{{"name": name, "aliases": []string{"alias"}, "description": rpcSensitiveCanary, "source": "extension", "input": map[string]any{"hint": rpcSensitiveCanary}}}
	}
	update := func(name string) {
		emit(map[string]any{"type": "available_commands_update", "commands": commands(name)})
	}
	sourceCommands := func(replaced bool) []map[string]any {
		sources := []any{"builtin", "extension", "file", "custom", "skill", "mcp_prompt", "future", nil, true, false, 42, 1.5, map[string]any{"kind": "builtin"}, []string{"builtin"}, "", " builtin", "Builtin"}
		if replaced {
			sources[0], sources[1], sources[2], sources[3] = "extension", "file", "extension", "future"
			sources[6], sources[7], sources[8] = "builtin", "skill", "mcp_prompt"
		}
		entries := make([]map[string]any, 0, len(sources)+1)
		for index, source := range sources {
			entries = append(entries, map[string]any{
				"name":    fmt.Sprintf("source-%d", index),
				"aliases": []string{fmt.Sprintf("alias-%d", index)},
				"source":  source,
			})
		}
		return append(entries, map[string]any{"name": "opaque command", "aliases": []string{"opaque\talias"}})
	}
	switch command["type"] {
	case "negotiate_protocol":
		if mode == "startup" {
			update("startup")
		}
		if mode == "unicode" {
			update("\U0001f469\u200d\U0001f4bb")
		}
		if mode == "spaced" {
			update("code review")
		}
		if mode == "sources" {
			emit(map[string]any{"type": "available_commands_update", "commands": sourceCommands(false)})
		}
		return false
	case "get_state":
		emit(response(map[string]any{"sessionId": "session-one"}))
	case "get_available_commands":
		fixtureCatalogQueryCount++
		data := map[string]any{"commands": commands("queried")}
		reply := response(data)
		switch mode {
		case "empty":
			data["commands"] = []any{}
		case "sources":
			data["commands"] = sourceCommands(fixtureCatalogQueryCount > 1)
		case "unicode":
			data["commands"] = []map[string]any{{
				"name":    "\U0001f469\u200d\U0001f4bb",
				"aliases": []string{"code\u200cname", "word\u2060joiner"},
				"source":  "file",
			}}
		case "spaced":
			data["commands"] = []map[string]any{{
				"name":    "code review",
				"aliases": []string{"code\treview", "code\nreview"},
				"source":  "file",
			}}
		case "unsupported":
			reply["success"] = false
			reply["error"] = "Unknown command: get_available_commands"
		case "rejected":
			reply["success"] = false
			reply["error"] = rpcSensitiveCanary
		case "transport":
			os.Exit(0)
		case "missing":
			delete(data, "commands")
		case "null":
			data["commands"] = nil
		case "invalid_name":
			data["commands"] = []any{map[string]any{"name": "/invalid"}}
		case "invalid_alias":
			data["commands"] = []any{map[string]any{"name": "valid", "aliases": []any{42}}}
		case "null_alias":
			data["commands"] = []any{map[string]any{"name": "valid", "aliases": nil}}
		case "update_first":
			update("updated")
		case "deferred", "deferred_rejected":
			if mode == "deferred_rejected" {
				reply["success"] = false
				reply["error"] = rpcSensitiveCanary
			}
			fixtureCatalogPending = reply
			emit(map[string]any{"type": "catalog_waiting"})
			return true
		}
		emit(reply)
	case "catalog_release":
		if fixtureCatalogPending != nil {
			emit(fixtureCatalogPending)
			fixtureCatalogPending = nil
		}
		emit(response(map[string]any{"queries": fixtureCatalogQueryCount}))
	case "catalog_publish":
		update(command["name"].(string))
		emit(response(nil))
	case "catalog_publish_sources":
		emit(map[string]any{"type": "available_commands_update", "commands": sourceCommands(command["replaced"].(bool))})
		emit(response(nil))
	case "catalog_session":
		emit(map[string]any{"type": "session_info_update", "sessionId": command["sessionId"], "title": rpcSensitiveCanary})
		emit(response(nil))
	case "catalog_malformed_update":
		emit(map[string]any{"type": "available_commands_update", "commands": []any{map[string]any{"name": 42}}})
	default:
		return false
	}
	return true
}

func catalogFixture(t *testing.T, mode string) *Client {
	t.Helper()
	t.Setenv("OMP_TEST_CATALOG_MODE", mode)
	return fixtureClient(t)
}

func catalogCall(t *testing.T, client *Client, command string, fields map[string]any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := client.Call(ctx, command, fields)
	if err != nil {
		t.Fatalf("catalog fixture control failed: %v", err)
	}
	return data
}

func catalogRefresh(t *testing.T, client *Client) (uint64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return client.RefreshCommandCatalog(ctx)
}

type catalogRefreshResult struct {
	epoch uint64
	err   error
}

func startCatalogRefresh(t *testing.T, client *Client, ctx context.Context) <-chan catalogRefreshResult {
	t.Helper()
	finished := make(chan catalogRefreshResult, 1)
	go func() {
		epoch, err := client.RefreshCommandCatalog(ctx)
		finished <- catalogRefreshResult{epoch: epoch, err: err}
	}()
	if event := nextSessionEvent(t, client); event["type"] != "catalog_waiting" {
		t.Fatal("catalog query did not reach the subprocess")
	}
	return finished
}

func finishCatalogRefresh(t *testing.T, finished <-chan catalogRefreshResult) (uint64, error) {
	t.Helper()
	select {
	case result := <-finished:
		return result.epoch, result.err
	case <-time.After(5 * time.Second):
		t.Fatal("catalog refresh did not finish")
		return 0, nil
	}
}

func TestCommandCatalogInitialUpdateAndImmutableSnapshots(t *testing.T) {
	client := catalogFixture(t, "startup")
	initial := client.CommandCatalog()
	if initial.State != CatalogReady || !initial.HasCommand("startup") || !initial.HasCommand("alias") || initial.HasCommand("/startup") || !initial.HasExtensionCommand("startup") || !initial.HasExtensionCommand("alias") || initial.HasBuiltinCommand("startup") {
		t.Fatal("startup catalog was not available before Start returned")
	}
	// get_state establishes the initial identity without erasing the startup update.
	catalogCall(t, client, "get_state", nil)
	if !client.CommandCatalog().HasCommand("startup") {
		t.Fatal("initial session identity erased startup catalog")
	}
	catalogCall(t, client, "catalog_publish", map[string]any{"name": "second"})
	catalogCall(t, client, "catalog_publish", map[string]any{"name": "third"})
	current := client.CommandCatalog()
	if !current.HasCommand("third") || current.HasCommand("second") || !initial.HasCommand("startup") || initial.HasCommand("third") || !current.HasExtensionCommand("third") || !initial.HasExtensionCommand("startup") || initial.HasExtensionCommand("third") {
		t.Fatal("catalog replacement mutated a retained snapshot")
	}
	// These older notifications are deliberately consumed only after both updates.
	for range 3 {
		if event := nextSessionEvent(t, client); event["type"] != "available_commands_update" || !client.CommandCatalog().HasCommand("third") {
			t.Fatal("delayed worker notification regressed the authoritative snapshot")
		}
	}
	if _, err := catalogRefresh(t, client); err != nil || !client.CommandCatalog().HasCommand("queried") {
		t.Fatalf("query did not replace the catalog: %v", err)
	}
	if !initial.HasCommand("startup") || !initial.HasExtensionCommand("alias") || !client.CommandCatalog().HasExtensionCommand("queried") {
		t.Fatal("query mutated a retained snapshot")
	}
}

func TestCommandCatalogSourceRecognitionAndReplacement(t *testing.T) {
	client := catalogFixture(t, "sources")
	initial := client.CommandCatalog()
	assertSources := func(snapshot CommandCatalog, replaced bool) {
		t.Helper()
		if snapshot.State != CatalogReady {
			t.Fatal("source catalog was not ready")
		}
		iterated := make(map[string]bool)
		for name := range snapshot.ExecutableCommands {
			if iterated[name] {
				t.Fatalf("executable iterator repeated command %q", name)
			}
			iterated[name] = true
		}
		expectedCount := 0
		for index := range 17 {
			builtin := (!replaced && index == 0) || (replaced && index == 6)
			extension := (!replaced && index == 1) || (replaced && (index == 0 || index == 2))
			executable := index == 4 || index == 5 || (!replaced && (index == 0 || index == 2 || index == 3)) || (replaced && (index == 1 || index == 6 || index == 7 || index == 8))
			for _, name := range []string{fmt.Sprintf("source-%d", index), fmt.Sprintf("alias-%d", index)} {
				if !snapshot.HasCommand(name) || snapshot.HasBuiltinCommand(name) != builtin || snapshot.HasExtensionCommand(name) != extension || snapshot.HasExecutableCommand(name) != executable {
					t.Fatalf("advertised command %q lost its source classification after replacement=%t", name, replaced)
				}
				if iterated[name] != executable {
					t.Fatalf("executable iterator changed source whitelist for %q after replacement=%t", name, replaced)
				}
				if executable {
					expectedCount++
				}
			}
		}
		if len(iterated) != expectedCount {
			t.Fatal("executable iterator included unadvertised or unknown-source commands")
		}
		for _, name := range []string{"opaque command", "opaque\talias"} {
			if !snapshot.HasCommand(name) || snapshot.HasBuiltinCommand(name) || snapshot.HasExtensionCommand(name) || snapshot.HasExecutableCommand(name) {
				t.Fatal("source-less opaque command lost lookup or gained a known source")
			}
		}
		for _, name := range []string{"absent", "/source-0", "/alias-1"} {
			if snapshot.HasCommand(name) || snapshot.HasBuiltinCommand(name) || snapshot.HasExtensionCommand(name) || snapshot.HasExecutableCommand(name) {
				t.Fatal("unadvertised command gained recognition")
			}
		}
	}
	assertSources(initial, false)
	var yielded int
	initial.ExecutableCommands(func(string) bool {
		yielded++
		return false
	})
	if yielded != 1 {
		t.Fatal("executable iterator ignored early termination")
	}
	for _, state := range []CommandCatalogState{CatalogUnknown, CatalogUnsupported} {
		unavailable := initial
		unavailable.State = state
		unavailable.ExecutableCommands(func(string) bool {
			t.Fatal("unavailable catalog yielded executable commands")
			return true
		})
		for index := range 17 {
			for _, name := range []string{fmt.Sprintf("source-%d", index), fmt.Sprintf("alias-%d", index)} {
				if unavailable.HasCommand(name) || unavailable.HasBuiltinCommand(name) || unavailable.HasExtensionCommand(name) || unavailable.HasExecutableCommand(name) {
					t.Fatal("unavailable catalog retained source recognition")
				}
			}
		}
	}
	if _, err := catalogRefresh(t, client); err != nil {
		t.Fatalf("malformed-source discovery query failed: %v", err)
	}
	assertSources(client.CommandCatalog(), false)
	assertSources(initial, false)
	if _, err := catalogRefresh(t, client); err != nil {
		t.Fatalf("source discovery query failed: %v", err)
	}
	current := client.CommandCatalog()
	if current.Revision <= initial.Revision || current.Epoch != initial.Epoch {
		t.Fatal("source replacement did not advance within the same catalog scope")
	}
	assertSources(current, true)
	assertSources(initial, false)
	if event := nextSessionEvent(t, client); event["type"] != "available_commands_update" {
		t.Fatal("source update notification was lost")
	}
	assertSources(client.CommandCatalog(), true)
	catalogCall(t, client, "catalog_publish_sources", map[string]any{"replaced": false})
	updated := client.CommandCatalog()
	if updated.Revision <= current.Revision || updated.Epoch != current.Epoch {
		t.Fatal("source update did not advance within the same catalog scope")
	}
	assertSources(updated, false)
	assertSources(current, true)
	assertSources(initial, false)
	if event := nextSessionEvent(t, client); event["type"] != "available_commands_update" {
		t.Fatal("source replacement update notification was lost")
	}
	assertSources(client.CommandCatalog(), false)
	client.InvalidateCommandCatalog()
	invalidated := client.CommandCatalog()
	invalidated.ExecutableCommands(func(string) bool {
		t.Fatal("invalidated catalog yielded executable commands")
		return true
	})
	for index := range 17 {
		for _, name := range []string{fmt.Sprintf("source-%d", index), fmt.Sprintf("alias-%d", index)} {
			if invalidated.HasCommand(name) || invalidated.HasBuiltinCommand(name) || invalidated.HasExtensionCommand(name) || invalidated.HasExecutableCommand(name) {
				t.Fatal("invalidated catalog retained source recognition")
			}
		}
	}
	assertSources(current, true)
	assertSources(updated, false)
	assertSources(initial, false)
}

func TestCommandCatalogUnicodeNamesAndAliasesRemainAvailable(t *testing.T) {
	client := catalogFixture(t, "unicode")
	name := "\U0001f469\u200d\U0001f4bb"
	if snapshot := client.CommandCatalog(); snapshot.State != CatalogReady || !snapshot.HasCommand(name) {
		t.Fatal("startup update rejected a native Unicode command name")
	}
	if _, err := catalogRefresh(t, client); err != nil {
		t.Fatalf("Unicode command query failed: %v", err)
	}
	snapshot := client.CommandCatalog()
	for _, command := range []string{name, "code\u200cname", "word\u2060joiner"} {
		if !snapshot.HasCommand(command) || snapshot.HasBuiltinCommand(command) {
			t.Fatalf("native Unicode command %q lost exact lookup or enabled builtin syntax", command)
		}
	}
	updated := "updated\u200cname"
	catalogCall(t, client, "catalog_publish", map[string]any{"name": updated})
	if current := client.CommandCatalog(); !current.HasCommand(updated) || current.HasCommand(name) {
		t.Fatal("Unicode update did not replace the available commands")
	}
	catalogCall(t, client, "get_state", nil)
}

func TestCommandCatalogOpaqueNamesDoNotTerminateRuntime(t *testing.T) {
	client := catalogFixture(t, "spaced")
	if initial := client.CommandCatalog(); initial.State != CatalogReady || !initial.HasCommand("code review") {
		t.Fatal("startup catalog rejected a native name containing a space")
	}
	if _, err := catalogRefresh(t, client); err != nil {
		t.Fatalf("native names prevented catalog discovery: %v", err)
	}
	queried := client.CommandCatalog()
	for _, name := range []string{"code review", "code\treview", "code\nreview"} {
		if !queried.HasCommand(name) || queried.HasBuiltinCommand(name) {
			t.Fatalf("opaque native name %q lost lookup or enabled builtin syntax", name)
		}
	}
	updated := " next command "
	catalogCall(t, client, "catalog_publish", map[string]any{"name": updated})
	current := client.CommandCatalog()
	if !current.HasCommand(updated) || current.HasCommand("next command") || current.HasCommand("code review") {
		t.Fatal("catalog update normalized names or failed to replace the snapshot")
	}
	catalogCall(t, client, "get_state", nil)
}

func TestCommandCatalogInvalidNameTypePreservesSnapshotAndFailsClient(t *testing.T) {
	client := catalogFixture(t, "startup")
	retained := client.CommandCatalog()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.Call(ctx, "catalog_malformed_update", nil)
	if err == nil || ClassifyError(err) != "protocol" || !retained.HasCommand("startup") || !client.CommandCatalog().HasCommand("startup") {
		t.Fatalf("invalid name type failed to preserve discovery and fail the client: %v", err)
	}
}

func TestCommandCatalogQueryCannotOverwriteWireUpdate(t *testing.T) {
	client := catalogFixture(t, "update_first")
	if _, err := catalogRefresh(t, client); err != nil {
		t.Fatal(err)
	}
	if snapshot := client.CommandCatalog(); snapshot.State != CatalogReady || !snapshot.HasCommand("updated") || snapshot.HasCommand("queried") {
		t.Fatal("query response overwrote the update received first")
	}
	if event := nextSessionEvent(t, client); event["type"] != "available_commands_update" || !client.CommandCatalog().HasCommand("updated") {
		t.Fatal("catalog update notification was lost")
	}
}

func TestCommandCatalogUpdateIsVisibleWhileQueryPending(t *testing.T) {
	client := catalogFixture(t, "deferred")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := startCatalogRefresh(t, client, ctx)
	catalogCall(t, client, "catalog_publish", map[string]any{"name": "live"})
	if !client.CommandCatalog().HasCommand("live") {
		t.Fatal("catalog update waited for query completion")
	}
	select {
	case result := <-finished:
		t.Fatalf("fixture query unexpectedly completed: %v", result.err)
	default:
	}
	catalogCall(t, client, "catalog_release", nil)
	if _, err := finishCatalogRefresh(t, finished); err != nil || !client.CommandCatalog().HasCommand("live") {
		t.Fatalf("late query regressed live catalog: %v", err)
	}
}

func TestCommandCatalogSessionScopeFencesResponses(t *testing.T) {
	for _, scope := range []string{"explicit", "session_event", "session_event_then_update"} {
		t.Run(scope, func(t *testing.T) {
			client := catalogFixture(t, "deferred")
			catalogCall(t, client, "get_state", nil)
			catalogCall(t, client, "catalog_publish", map[string]any{"name": "old"})
			nextSessionEvent(t, client)
			retained := client.CommandCatalog()
			id := client.ID()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			finished := startCatalogRefresh(t, client, ctx)
			if scope == "explicit" {
				client.InvalidateCommandCatalog()
			} else {
				catalogCall(t, client, "catalog_session", map[string]any{"sessionId": "session-two"})
			}
			if snapshot := client.CommandCatalog(); snapshot.State != CatalogUnknown || snapshot.HasCommand("old") || snapshot.Epoch <= retained.Epoch || client.ID() != id {
				t.Fatal("scope change failed to invalidate discovery without replacing client")
			}
			if scope == "session_event_then_update" {
				catalogCall(t, client, "catalog_publish", map[string]any{"name": "new"})
			}
			catalogCall(t, client, "catalog_release", nil)
			if _, err := finishCatalogRefresh(t, finished); err != nil {
				t.Fatal(err)
			}
			snapshot := client.CommandCatalog()
			if snapshot.HasCommand("queried") || !retained.HasCommand("old") {
				t.Fatal("stale query crossed scope or invalidation mutated retained snapshot")
			}
			if scope == "session_event_then_update" {
				if !snapshot.HasCommand("new") {
					t.Fatal("old response erased the new session update")
				}
				// Deliver the old-session change notification after the newer catalog.
				nextSessionEvent(t, client)
				nextSessionEvent(t, client)
				if !client.CommandCatalog().HasCommand("new") {
					t.Fatal("delayed scope notification erased current catalog")
				}
			} else if snapshot.State != CatalogUnknown {
				t.Fatal("stale response populated invalidated catalog")
			}
		})
	}
}

func TestCommandCatalogSameSessionUpdatePreservesDiscovery(t *testing.T) {
	client := catalogFixture(t, "startup")
	catalogCall(t, client, "get_state", nil)
	catalogCall(t, client, "catalog_session", map[string]any{"sessionId": "session-one"})
	if !client.CommandCatalog().HasCommand("startup") {
		t.Fatal("title-only session update erased discovery")
	}
}

// Block entry before query selection so a session transition can win deterministically.
type catalogEntryContext struct {
	context.Context
	entering chan struct{}
	proceed  <-chan struct{}
}

func (ctx catalogEntryContext) Err() error {
	select {
	case ctx.entering <- struct{}{}:
	default:
	}
	select {
	case <-ctx.proceed:
	case <-ctx.Context.Done():
	}
	return ctx.Context.Err()
}

func TestCommandCatalogRejectionCreatedAfterSessionTransition(t *testing.T) {
	client := catalogFixture(t, "deferred_rejected")
	catalogCall(t, client, "get_state", nil)
	callerEpoch := client.CommandCatalog().Epoch
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entering := make(chan struct{}, 1)
	proceed := make(chan struct{})
	finished := make(chan catalogRefreshResult, 1)
	go func() {
		epoch, err := client.RefreshCommandCatalog(catalogEntryContext{Context: ctx, entering: entering, proceed: proceed})
		finished <- catalogRefreshResult{epoch: epoch, err: err}
	}()
	select {
	case <-entering:
	case <-ctx.Done():
		t.Fatal("refresh did not reach the query-entry gate")
	}
	catalogCall(t, client, "catalog_session", map[string]any{"sessionId": "session-two"})
	if event := nextSessionEvent(t, client); event["type"] != "session_info_update" {
		t.Fatal("session transition notification was lost")
	}
	current := client.CommandCatalog()
	if current.Epoch == callerEpoch || current.SessionID != "session-two" {
		t.Fatal("session did not transition before query selection")
	}
	close(proceed)
	if event := nextSessionEvent(t, client); event["type"] != "catalog_waiting" {
		t.Fatal("new-session query did not reach the subprocess")
	}
	catalogCall(t, client, "catalog_release", nil)
	epoch, err := finishCatalogRefresh(t, finished)
	if ClassifyError(err) != "rejected" || epoch != current.Epoch || epoch == callerEpoch {
		t.Fatalf("current-session rejection was assigned an old scope: epoch=%d current=%d caller=%d err=%v", epoch, current.Epoch, callerEpoch, err)
	}
	if snapshot := client.CommandCatalog(); snapshot.Epoch != epoch || snapshot.State != CatalogUnknown || snapshot.SessionID != "session-two" {
		t.Fatal("current-session rejection changed discovery or lost its rejection scope")
	}
}

func TestCommandCatalogRejectionJoinedAfterSessionTransition(t *testing.T) {
	client := catalogFixture(t, "deferred_rejected")
	catalogCall(t, client, "get_state", nil)
	oldEpoch := client.CommandCatalog().Epoch
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := startCatalogRefresh(t, client, ctx)
	catalogCall(t, client, "catalog_session", map[string]any{"sessionId": "session-two"})
	nextSessionEvent(t, client)
	catalogCall(t, client, "catalog_publish", map[string]any{"name": "new"})
	nextSessionEvent(t, client)
	current := client.CommandCatalog()
	if current.Epoch == oldEpoch || current.SessionID != "session-two" || !current.HasCommand("new") {
		t.Fatal("new session discovery was not established while the old query remained pending")
	}
	waiting := make(chan struct{}, 1)
	joined := make(chan catalogRefreshResult, 1)
	go func() {
		epoch, err := client.RefreshCommandCatalog(catalogWaitContext{Context: ctx, waiting: waiting})
		joined <- catalogRefreshResult{epoch: epoch, err: err}
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("new-session caller did not join the pending query")
	}
	data := catalogCall(t, client, "catalog_release", nil)
	for _, waiter := range []<-chan catalogRefreshResult{finished, joined} {
		epoch, err := finishCatalogRefresh(t, waiter)
		if ClassifyError(err) != "rejected" || epoch != oldEpoch || epoch == current.Epoch {
			t.Fatalf("old rejection could retire the newer session: epoch=%d old=%d current=%d err=%v", epoch, oldEpoch, current.Epoch, err)
		}
	}
	var counts struct {
		Queries int `json:"queries"`
	}
	if json.Unmarshal(data, &counts) != nil || counts.Queries != 1 {
		t.Fatal("joining an old-scope rejection issued an overlapping query")
	}
	if snapshot := client.CommandCatalog(); snapshot.Epoch != current.Epoch || snapshot.Revision != current.Revision || !snapshot.HasCommand("new") {
		t.Fatal("old rejection changed the newer session catalog")
	}
}

type catalogWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (ctx catalogWaitContext) Done() <-chan struct{} {
	select {
	case ctx.waiting <- struct{}{}:
	default:
	}
	return ctx.Context.Done()
}

func TestCommandCatalogCancellationDoesNotOverlapDiscovery(t *testing.T) {
	client := catalogFixture(t, "deferred")
	client.InvalidateCommandCatalog()
	queryEpoch := client.CommandCatalog().Epoch
	ctx, cancel := context.WithCancel(context.Background())
	finished := startCatalogRefresh(t, client, ctx)
	cancel()
	if epoch, err := finishCatalogRefresh(t, finished); !errors.Is(err, context.Canceled) || epoch != queryEpoch {
		t.Fatalf("refresh lost cancellation or its query scope: epoch=%d want=%d err=%v", epoch, queryEpoch, err)
	}
	// An already-cancelled waiter does not abandon the sent request or issue another.
	if epoch, err := client.RefreshCommandCatalog(ctx); !errors.Is(err, context.Canceled) || epoch != 0 {
		t.Fatalf("cancelled refresh selected a query: epoch=%d err=%v", epoch, err)
	}
	// A live concurrent waiter must join discovery, not issue a second request.
	waitCtx, stopWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopWait()
	waiting := make(chan struct{}, 1)
	joined := make(chan catalogRefreshResult, 1)
	go func() {
		epoch, err := client.RefreshCommandCatalog(catalogWaitContext{Context: waitCtx, waiting: waiting})
		joined <- catalogRefreshResult{epoch: epoch, err: err}
	}()
	select {
	case <-waiting:
	case <-waitCtx.Done():
		t.Fatal("concurrent waiter never entered discovery")
	}
	data := catalogCall(t, client, "catalog_release", nil)
	if epoch, err := finishCatalogRefresh(t, joined); err != nil || epoch != queryEpoch {
		t.Fatalf("concurrent refresh did not join the original discovery scope: epoch=%d want=%d err=%v", epoch, queryEpoch, err)
	}
	var counts struct {
		Queries int `json:"queries"`
	}
	if json.Unmarshal(data, &counts) != nil || counts.Queries != 1 || !client.CommandCatalog().HasCommand("queried") {
		t.Fatal("cancellation overlapped discovery or lost its late response")
	}
}

func TestCommandCatalogEmptyUnsupportedAndFailures(t *testing.T) {
	for _, mode := range []string{"empty", "unsupported", "rejected", "transport", "missing", "null", "invalid_name", "invalid_alias", "null_alias"} {
		t.Run(mode, func(t *testing.T) {
			client := catalogFixture(t, mode)
			if snapshot := client.CommandCatalog(); snapshot.State != CatalogUnknown || snapshot.HasCommand("queried") {
				t.Fatal("discovery was not initially unknown")
			}
			_, err := catalogRefresh(t, client)
			snapshot := client.CommandCatalog()
			switch mode {
			case "empty":
				if err != nil || snapshot.State != CatalogReady || snapshot.HasCommand("queried") || snapshot.HasCommand("alias") {
					t.Fatalf("ready empty catalog was conflated with unavailable: %v", err)
				}
			case "unsupported":
				if err != nil || snapshot.State != CatalogUnsupported {
					t.Fatalf("explicit unsupported rejection not recognized: %v", err)
				}
			case "rejected":
				if err == nil || ClassifyError(err) != "rejected" || strings.Contains(err.Error(), "SECRET") || snapshot.State != CatalogUnknown {
					t.Fatalf("ordinary rejection mistaken for unsupported or leaked diagnostics: %v", err)
				}
			case "transport":
				if err == nil || snapshot.State != CatalogUnknown {
					t.Fatal("transport failure mistaken for unsupported")
				}
			default:
				if err == nil || ClassifyError(err) != "protocol" || snapshot.State != CatalogUnknown {
					t.Fatalf("malformed catalog did not fail as protocol: %v", err)
				}
			}
			if mode == "empty" || mode == "unsupported" || mode == "rejected" {
				catalogCall(t, client, "prompt", map[string]any{"message": "ordinary prompt"})
			}
		})
	}
}

func TestCommandCatalogLogsExcludeRecognitionAndMetadata(t *testing.T) {
	for _, mode := range []string{"startup", "rejected", "invalid_alias"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("OMP_TEST_CATALOG_MODE", mode)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			logger, output := bufferedRPCLogger("json")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Start(ctx, Config{Binary: binary}, logger)
			if err != nil {
				t.Fatal(err)
			}
			catalogCall(t, client, "catalog_publish", map[string]any{"name": "SECRET_CATALOG_NAME"})
			_, _ = client.RefreshCommandCatalog(ctx)
			_ = client.Close()
			if logs := output.String(); strings.Contains(logs, "SECRET") || strings.Contains(logs, "Authorization") || strings.Contains(logs, "https://api.telegram.org") {
				t.Fatal("catalog recognition, description, or native rejection leaked into logs")
			}
		})
	}
}
