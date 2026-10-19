package store

import (
	"testing"

	"omp-telegram/internal/i18n"
)

func TestGlobalLanguagePersistsAcrossStoreLifetimes(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	language, err := s.Language()
	requireStoreOK(t, err)
	if language != i18n.English {
		t.Fatalf("unconfigured language = %q", language)
	}
	requireStoreOK(t, s.SetLanguage(i18n.Chinese))
	if err := s.SetLanguage("invalid"); err == nil {
		t.Fatal("unsupported language was accepted")
	}
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	language, err = s.Language()
	requireStoreOK(t, err)
	if language != i18n.Chinese {
		t.Fatalf("reopen or failed update lost language: %q", language)
	}
	requireStoreOK(t, s.SetLanguage(i18n.English))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	language, err = s.Language()
	requireStoreOK(t, err)
	if language != i18n.English {
		t.Fatalf("English selection was not persisted: %q", language)
	}
}

func TestLanguageSchema19UpgradePreservesBindingAndPendingOutput(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir)
	before := Binding{Bot: 1, Chat: 2, Thread: 3, Workspace: "/workspace", Session: "/session", Generation: 7}
	requireStoreOK(t, s.Save(before))
	requireStoreOK(t, s.Enqueue(2, 3, "previously rendered output"))
	_, err := s.DB.Exec("DROP TABLE interface_language; PRAGMA user_version=19")
	requireStoreOK(t, err)
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	after, err := s.Binding(1, 2, 3)
	requireStoreOK(t, err)
	if after != before {
		t.Fatalf("language migration changed binding: %+v", after)
	}
	var text string
	requireStoreOK(t, s.DB.QueryRow("SELECT text FROM outbox WHERE chat=2 AND thread=3 AND state='pending'").Scan(&text))
	if text != "previously rendered output" {
		t.Fatalf("language migration rewrote pending output: %q", text)
	}
	language, err := s.Language()
	requireStoreOK(t, err)
	if language != i18n.English {
		t.Fatalf("migrated language = %q", language)
	}
	requireStoreOK(t, s.SetLanguage(i18n.Chinese))
	requireStoreOK(t, s.Close())
	s = openTestStore(t, dir)
	language, err = s.Language()
	requireStoreOK(t, err)
	if language != i18n.Chinese {
		t.Fatalf("migrated preference was not retained: %q", language)
	}
}
