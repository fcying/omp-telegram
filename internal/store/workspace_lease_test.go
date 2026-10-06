package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWorkspaceLeaseSharedUsersAndConfirmedRelease(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	requireStoreOK(t, s.AdmitWorkspace("/root", a))
	requireStoreOK(t, s.AdmitWorkspace("/root", b))
	if err := s.ClaimResearchWorkspace("/root", a); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("shared claim = %v", err)
	}
	requireStoreOK(t, s.CloseWorkspaceUsers(b))
	requireStoreOK(t, s.ClaimResearchWorkspace("/root", a))
	if err := s.AdmitWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("exclusive admission = %v", err)
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(b))
	if err := s.AdmitWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("foreign release removed lease: %v", err)
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(a))
	requireStoreOK(t, s.AdmitWorkspace("/root", b))
	// Off releases exclusivity, not the normal logical session registration.
	if err := s.ClaimResearchWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("off removed normal owner: %v", err)
	}
}

func TestWorkspaceLeaseCloseAndReopenRetainExactOwner(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	requireStoreOK(t, s.ClaimResearchWorkspace("/root", a))
	requireStoreOK(t, s.CloseWorkspaceUsers(a))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	if err := s.AdmitWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("reopen lost exclusivity: %v", err)
	}
	replacement := a
	replacement.Session = "cccccccc-3333"
	if err := s.CheckWorkspaceSession(replacement); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("replacement accepted old lease: %v", err)
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(replacement))
	if err := s.AdmitWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("new session released old lease: %v", err)
	}
	for _, id := range []string{"AAAAAAAA", a.Session, "AAAAAAAA-1111"} {
		leased, err := s.ResearchSessionLeased(a.Bot, id)
		requireStoreOK(t, err)
		if !leased {
			t.Fatalf("lease history not protected for %s", id)
		}
	}
	requireStoreOK(t, s.AdmitWorkspace("/root", a))
	requireStoreOK(t, s.ReleaseResearchWorkspace(a))
	requireStoreOK(t, s.AdmitWorkspace("/root", b))
}

func TestDeleteClosedBindingPreservesResearchWorkspaceSource(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	previous := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/previous", Session: "/previous.jsonl", Generation: 1}
	requireStoreOK(t, s.Save(previous))
	bound := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/root/sub", Session: "/current.jsonl", SessionID: "aaaaaaaa-1111", Generation: 2}
	requireStoreOK(t, s.Save(bound))
	requireStoreOK(t, s.SetPinnedSession(1, 2, 3, bound.Workspace, bound.SessionID, true))
	owner := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: bound.SessionID}
	requireStoreOK(t, s.ClaimResearchWorkspace("/root", owner))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	deleted, err := s.DeleteClosedBinding(1, 2, 3, previous.Generation)
	requireStoreOK(t, err)
	if deleted {
		t.Fatal("stale request deleted the lease owner's binding")
	}
	deleted, err = s.DeleteClosedBinding(1, 2, 3, bound.Generation)
	if deleted || !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("leased binding deletion = %v, %v", deleted, err)
	}
	current, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if current != bound {
		t.Fatalf("refused deletion changed binding: %+v", current)
	}
	var workspace, session string
	requireStoreOK(t, s.DB.QueryRow("SELECT workspace,session FROM history WHERE bot=1 AND chat=2 AND thread=3").Scan(&workspace, &session))
	if workspace != previous.Workspace || session != previous.Session {
		t.Fatalf("refused deletion changed history: %q, %q", workspace, session)
	}
	pinned, err := s.PinnedSessions(1, 2, 3, bound.Workspace)
	requireStoreOK(t, err)
	if _, ok := pinned[bound.SessionID]; !ok {
		t.Fatal("refused deletion removed the pinned session")
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(owner))
	deleted, err = s.DeleteClosedBinding(1, 2, 3, bound.Generation)
	requireStoreOK(t, err)
	if !deleted {
		t.Fatal("confirmed release did not allow binding deletion")
	}
}

