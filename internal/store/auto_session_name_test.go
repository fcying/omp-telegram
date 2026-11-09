package store

import "testing"

func autoSessionNameBinding() Binding {
	return Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspace", Session: "/sessions/new.jsonl", SessionID: "new-session", Generation: 1, Running: true, AutoSessionNamePending: true}
}

func TestAutoSessionNamePendingSurvivesReopenAndConsumesOnce(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	b := autoSessionNameBinding()
	intent := StartIntent{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread, Kind: "new", Workspace: b.Workspace, Generation: b.Generation}
	requireStoreOK(t, s.PrepareStart(Binding{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread}, intent))
	requireStoreOK(t, s.CommitStart(b))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err := s.Binding(b.Bot, b.Chat, b.Thread)
	requireStoreOK(t, err)
	if got != b {
		t.Fatalf("reopened binding = %+v, want %+v", got, b)
	}
	entries, err := s.BindingsForChat(b.Bot, b.Chat)
	requireStoreOK(t, err)
	if len(entries) != 1 || entries[0].Binding == nil || *entries[0].Binding != b {
		t.Fatalf("listed binding lost eligibility: %+v", entries)
	}
	running, err := s.RunningBindings(b.Bot)
	requireStoreOK(t, err)
	if len(running) != 1 || running[0] != b {
		t.Fatalf("running binding lost eligibility: %+v", running)
	}
	consumed, err := s.ConsumeAutoSessionName(b)
	requireStoreOK(t, err)
	if !consumed {
		t.Fatal("eligible binding was not consumed")
	}
	consumed, err = s.ConsumeAutoSessionName(b)
	requireStoreOK(t, err)
	if consumed {
		t.Fatal("eligibility was consumed twice")
	}
	// A stale runtime snapshot must not re-grant consumed eligibility.
	requireStoreOK(t, s.Save(b))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err = s.Binding(b.Bot, b.Chat, b.Thread)
	requireStoreOK(t, err)
	if got.AutoSessionNamePending {
		t.Fatal("consumption did not remain durable")
	}
}

func TestAutoSessionNameConsumeRequiresCompleteIdentity(t *testing.T) {
	changes := map[string]func(*Binding){
		"bot":          func(b *Binding) { b.Bot++ },
		"chat":         func(b *Binding) { b.Chat++ },
		"thread":       func(b *Binding) { b.Thread++ },
		"generation":   func(b *Binding) { b.Generation++ },
		"session-id":   func(b *Binding) { b.SessionID = "other-session" },
		"session-file": func(b *Binding) { b.Session = "/sessions/other.jsonl" },
		"workspace":    func(b *Binding) { b.Workspace = "/other" },
		"pending":      func(b *Binding) { b.AutoSessionNamePending = false },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			b := autoSessionNameBinding()
			requireStoreOK(t, s.Save(b))
			stale := b
			change(&stale)
			consumed, err := s.ConsumeAutoSessionName(stale)
			requireStoreOK(t, err)
			if consumed {
				t.Fatal("stale identity consumed eligibility")
			}
			consumed, err = s.ConsumeAutoSessionName(b)
			requireStoreOK(t, err)
			if !consumed {
				t.Fatal("stale request removed current eligibility")
			}
		})
	}
}

func TestAutoSessionNamePendingSurvivesRuntimeBookkeeping(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	b := autoSessionNameBinding()
	requireStoreOK(t, s.Save(b))
	requireStoreOK(t, s.SetRunning(b, false))
	requireStoreOK(t, s.SetInterrupted(b, true))
	changed, err := s.TouchBinding(b.Bot, b.Chat, b.Thread, b.Generation, 123)
	requireStoreOK(t, err)
	if !changed {
		t.Fatal("binding touch did not match")
	}
	b.Running, b.Interrupted, b.LastUsedAt = false, true, 123
	// A same-session runtime save need not carry the persisted one-shot flag.
	b.AutoSessionNamePending = false
	requireStoreOK(t, s.Save(b))
	got, err := s.Binding(b.Bot, b.Chat, b.Thread)
	requireStoreOK(t, err)
	b.AutoSessionNamePending = true
	if got != b {
		t.Fatalf("runtime bookkeeping changed eligibility: %+v", got)
	}
}

