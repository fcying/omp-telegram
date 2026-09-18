package bridge

import "testing"

func TestHandoffResultPreservesSessionAndReleasesQueue(t *testing.T) {
	for _, scenario := range []struct{ instructions, result string }{
		{"preserve decisions", "Handoff completed."},
		{"cancel", "Handoff canceled without a result."},
		{"fail", "Handoff failed or its outcome is uncertain."},
	} {
		t.Run(scenario.instructions, func(t *testing.T) {
			f, db, send := setupBridge(t)
			send(update(1, 11, "/new "+t.TempDir()))
			waitBinding(t, db, 11)
			before, err := db.Binding(99, -10, 11)
			if err != nil {
				t.Fatal(err)
			}
			send(update(2, 11, "/handoff "+scenario.instructions))
			waitFor(t, func() bool { return f.has(11, scenario.result) })
			waitInputDone(t, db, 2)
			send(update(3, 11, "after handoff"))
			waitFor(t, func() bool { return f.has(11, "answer: after handoff") })
			waitInputDone(t, db, 3)
			after, err := db.Binding(99, -10, 11)
			if err != nil || after != before {
				t.Fatalf("handoff changed session binding: %+v, %v", after, err)
			}
		})
	}
}

func TestPendingHandoffAllowsHelpCloseAndNewSession(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)
	before, err := db.Binding(99, -10, 11)
	if err != nil {
		t.Fatal(err)
	}
	send(update(2, 11, "/handoff hold"))
	waitInputDone(t, db, 2)
	send(update(3, 11, "/help"))
	waitInputDone(t, db, 3)
	send(update(4, 11, "/close"))
	waitInputDone(t, db, 4)
	send(update(5, 11, "/new"))
	waitInputDone(t, db, 5)
	send(update(6, 11, "after closed handoff"))
	waitFor(t, func() bool { return f.has(11, "answer: after closed handoff") })
	after, err := db.Binding(99, -10, 11)
	if err != nil || after.Generation <= before.Generation || after.Session == before.Session {
		t.Fatalf("could not replace session after pending handoff: %+v, %v", after, err)
	}
	if f.has(11, "Handoff completed.") || f.has(11, "Handoff failed or its outcome is uncertain.") {
		t.Fatal("old-generation handoff published a result after close")
	}
}

func TestHandoffRequiresIdleInstanceAndEmptyQueue(t *testing.T) {
	w, _, command := setupWorkspaceWorker(t)
	command("/handoff")
	if w.client != nil {
		t.Fatal("handoff created an instance")
	}
	command("/new " + t.TempDir())
	w.b.cfg.QueueCapacity = 1
	command("queued work")
	command("/handoff preserve queued work")
	if w.busy || w.compacting || len(w.queue) != 1 {
		t.Fatal("handoff overtook the prompt queue")
	}
	w.clearQueue()
	command("wait")
	w.dispatch()
	active := w.active
	command("/handoff")
	if w.compacting || !w.busy || w.active != active {
		t.Fatal("handoff interrupted the active prompt")
	}
}