func TestWorkspaceLeaseConcurrentStartupAndClaimAreMutuallyExclusive(t *testing.T) {
	for range 30 {
		s := openTestStore(t, t.TempDir())
		a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
		b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
		requireStoreOK(t, s.AdmitWorkspace("/root", a))
		start := make(chan struct{})
		var claimErr, admitErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; claimErr = s.ClaimResearchWorkspace("/root", a) }()
		go func() {
			defer wg.Done()
			<-start
			previous := Binding{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread}
			intent := StartIntent{Bot: b.Bot, Chat: b.Chat, Thread: b.Thread, Generation: 1, Kind: "new", Workspace: "/root"}
			admitErr = s.PrepareWorkspaceStart(previous, intent, "/root", b)
		}()
		close(start)
		wg.Wait()
		if (claimErr == nil) == (admitErr == nil) {
			t.Fatalf("claim/admit must have exactly one winner: %v / %v", claimErr, admitErr)
		}
		if claimErr != nil && !errors.Is(claimErr, ErrWorkspaceOccupied) {
			t.Fatal(claimErr)
		}
		if admitErr != nil && !errors.Is(admitErr, ErrWorkspaceOccupied) {
			t.Fatal(admitErr)
		}
		intents, err := s.PendingStarts(b.Bot)
		requireStoreOK(t, err)
		if (len(intents) == 1) != (admitErr == nil) {
			t.Fatalf("startup intent and workspace admission diverged: %+v / %v", intents, admitErr)
		}
	}
}

func TestWorkspaceStartupAdmissionAndIntentRollbackTogether(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	previous := Binding{Bot: 1, Chat: 2, Thread: 3, Generation: 1, Running: true}
	requireStoreOK(t, s.Save(previous))
	intent := StartIntent{Bot: 1, Chat: 2, Thread: 3, Generation: 2, Kind: "new", Workspace: "/root"}
	_, err := s.DB.Exec("CREATE TRIGGER reject_start BEFORE INSERT ON startup_intents BEGIN SELECT RAISE(FAIL,'injected startup failure'); END")
	requireStoreOK(t, err)
	if err = s.PrepareWorkspaceStart(previous, intent, "/root", WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "start:2"}); err == nil {
		t.Fatal("injected intent failure accepted")
	}
	var count int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM workspace_users").Scan(&count))
	if count != 0 {
		t.Fatal("failed intent retained an admission")
	}
	binding, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if !binding.Running {
		t.Fatal("failed intent closed the old binding")
	}
}

func TestVersionFourteenMigratesWorkspaceLeaseTables(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	_, err := s.DB.Exec("DROP TABLE workspace_users; DROP TABLE workspace_leases; PRAGMA user_version=14")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	requireStoreOK(t, s.ClaimResearchWorkspace("/root", a))
	var version int
	requireStoreOK(t, s.DB.QueryRow("PRAGMA user_version").Scan(&version))
	if version != schemaVersion {
		t.Fatalf("migration version=%d", version)
	}
}

func retireLeaseTestIntent(t *testing.T, s *Store, intent StartIntent, mode string) error {
	t.Helper()
	switch mode {
	case "cancel":
		return s.CancelStart(intent)
	case "expire":
		removed, err := s.ExpireStart(context.Background(), intent, time.Now().Unix()+1)
		if err == nil && !removed {
			t.Fatal("eligible startup intent was not expired")
		}
		return err
	case "cleanup":
		count, err := s.CleanupExpiredStarts(context.Background(), time.Now().Unix()+1)
		if err == nil && count != 1 {
			t.Fatalf("expired startup count=%d", count)
		}
		return err
	default:
		t.Fatal("unsupported retirement mode")
		return nil
	}
}

func TestRetiredStartupIntentReleasesOnlyNormalOccupancy(t *testing.T) {
	for _, mode := range []string{"cancel", "expire", "cleanup"} {
		for _, retained := range []struct {
			name   string
			leased bool
		}{{"normal", false}, {"leased", true}} {
			retainedLease := retained.leased
			t.Run(mode+"/"+retained.name, func(t *testing.T) {
				s := openTestStore(t, t.TempDir())
				owner := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
				other := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
				if retainedLease {
					requireStoreOK(t, s.ClaimResearchWorkspace("/root", owner))
				}
				previous := Binding{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread}
				intent := StartIntent{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread, Generation: 1, Kind: "resume", Workspace: "/root", Session: owner.Session}
				requireStoreOK(t, s.PrepareWorkspaceStart(previous, intent, "/root", owner))
				requireStoreOK(t, retireLeaseTestIntent(t, s, intent, mode))
				intents, err := s.PendingStarts(owner.Bot)
				requireStoreOK(t, err)
				if len(intents) != 0 {
					t.Fatal("retired intent survived")
				}
				err = s.ClaimResearchWorkspace("/root", other)
				if retainedLease {
					if !errors.Is(err, ErrWorkspaceOccupied) {
						t.Fatalf("retirement discarded exclusive lease: %v", err)
					}
				} else {
					requireStoreOK(t, err)
				}
			})
		}
	}
}