func TestAutoSessionNameReplacementDoesNotInheritEligibility(t *testing.T) {
	for _, kind := range []string{"save", "new-disabled", "resume"} {
		t.Run(kind, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			b := autoSessionNameBinding()
			requireStoreOK(t, s.Save(b))
			next := b
			next.Generation++
			next.Session, next.SessionID = "/sessions/replacement.jsonl", "replacement"
			if kind == "save" {
				requireStoreOK(t, s.Save(next))
			} else {
				intentKind := "new"
				if kind == "resume" {
					intentKind = "resume"
				} else {
					next.AutoSessionNamePending = false
				}
				intent := StartIntent{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread, Kind: intentKind, Workspace: next.Workspace, Session: next.Session, Generation: next.Generation}
				requireStoreOK(t, s.PrepareStart(b, intent))
				requireStoreOK(t, s.CommitStart(next))
			}
			got, err := s.Binding(b.Bot, b.Chat, b.Thread)
			requireStoreOK(t, err)
			if got.AutoSessionNamePending {
				t.Fatal("replacement inherited naming eligibility")
			}
		})
	}
}

func TestAutoSessionNameConsumeWaitsForStartupIntent(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	b := autoSessionNameBinding()
	requireStoreOK(t, s.Save(b))
	intent := StartIntent{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread, Kind: "new", Workspace: b.Workspace, Generation: b.Generation + 1}
	requireStoreOK(t, s.PrepareStart(b, intent))
	consumed, err := s.ConsumeAutoSessionName(b)
	requireStoreOK(t, err)
	if consumed {
		t.Fatal("pending start allowed consumption")
	}
	requireStoreOK(t, s.CancelStart(intent))
	consumed, err = s.ConsumeAutoSessionName(b)
	requireStoreOK(t, err)
	if !consumed {
		t.Fatal("cancelled start discarded source eligibility")
	}
}

func TestAutoSessionNameMigrationNeverTransfersEligibility(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "commit"
		if recovery {
			name = "recover-source"
		}
		t.Run(name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			m := migrationFixture(t, s)
			_, err := s.DB.Exec("UPDATE bindings SET auto_session_name_pending=1")
			requireStoreOK(t, err)
			m.Source.AutoSessionNamePending = true
			m = prepareVerifiedMigration(t, s, m)
			consumed, err := s.ConsumeAutoSessionName(m.Source)
			requireStoreOK(t, err)
			if consumed {
				t.Fatal("migration allowed consumption")
			}
			target := migrationTarget(m)
			if recovery {
				target.Session, target.SessionID = m.Source.Session, m.Source.SessionID
				requireStoreOK(t, s.ResolveMigration(m, target, "recovered"))
			} else {
				requireStoreOK(t, s.CommitMigration(m, target, "migrated"))
			}
			got, err := s.Binding(target.Bot, target.Chat, target.Thread)
			requireStoreOK(t, err)
			if got.AutoSessionNamePending {
				t.Fatal("migration transferred source eligibility")
			}
		})
	}
}

func TestAutoSessionNameSchema26UpgradeDoesNotGrantEligibility(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	b := autoSessionNameBinding()
	b.AutoSessionNamePending = false
	requireStoreOK(t, s.Save(b))
	_, err := s.DB.Exec("ALTER TABLE bindings DROP COLUMN auto_session_name_pending; PRAGMA user_version=26")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	got, err := s.Binding(b.Bot, b.Chat, b.Thread)
	requireStoreOK(t, err)
	if got != b {
		t.Fatalf("legacy binding gained eligibility or changed: %+v", got)
	}
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if version != schemaVersion {
		t.Fatalf("upgraded version = %d, want %d", version, schemaVersion)
	}
	consumed, err := s.ConsumeAutoSessionName(autoSessionNameBinding())
	requireStoreOK(t, err)
	if consumed {
		t.Fatal("upgraded legacy binding was eligible")
	}
	if _, err := s.DB.Exec("UPDATE bindings SET auto_session_name_pending=2"); err == nil {
		t.Fatal("eligibility column accepted a non-boolean value")
	}
}

func TestAutoSessionNameConsumeFailureKeepsEligibility(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	b := autoSessionNameBinding()
	requireStoreOK(t, s.Save(b))
	_, err := s.DB.Exec(`CREATE TRIGGER reject_auto_session_name BEFORE UPDATE OF auto_session_name_pending ON bindings BEGIN SELECT RAISE(ABORT,'blocked'); END`)
	requireStoreOK(t, err)
	consumed, err := s.ConsumeAutoSessionName(b)
	if err == nil || consumed {
		t.Fatalf("failed consume = %v, %v", consumed, err)
	}
	got, err := s.Binding(b.Bot, b.Chat, b.Thread)
	requireStoreOK(t, err)
	if !got.AutoSessionNamePending {
		t.Fatal("failed consumption removed eligibility")
	}
}
