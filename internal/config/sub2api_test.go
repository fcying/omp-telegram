package config

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"omp-telegram/internal/omp"
)

func sub2apiSource(source string) string {
	source = setTOMLField(source, "sub2api", "extension", "true")
	source = setTOMLField(source, "sub2api", "base_url", `"https://quota.example/prefix"`)
	source = setTOMLField(source, "sub2api", "api_key", `"private-admin-key"`)
	return setTOMLField(source, "sub2api", "providers", `["gateway", "Gateway-two"]`)
}

func TestSub2apiOptInAndExactProviders(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil || c.Sub2API.Enabled() || c.Sub2API.HasProvider("gateway") {
		t.Fatalf("omitted upstream configuration must be disabled: %v", err)
	}
	configured := sub2apiSource(source)
	c, err = loadSource(t, path, configured)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Sub2API.Enabled() || !c.Sub2API.HasProvider("gateway") || !c.Sub2API.HasProvider("Gateway-two") || c.Sub2API.HasProvider("Gateway") || c.Sub2API.HasProvider(" gateway") {
		t.Fatal("provider matching must be exact and case-sensitive")
	}
	for _, option := range []string{"false", `"private-admin-key"`} {
		_, err = loadSource(t, path, setTOMLField(configured, "sub2api", "extension", option))
		if err == nil || strings.Contains(err.Error(), "private-admin-key") {
			t.Fatalf("upstream requires explicit boolean observer opt-in: %v", err)
		}
	}
	for _, field := range []string{"base_url", "api_key", "providers"} {
		value := `""`
		if field == "providers" {
			value = `[]`
		}
		if _, err = loadSource(t, path, setTOMLField(configured, "sub2api", field, value)); err == nil {
			t.Fatalf("partial upstream configuration accepted without %s", field)
		}
	}
}

func TestSub2apiRejectsHeadlessNotificationSuppression(t *testing.T) {
	path, source := configFixture(t)
	headless := setTOMLField(source, "omp", "args", `["--no-ui"]`)
	if _, err := loadSource(t, path, headless); err != nil {
		t.Fatalf("disabled upstream changed existing headless arguments: %v", err)
	}
	for _, args := range []string{`["--no-ui"]`, `["--model first", "--model second --no-ui"]`} {
		configured := setTOMLField(sub2apiSource(source), "omp", "args", args)
		if _, err := loadSource(t, path, configured); err == nil {
			t.Fatal("enabled observer accepted arguments that discard its notifications")
		}
	}
}

func TestSub2apiRejectsUnsafeInputsPrivately(t *testing.T) {
	path, source := configFixture(t)
	configured := sub2apiSource(source)
	for _, tc := range []struct {
		field string
		value string
	}{
		{"base_url", `"https://private-admin-key@example.com"`},
		{"base_url", `"https://example.com/?private-admin-key"`},
		{"base_url", `"https://example.com/#private-admin-key"`},
		{"base_url", `"file:///private-admin-key"`},
		{"base_url", `"https:///private-admin-key"`},
		{"base_url", `"https://example.com:65536/private-admin-key"`},
		{"base_url", `"https://example.com:/private-admin-key"`},
		{"base_url", `"https://example.com/%2e%2e/private-admin-key"`},
		{"base_url", `"https://example.com/%0aprivate-admin-key"`},
		{"api_key", `"private-admin-key\n"`},
		{"api_key", `" private-admin-key"`},
		{"api_key", `"${UNSET_PRIVATE_ADMIN_KEY}"`},
		{"providers", `["private-admin-key", "private-admin-key"]`},
		{"providers", `["private-admin-key/invalid"]`},
		{"providers", `["private-admin-key\n"]`},
		{"providers", `[""]`},
		{"providers", `[7]`},
	} {
		t.Run(tc.field+tc.value, func(t *testing.T) {
			_, err := loadSource(t, path, setTOMLField(configured, "sub2api", tc.field, tc.value))
			if err == nil || strings.Contains(strings.ToLower(err.Error()), "private-admin-key") || strings.Contains(err.Error(), "UNSET_PRIVATE_ADMIN_KEY") {
				t.Fatalf("unsafe input must fail without source data: %v", err)
			}
		})
	}
}

