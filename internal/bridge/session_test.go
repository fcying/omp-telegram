package bridge

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestResumeNativeIDRestoresDirectoryAcrossTopics(t *testing.T) {
	first, _, command := setupWorkspaceWorker(t)
	command("/new test")
	if first.client == nil || !validSessionID(first.sessionID) {
		t.Fatal("native session identity unavailable")
	}
	original, id := first.binding, first.sessionID
	ctx, cancel := context.WithCancel(first.ctx)
	second := testWorker(t, &worker{b: first.b, key: target{chat: -10, thread: 22}, ctx: ctx, cancel: cancel, confirms: make(map[string]confirmation)})
	t.Cleanup(func() { second.teardownWorker(true); cancel(); second.background.Wait(); second.drainMediaResults() })
	second.start(true, id, "", false)
	if second.client != nil {
		t.Fatal("same native session opened concurrently in another topic")
	}
	command("/close")
	second.start(true, id, "", false)
	if second.client == nil || second.binding.Session != original.Session || second.binding.Workspace != original.Workspace || second.sessionID != id {
		t.Fatal("ID resume did not restore original native session and cwd")
	}
	first.start(true, id, "", false)
	if first.client != nil {
		t.Fatal("native ID resume bypassed active session ownership")
	}
}

func TestSessionPrefixClaimDoesNotBypassOtherOwner(t *testing.T) {
	current, other := &worker{}, &worker{}
	b := testBridge(t, &Bridge{sessionClaims: map[string]sessionClaim{
		"current": {owner: current, id: "abcdef0123456789"},
		"other":   {owner: other, id: "abcdef0fedcba987"},
	}})
	if !b.sessionInUseByOther(current, "abcdef0") {
		t.Fatal("another owner with the same session prefix was not detected")
	}
	if b.sessionInUseByOther(other, "abcdef0fed") {
		t.Fatal("current owner was incorrectly treated as another owner")
	}
}

func TestInvalidResumeIDPreservesSavedSession(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	if w.client == nil {
		t.Fatal("session did not start")
	}
	command("/close")
	before := w.binding
	savedBefore, err := w.b.db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"--print", "../../session.jsonl", "deadbeef-0000-4000-8000-000000000000"} {
		command("/resume " + id)
		if w.client != nil {
			t.Fatal("invalid ID started an instance")
		}
		saved, err := w.b.db.Binding(99, -10, 11)
		if err != nil || saved != savedBefore || w.binding != before {
			t.Fatal("failed ID resume changed the saved binding")
		}
	}
}

func TestNativeIDResumeDoesNotRecreateMissingDirectory(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/new test")
	if w.client == nil {
		t.Fatal("session did not start")
	}
	cwd, id := w.binding.Workspace, w.sessionID
	command("/close")
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	command("/resume " + id)
	if w.client != nil {
		t.Fatal("native ID resumed without original directory")
	}
	if _, err := os.Stat(cwd); !os.IsNotExist(err) {
		t.Fatal("resume recreated a missing directory")
	}
}

func TestNameChangesTitleWithoutInterruptingTasks(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	w.b.cfg.QueueCapacity = 2
	command("/new " + t.TempDir())
	before := w.binding
	command("wait")
	w.dispatch()
	active := w.active
	command("queued task")
	command("/name Bugfix HAL")
	command("/status")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil {
		t.Fatal(err)
	}
	if statusFields(text)["Session"] != "Bugfix HAL" {
		t.Fatalf("status did not expose native session title: %q", text)
	}
	if !sameBindingIdentity(w.binding, before) || !w.busy || w.active != active || len(w.queue) != 1 {
		t.Fatal("renaming changed session identity or active/queued work")
	}
	raw, err := w.call("get_state", nil)
	var state struct {
		RootPrompts int `json:"fixtureRootPrompts"`
	}
	if err != nil || json.Unmarshal(raw, &state) != nil || state.RootPrompts != 1 {
		t.Fatalf("naming incorrectly started an agent turn: %+v, %v", state, err)
	}
	command("/name")
	command("/status")
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || statusFields(text)["Session"] != "Bugfix HAL" {
		t.Fatal("empty name cleared the existing title")
	}
}

func TestRejectedNamePreservesTitleAndSession(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/name No session")
	if w.client != nil {
		t.Fatal("name command created a session")
	}
	command("/new " + t.TempDir())
	command("/name Existing title")
	command("/model fixture/reject-name")
	before := w.binding
	command("/name Rejected title")
	command("/status")
	var text string
	if err := w.b.db.DB.QueryRow("SELECT text FROM outbox ORDER BY id DESC LIMIT 1").Scan(&text); err != nil || statusFields(text)["Session"] != "Existing title" {
		t.Fatal("rejected name changed the displayed native title")
	}
	if w.client == nil || !sameBindingIdentity(w.binding, before) {
		t.Fatal("rejected metadata update closed or replaced the session")
	}
}
