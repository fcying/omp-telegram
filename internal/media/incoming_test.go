package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteIncomingOwnerRejectsUnreadableSize(t *testing.T) {
	dataDir := t.TempDir()
	directory := filepath.Join(dataDir, "attachments", "inbox", "incoming-large-owner")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	owner := IncomingOwner{SessionID: "session", Workspace: "/" + strings.Repeat(strings.Repeat("<", 180)+"/", 16)}
	if err := WriteIncomingOwner(dataDir, directory, owner); err == nil {
		t.Fatal("accepted owner metadata that cleanup cannot read")
	}
	if _, err := os.Stat(filepath.Join(directory, incomingOwnerFilename)); !os.IsNotExist(err) {
		t.Fatalf("rejected owner left metadata behind: %v", err)
	}
}

func TestCleanupIncomingRemovesOnlyMarkedInterruptedPreparations(t *testing.T) {
	dataDir := t.TempDir()
	inbox := filepath.Join(dataDir, "attachments", "inbox")
	if err := os.MkdirAll(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	paths := map[string]string{}
	for _, name := range []string{"orphan", "active", "recent", "legacy", "fake-marker", "invalid-owner", "valid-owner"} {
		path := filepath.Join(inbox, "incoming-"+name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		if name == "legacy" {
			if err := os.WriteFile(filepath.Join(path, "report.pdf"), []byte("keep legacy"), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			marker := incomingPreparingContents
			if name == "fake-marker" {
				marker = "other data"
			}
			if err := os.WriteFile(filepath.Join(path, incomingPreparingFilename), []byte(marker), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "orphan" {
			if err := os.WriteFile(filepath.Join(path, "report.pdf"), []byte("interrupted download"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "invalid-owner" {
			if err := os.WriteFile(filepath.Join(path, incomingOwnerFilename), []byte("partial"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if name == "valid-owner" {
			if err := WriteIncomingOwner(dataDir, path, IncomingOwner{SessionID: "session", Workspace: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(path, incomingPreparingFilename)); !os.IsNotExist(err) {
				t.Fatalf("owner write left preparation marker: %v", err)
			}
		}
		if name != "recent" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	active := map[string]struct{}{paths["active"]: {}}
	ownerChecks := 0
	removed, err := CleanupIncoming(context.Background(), dataDir, cutoff, active, func(owner IncomingOwner, remove func() error) (bool, error) {
		ownerChecks++
		return false, nil
	})
	if err != nil || removed != 2 || ownerChecks != 1 {
		t.Fatalf("cleanup removed %d paths, checked %d owners, error %v", removed, ownerChecks, err)
	}
	for _, name := range []string{"orphan", "invalid-owner"} {
		if _, err := os.Stat(paths[name]); !os.IsNotExist(err) {
			t.Fatalf("interrupted %s directory remains: %v", name, err)
		}
	}
	for _, name := range []string{"active", "recent", "legacy", "fake-marker", "valid-owner"} {
		if _, err := os.Stat(paths[name]); err != nil {
			t.Fatalf("protected %s directory removed: %v", name, err)
		}
	}
}

func TestCleanupIncomingRetainsLiveAndNonOwnedPaths(t *testing.T) {
	dataDir := t.TempDir()
	inbox := filepath.Join(dataDir, "attachments", "inbox")
	if err := os.MkdirAll(inbox, 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	paths := map[string]string{
		"expired":   filepath.Join(inbox, "incoming-expired"),
		"active":    filepath.Join(inbox, "incoming-active"),
		"recent":    filepath.Join(inbox, "incoming-recent"),
		"unrelated": filepath.Join(inbox, "keep"),
	}
	for _, path := range paths {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteIncomingOwner(dataDir, paths["expired"], IncomingOwner{SessionID: "session", Workspace: outside}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inbox, "incoming-link")); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-time.Minute)
	for _, path := range []string{paths["expired"], paths["active"], paths["unrelated"]} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	live := map[string]struct{}{paths["active"]: {}}
	removed, err := CleanupIncoming(context.Background(), dataDir, cutoff, live, func(owner IncomingOwner, remove func() error) (bool, error) {
		if owner.SessionID != "session" {
			return false, nil
		}
		return true, remove()
	})
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed %d paths, error %v", removed, err)
	}
	if _, err := os.Stat(paths["expired"]); !os.IsNotExist(err) {
		t.Fatalf("expired attachment directory remains: %v", err)
	}
	for _, name := range []string{"active", "recent", "unrelated"} {
		if _, err := os.Stat(paths[name]); err != nil {
			t.Fatalf("retained %s path is missing: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(inbox, "incoming-link")); err != nil {
		t.Fatalf("incoming symlink was removed: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("cleanup traversed the incoming symlink: %v, %v", entries, err)
	}
}

func TestRemoveIncomingAcceptsOnlyOwnedDirectChild(t *testing.T) {
	dataDir := t.TempDir()
	inbox := filepath.Join(dataDir, "attachments", "inbox")
	owned := filepath.Join(inbox, "incoming-task")
	if err := os.MkdirAll(owned, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "file.txt"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, filepath.Join(inbox, "..", "outbox", "attachment"), filepath.Join(inbox, "incoming-task", "file.txt")} {
		if err := RemoveIncoming(dataDir, path); err != nil {
			t.Fatalf("remove rejected path %q: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(owned, "file.txt")); err != nil {
		t.Fatalf("rejected path removed owned contents: %v", err)
	}
	if err := RemoveIncoming(dataDir, owned); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("owned directory remains: %v", err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("outside file changed: %q, %v", data, err)
	}
}