func TestSub2apiExpansionAndSecretIsolation(t *testing.T) {
	path, source := configFixture(t)
	configured := sub2apiSource(source)
	t.Setenv("BRIDGE_SUB2API_URL", "http://localhost:8080/prefix/")
	t.Setenv("BRIDGE_SUB2API_KEY", "private-$UNEXPANDED-admin-key")
	t.Setenv("BRIDGE_SUB2API_ALIAS", "private-$UNEXPANDED-admin-key")
	t.Setenv("BRIDGE_SUB2API_PROVIDER", "gateway")
	t.Setenv("BRIDGE_MODEL_KEY", "untouched-provider-key")
	configured = setTOMLField(configured, "sub2api", "base_url", `"${BRIDGE_SUB2API_URL}"`)
	configured = setTOMLField(configured, "sub2api", "api_key", `"$BRIDGE_SUB2API_KEY"`)
	configured = setTOMLField(configured, "sub2api", "providers", `["${BRIDGE_SUB2API_PROVIDER}"]`)
	for _, mode := range []string{"denylist", "allowlist"} {
		t.Run(mode, func(t *testing.T) {
			input := setTOMLField(configured, "omp.environment", "mode", strconv.Quote(mode))
			if mode == "allowlist" {
				input = setTOMLField(input, "omp.environment", "allow", `["BRIDGE_MODEL_KEY"]`)
			} else {
				input = setTOMLField(input, "omp.environment", "deny", `["BRIDGE_SUB2API_KEY", "BRIDGE_SUB2API_ALIAS"]`)
			}
			c, err := loadSource(t, path, input)
			if err != nil {
				t.Fatal(err)
			}
			if c.Sub2API.BaseURL != "http://localhost:8080/prefix" || c.Sub2API.APIKey != os.Getenv("BRIDGE_SUB2API_KEY") || !c.Sub2API.HasProvider("gateway") {
				t.Fatal("upstream references must expand once as parsed values")
			}
			for _, name := range []string{"BRIDGE_SUB2API_KEY", "BRIDGE_SUB2API_ALIAS"} {
				if _, ok := c.OMPEnvironment.Lookup(name); ok {
					t.Fatal("admin key reached native environment lookup")
				}
			}
			for _, entry := range omp.ChildEnv(c.OMPEnvironment) {
				if strings.Contains(entry, c.Sub2API.APIKey) {
					t.Fatal("admin key reached native child environment")
				}
			}
			if value, ok := c.OMPEnvironment.Lookup("BRIDGE_MODEL_KEY"); !ok || value != "untouched-provider-key" {
				t.Fatal("model credential policy changed")
			}
			for _, args := range c.OMPArgs {
				for _, arg := range args {
					if strings.Contains(arg, c.Sub2API.APIKey) {
						t.Fatal("upstream configuration added an admin credential to OMP argv")
					}
				}
			}
		})
	}
	// Opt-in must not override a user's existing explicit environment policy.
	input := setTOMLField(configured, "omp.environment", "mode", `"allowlist"`)
	input = setTOMLField(input, "omp.environment", "allow", `["BRIDGE_SUB2API_KEY"]`)
	c, err := loadSource(t, path, input)
	if err != nil {
		t.Fatal(err)
	}
	if value, ok := c.OMPEnvironment.Lookup("BRIDGE_SUB2API_KEY"); !ok || value != os.Getenv("BRIDGE_SUB2API_KEY") {
		t.Fatal("upstream opt-in rewrote the user's explicit child policy")
	}
}

func TestSub2apiObserverPathConstraints(t *testing.T) {
	for _, kind := range []string{"disabled", "directory", "file"} {
		t.Run(kind, func(t *testing.T) {
			path, source := configFixture(t)
			source = setTOMLField(source, "omp", "args", `["--model first", "--model second"]`)
			base, err := loadSource(t, path, source)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "disabled" {
				if err := os.WriteFile(filepath.Join(base.DataDir, ".bridge-extensions"), []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
				c, err := loadSource(t, path, source)
				if err != nil || !slices.Equal(c.OMPArgs[0], []string{"--model", "first"}) {
					t.Fatalf("disabled observer touched extension cache or argv: %v", err)
				}
				if _, err = loadSource(t, path, setTOMLField(source, "sub2api", "extension", "true")); err == nil || strings.Contains(err.Error(), base.DataDir) {
					t.Fatalf("observer cache failure must be private: %v", err)
				}
				return
			}
			source = setTOMLField(source, "sub2api", "extension", "true")
			c, err := loadSource(t, path, source)
			if err != nil {
				t.Fatal(err)
			}
			asset := c.OMPArgs[0][len(c.OMPArgs[0])-1]
			if !filepath.IsAbs(asset) || filepath.Dir(asset) != filepath.Join(c.DataDir, ".bridge-extensions") || c.OMPArgs[0][len(c.OMPArgs[0])-2] != "-e" || c.OMPArgs[1][len(c.OMPArgs[1])-1] != asset {
				t.Fatal("observer must use a private absolute bridge cache path in every preset")
			}
			outside := t.TempDir()
			protected := filepath.Join(outside, filepath.Base(asset))
			if err := os.WriteFile(protected, []byte("user-owned"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(asset); err != nil {
				t.Fatal(err)
			}
			link, target := asset, protected
			if kind == "directory" {
				link, target = filepath.Dir(asset), outside
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, err = loadSource(t, path, source); err == nil || strings.Contains(err.Error(), protected) {
				t.Fatalf("observer cache symlink must fail privately: %v", err)
			}
			contents, err := os.ReadFile(protected)
			if err != nil || string(contents) != "user-owned" {
				t.Fatal("observer preparation modified user-owned extension")
			}
		})
	}
}