func TestStartupRetirementPreservesStaleFreshAndRunningAdmissions(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	owner := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "start:1"}
	other := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	previous := Binding{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread}
	intent := StartIntent{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread, Generation: 1, Kind: "new", Workspace: "/root"}
	requireStoreOK(t, s.PrepareWorkspaceStart(previous, intent, "/root", owner))
	stale := intent
	stale.Generation++
	requireStoreOK(t, s.CancelStart(stale))
	removed, err := s.ExpireStart(context.Background(), stale, time.Now().Unix()+1)
	requireStoreOK(t, err)
	if removed {
		t.Fatal("stale generation expired current intent")
	}
	removed, err = s.ExpireStart(context.Background(), intent, 1)
	requireStoreOK(t, err)
	if removed {
		t.Fatal("fresh intent was expired")
	}
	count, err := s.CleanupExpiredStarts(context.Background(), 1)
	requireStoreOK(t, err)
	if count != 0 {
		t.Fatal("fresh intent was bulk expired")
	}
	if err = s.ClaimResearchWorkspace("/root", other); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("stale/fresh retirement lost admission: %v", err)
	}

	running := WorkspaceOwner{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread, Session: "cccccccc-3333"}
	requireStoreOK(t, s.AdmitWorkspace("/root", running))
	requireStoreOK(t, s.Save(Binding{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread, Workspace: "/root", SessionID: running.Session, Generation: 2, Running: true}))
	requireStoreOK(t, s.CancelStart(intent))
	var session string
	requireStoreOK(t, s.DB.QueryRow("SELECT session FROM workspace_users").Scan(&session))
	if session != running.Session {
		t.Fatalf("retirement did not preserve running registration: %s", session)
	}
	if err = s.ClaimResearchWorkspace("/root", other); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("running binding occupancy was removed: %v", err)
	}
}

func TestStartupRetirementRollsBackIntentAndNormalAdmissionTogether(t *testing.T) {
	for _, table := range []string{"workspace_users", "startup_intents"} {
		t.Run(table, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			owner := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "start:1"}
			intent := StartIntent{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread, Generation: 1, Kind: "new", Workspace: "/root"}
			requireStoreOK(t, s.PrepareWorkspaceStart(Binding{Bot: owner.Bot, Chat: owner.Chat, Thread: owner.Thread}, intent, "/root", owner))
			_, err := s.DB.Exec("CREATE TRIGGER reject_retirement BEFORE DELETE ON " + table + " BEGIN SELECT RAISE(FAIL,'injected retirement failure'); END")
			requireStoreOK(t, err)
			if err = s.CancelStart(intent); err == nil {
				t.Fatal("injected retirement failure accepted")
			}
			intents, err := s.PendingStarts(owner.Bot)
			requireStoreOK(t, err)
			if len(intents) != 1 {
				t.Fatal("failed retirement lost startup intent")
			}
			other := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
			if err = s.ClaimResearchWorkspace("/root", other); !errors.Is(err, ErrWorkspaceOccupied) {
				t.Fatalf("failed retirement lost ordinary admission: %v", err)
			}
		})
	}
}

func TestWorkspaceRefreshFailureRollsBackNormalUsersAndLeaseAliases(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	requireStoreOK(t, s.ClaimResearchWorkspace("/root/sub", a))
	requireStoreOK(t, s.AdmitWorkspace("/elsewhere", b))
	_, err := s.DB.Exec("CREATE TRIGGER reject_refresh BEFORE INSERT ON workspace_users WHEN NEW.thread=4 BEGIN SELECT RAISE(FAIL,'injected refresh failure'); END")
	requireStoreOK(t, err)
	users := []WorkspaceRegistration{{Root: "/root", Owner: a}, {Root: "/root", Owner: b}}
	aliases := []WorkspaceRegistration{{Root: "/root", Owner: a}}
	if err := s.RefreshWorkspaceRegistrations(users, aliases); err == nil {
		t.Fatal("injected refresh failure accepted")
	}
	var oldUsers, allUsers, oldLeases, allLeases int
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM workspace_users WHERE root IN ('/root/sub','/elsewhere')").Scan(&oldUsers))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM workspace_users").Scan(&allUsers))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases WHERE root='/root/sub'").Scan(&oldLeases))
	requireStoreOK(t, s.DB.QueryRow("SELECT COUNT(*) FROM workspace_leases").Scan(&allLeases))
	if oldUsers != 2 || allUsers != 2 || oldLeases != 1 || allLeases != 1 {
		t.Fatalf("failed refresh was not atomic: users=%d/%d leases=%d/%d", oldUsers, allUsers, oldLeases, allLeases)
	}
	_, err = s.DB.Exec("DROP TRIGGER reject_refresh")
	requireStoreOK(t, err)
	requireStoreOK(t, s.RefreshWorkspaceRegistrations(users, aliases))
	for _, root := range []string{"/root/sub", "/root"} {
		if err := s.AdmitWorkspace(root, b); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("refresh lost retained lease %s: %v", root, err)
		}
	}
	prefix := a
	prefix.Session = "aaaaaaaa"
	requireStoreOK(t, s.ReleaseResearchWorkspace(prefix))
	if err := s.AdmitWorkspace("/root", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("prefix released exact owner's aliases: %v", err)
	}
	a.Session = "AAAAAAAA-1111"
	requireStoreOK(t, s.ReleaseResearchWorkspace(a))
	for _, root := range []string{"/root/sub", "/root"} {
		requireStoreOK(t, s.AdmitWorkspace(root, b))
	}
}

