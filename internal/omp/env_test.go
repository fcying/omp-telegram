package omp

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestEnvironmentValidation(t *testing.T) {
	for _, mode := range []string{"", "all", "inherit", "restricted", "blacklist", "whitelist"} {
		if _, err := NewEnvironment(mode, nil, nil); err == nil {
			t.Fatalf("accepted unknown environment mode %q", mode)
		}
	}
	if _, err := NewEnvironment("denylist", []string{}, nil); err == nil {
		t.Fatal("accepted allow list with denylist environment")
	}
	if _, err := NewEnvironment("allowlist", []string{}, []string{}); err == nil {
		t.Fatal("accepted deny list with allowlist environment")
	}
	for _, name := range []string{"", "1NAME", "A-B", "A=B", "A B", "A\x00B", "å"} {
		if _, err := NewEnvironment("denylist", nil, []string{name}); err == nil || strings.Contains(err.Error(), name) && name != "" {
			t.Fatalf("accepted invalid deny name or exposed it in error: %q, %v", name, err)
		}
	}
	if _, err := NewEnvironment("denylist", nil, []string{"_VALID_9", botTokenName}); err != nil {
		t.Fatalf("rejected valid deny name: %v", err)
	}
	for _, name := range []string{"", "1NAME", "A-B", "A=B", "A B", "A\x00B", "å"} {
		if _, err := NewEnvironment("allowlist", []string{name}, nil); err == nil || strings.Contains(err.Error(), name) && name != "" {
			t.Fatalf("accepted invalid allow name or exposed it in error: %q, %v", name, err)
		}
	}
	if _, err := NewEnvironment("allowlist", []string{botTokenName}, nil); err == nil || strings.Contains(err.Error(), botTokenName) {
		t.Fatalf("accepted bot token in allowlist or exposed its name in error: %v", err)
	}
	if _, err := NewEnvironment("allowlist", []string{"_VALID_9"}, nil); err != nil {
		t.Fatalf("rejected POSIX environment variable: %v", err)
	}
	for _, deny := range [][]string{nil, {}} {
		if _, err := NewEnvironment("denylist", nil, deny); err != nil {
			t.Fatalf("rejected empty denylist: %v", err)
		}
	}
	for _, allow := range [][]string{nil, {}} {
		if _, err := NewEnvironment("allowlist", allow, nil); err != nil {
			t.Fatalf("rejected empty allowlist: %v", err)
		}
	}
}

func TestChildEnvironmentPolicy(t *testing.T) {
	t.Setenv(botTokenName, "secret")
	t.Setenv("OMP_TEST_ALLOWED", "permitted")
	t.Setenv("OMP_TEST_EXCLUDED", "not permitted")
	t.Setenv("OMP_TEST_ABSENT", "temporary")
	if err := os.Unsetenv("OMP_TEST_ABSENT"); err != nil {
		t.Fatal(err)
	}

	denylist, err := NewEnvironment("denylist", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []Environment{{}, denylist} {
		entries := ChildEnv(policy)
		if !strings.Contains(strings.Join(entries, "\n"), "OMP_TEST_EXCLUDED=not permitted") {
			t.Fatal("default environment dropped an unrelated parent variable")
		}
		if value, ok := policy.Lookup("OMP_TEST_EXCLUDED"); !ok || value != "not permitted" {
			t.Fatal("default environment did not expose a permitted parent variable")
		}
		if value, ok := policy.Lookup(botTokenName); ok || value != "" {
			t.Fatal("default lookup exposed the bot token")
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry, botTokenName+"=") {
				t.Fatal("default child environment exposed the bot token")
			}
		}
	}

	allow := []string{"OMP_TEST_ALLOWED", "OMP_TEST_ABSENT", "OMP_TEST_ALLOWED"}
	allowlist, err := NewEnvironment("allowlist", allow, nil)
	if err != nil {
		t.Fatal(err)
	}
	allow[0] = "OMP_TEST_EXCLUDED"
	if got := ChildEnv(allowlist); !reflect.DeepEqual(got, []string{"OMP_TEST_ALLOWED=permitted"}) {
		t.Fatalf("allowlist child environment = %q", got)
	}
	if value, ok := allowlist.Lookup("OMP_TEST_ALLOWED"); !ok || value != "permitted" {
		t.Fatal("allowlist lookup lost permitted parent value")
	}
	for _, name := range []string{"OMP_TEST_EXCLUDED", "OMP_TEST_ABSENT", botTokenName} {
		if value, ok := allowlist.Lookup(name); ok || value != "" {
			t.Fatalf("allowlist lookup exposed %s", name)
		}
	}
	empty, err := NewEnvironment("allowlist", []string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries := ChildEnv(empty); entries == nil || len(entries) != 0 {
		t.Fatalf("empty allowlist environment = %q, want non-nil empty slice", entries)
	}
}

func TestDenylistEnvironmentRejectsOnlySelectedVariables(t *testing.T) {
	t.Setenv(botTokenName, "secret")
	t.Setenv("OMP_TEST_EXCLUDED", "parent-secret")
	t.Setenv("OMP_TEST_ALLOWED", "parent-safe")
	deny := []string{"OMP_TEST_EXCLUDED"}
	policy, err := NewEnvironment("denylist", nil, deny)
	if err != nil {
		t.Fatal(err)
	}
	deny[0] = "OMP_TEST_ALLOWED"
	if value, ok := policy.Lookup("OMP_TEST_EXCLUDED"); ok || value != "" {
		t.Fatal("excluded parent value exposed by lookup")
	}
	if value, ok := policy.Lookup("OMP_TEST_ALLOWED"); !ok || value != "parent-safe" {
		t.Fatal("unrelated parent value lost from lookup")
	}
	entries := ChildEnv(policy)
	for _, entry := range entries {
		if strings.HasPrefix(entry, "OMP_TEST_EXCLUDED=") || strings.HasPrefix(entry, botTokenName+"=") {
			t.Fatal("excluded parent value exposed to child")
		}
	}
	if !strings.Contains(strings.Join(entries, "\n"), "OMP_TEST_ALLOWED=parent-safe") {
		t.Fatal("unrelated parent value lost from child")
	}
	if got := os.Getenv("OMP_TEST_EXCLUDED"); got != "parent-secret" {
		t.Fatal("policy changed the bridge parent environment")
	}
}