func TestWorkspaceRefreshRetainsConflictingLeaseOwnersUntilExactRelease(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	c := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 5, Session: "cccccccc-3333"}
	requireStoreOK(t, s.ClaimResearchWorkspace("/one/left", a))
	requireStoreOK(t, s.ClaimResearchWorkspace("/one/right", b))
	users := []WorkspaceRegistration{{Root: "/one", Owner: a}, {Root: "/one", Owner: b}}
	requireStoreOK(t, s.RefreshWorkspaceRegistrations(users, users))
	for _, owner := range []WorkspaceOwner{a, b, c} {
		if err := s.AdmitWorkspace("/one", owner); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("merged lease admitted owner %+v: %v", owner, err)
		}
		if err := s.ClaimResearchWorkspace("/one", owner); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("merged lease admitted research owner %+v: %v", owner, err)
		}
	}
	requireStoreOK(t, s.AdmitWorkspace("/unrelated", c))
	prefix := a
	prefix.Session = "aaaaaaaa"
	owned, err := s.ResearchWorkspaceOwned(prefix)
	requireStoreOK(t, err)
	if owned {
		t.Fatal("ID prefix authorized lease-owner control")
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(prefix))
	if err := s.AdmitWorkspace("/one", b); !errors.Is(err, ErrWorkspaceOccupied) {
		t.Fatalf("prefix release removed another exact owner's protection: %v", err)
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(a))
	for _, root := range []string{"/one", "/one/right"} {
		if err := s.AdmitWorkspace(root, a); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("first release discarded second owner's root %s: %v", root, err)
		}
	}
	owned, err = s.ResearchWorkspaceOwned(b)
	requireStoreOK(t, err)
	if !owned {
		t.Fatal("first release discarded second owner's control identity")
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(b))
	for _, root := range []string{"/one", "/one/left", "/one/right"} {
		requireStoreOK(t, s.AdmitWorkspace(root, c))
	}
}

func TestVersionFifteenMigrationPreservesLeaseOwnersAndHistoricalRoots(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	_, err := s.DB.Exec(`DROP TABLE workspace_leases;
CREATE TABLE workspace_leases(root TEXT PRIMARY KEY,bot INTEGER NOT NULL,chat INTEGER NOT NULL,thread INTEGER NOT NULL,session TEXT NOT NULL);
PRAGMA user_version=15;`)
	requireStoreOK(t, err)
	a := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 3, Session: "aaaaaaaa-1111"}
	b := WorkspaceOwner{Bot: 1, Chat: 2, Thread: 4, Session: "bbbbbbbb-2222"}
	requireStoreOK(t, s.ClaimResearchWorkspace("/one/left", a))
	requireStoreOK(t, s.ClaimResearchWorkspace("/one/right", b))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	aliases := []WorkspaceRegistration{{Root: "/one", Owner: a}, {Root: "/one", Owner: b}}
	requireStoreOK(t, s.RefreshWorkspaceRegistrations(aliases, aliases))
	for _, root := range []string{"/one", "/one/left"} {
		if err := s.AdmitWorkspace(root, b); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("migration lost first owner's root %s: %v", root, err)
		}
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(a))
	for _, root := range []string{"/one", "/one/right"} {
		if err := s.AdmitWorkspace(root, a); !errors.Is(err, ErrWorkspaceOccupied) {
			t.Fatalf("migration lost second owner's root %s: %v", root, err)
		}
	}
	requireStoreOK(t, s.ReleaseResearchWorkspace(b))
	requireStoreOK(t, s.AdmitWorkspace("/one", a))
}
