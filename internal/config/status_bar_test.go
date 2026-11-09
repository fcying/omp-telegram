package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatusBarDefaults(t *testing.T) {
	path, source := configFixture(t)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if c.StatusBar.Progress != nil || c.StatusBar.Intermediate != nil || c.StatusBar.Final != nil {
		t.Fatal("omitted arrays must remain nil")
	}
	if c.StatusBar.GitTimeout != 450*time.Millisecond {
		t.Fatalf("default Git timeout = %v, want 450ms", c.StatusBar.GitTimeout)
	}
	want := [][]string{{"status"}, {"thinking", "model", "fast"}, {"context"}, {"speed"}}
	for _, settings := range []StatusBar{{}, c.StatusBar} {
		for _, kind := range []string{"progress", "final"} {
			if got := settings.Fields(kind); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s default fields = %v, want %v", kind, got, want)
			}
			for _, group := range want {
				for _, field := range group {
					if !settings.Has(kind, field) || !settings.Uses(field) {
						t.Fatalf("default %s field %q was not selected", kind, field)
					}
				}
			}
			if settings.Has(kind, "quota") || settings.Has(kind, "git") {
				t.Fatalf("%s enabled an opt-in field by default", kind)
			}
		}
		if len(settings.Fields("intermediate")) != 0 {
			t.Fatal("omitted intermediate fields must disable its header")
		}
		if settings.Uses("quota") || settings.Uses("git") || settings.Uses("unknown") {
			t.Fatal("defaults selected an opt-in or unknown field")
		}
		if settings.Fields("unknown") != nil || settings.Has("unknown", "status") {
			t.Fatal("unknown header kind selected fields")
		}
	}
}

func TestStatusBarExplicitEmptyDisablesOnlySelectedHeader(t *testing.T) {
	for _, kind := range []string{"progress", "intermediate", "final"} {
		t.Run(kind, func(t *testing.T) {
			path, source := configFixture(t)
			source = setTOMLField(source, "telegram.status_bar", kind, `[]`)
			c, err := loadSource(t, path, source)
			if err != nil {
				t.Fatal(err)
			}
			if fields := c.StatusBar.Fields(kind); fields == nil || len(fields) != 0 {
				t.Fatalf("explicit empty %s = %v; want nonnil empty fields", kind, fields)
			}
			if c.StatusBar.Has(kind, "status") {
				t.Fatal("disabled header retained a default field")
			}
			for _, other := range []string{"progress", "final"} {
				if other != kind && !c.StatusBar.Has(other, "status") {
					t.Fatalf("disabling %s also disabled omitted %s", kind, other)
				}
			}
			if !c.StatusBar.Uses("status") {
				t.Fatal("other default headers must still select status")
			}
		})
	}
}

func TestStatusBarAllHeadersExplicitlyDisabled(t *testing.T) {
	path, source := configFixture(t)
	for _, kind := range []string{"progress", "intermediate", "final"} {
		source = setTOMLField(source, "telegram.status_bar", kind, `[]`)
	}
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"status", "thinking", "model", "fast", "context", "speed", "quota", "git"} {
		if c.StatusBar.Uses(field) {
			t.Fatalf("disabled headers still select %q", field)
		}
	}
}

func TestStatusBarPreservesOrderAndIndependentSelections(t *testing.T) {
	path, source := configFixture(t)
	source = setTOMLField(source, "telegram.status_bar", "progress", `["quota", "model"]`)
	source = setTOMLField(source, "telegram.status_bar", "intermediate", `["thinking", "fast"]`)
	source = setTOMLField(source, "telegram.status_bar", "final", `["git", "quota", "speed", "context", "fast", "model", "thinking", "status"]`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind string
		want [][]string
	}{
		{"progress", [][]string{{"quota"}, {"model"}}},
		{"intermediate", [][]string{{"thinking"}, {"fast"}}},
		{"final", [][]string{{"git"}, {"quota"}, {"speed"}, {"context"}, {"fast"}, {"model"}, {"thinking"}, {"status"}}},
	} {
		if got := c.StatusBar.Fields(tc.kind); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s fields = %v, want %v", tc.kind, got, tc.want)
		}
	}
	if c.StatusBar.Has("progress", "thinking") || c.StatusBar.Has("intermediate", "model") || c.StatusBar.Has("intermediate", "quota") {
		t.Fatal("separate header and model/thinking selections were combined")
	}
	if !c.StatusBar.Has("progress", "quota") || !c.StatusBar.Has("final", "git") || !c.StatusBar.Uses("quota") || !c.StatusBar.Uses("git") {
		t.Fatal("explicit opt-in fields were not selected")
	}
}

func TestStatusBarGroupedFields(t *testing.T) {
	path, source := configFixture(t)
	source = setTOMLField(source, "telegram.status_bar", "progress", `[["status", "model"], ["context", "speed"]]`)
	source = setTOMLField(source, "telegram.status_bar", "intermediate", `[[], ["quota"], []]`)
	source = setTOMLField(source, "telegram.status_bar", "final", `["status", ["thinking", "model", "fast"], "git"]`)
	c, err := loadSource(t, path, source)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.StatusBar.Progress, [][]string{{"status", "model"}, {"context", "speed"}}) ||
		!reflect.DeepEqual(c.StatusBar.Intermediate, [][]string{{"quota"}}) ||
		!reflect.DeepEqual(c.StatusBar.Final, [][]string{{"status"}, {"thinking", "model", "fast"}, {"git"}}) {
		t.Fatalf("field groups lost order or boundaries: %+v", c.StatusBar)
	}
	if !c.StatusBar.Has("progress", "speed") || !c.StatusBar.Uses("quota") || !c.StatusBar.Has("final", "fast") {
		t.Fatal("grouped fields did not enable their sampling requirements")
	}
}

func TestStatusBarRejectsInvalidFields(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_STATUS_FIELD", "model")
	for _, kind := range []string{"progress", "intermediate", "final"} {
		for _, value := range []string{
			`["unknown-private-field"]`,
			`[""]`,
			`["Model"]`,
			`[" model"]`,
			`["${BRIDGE_TEST_STATUS_FIELD}"]`,
			`["status", "model", "status"]`,
			`["quota", "quota"]`,
			`[["status", "model"], ["status"]]`,
			`["quota", ["quota"]]`,
			`[["unknown-private-field"]]`,
		} {
			t.Run(kind+"/"+value, func(t *testing.T) {
				_, err := loadSource(t, path, setTOMLField(source, "telegram.status_bar", kind, value))
				if err == nil || !strings.Contains(err.Error(), "telegram.status_bar."+kind) {
					t.Fatalf("invalid %s fields %s: error = %v", kind, value, err)
				}
				if strings.Contains(err.Error(), "unknown-private-field") || strings.Contains(err.Error(), "BRIDGE_TEST_STATUS_FIELD") {
					t.Fatalf("validation exposed an untrusted field: %v", err)
				}
			})
		}
	}
	for _, kind := range []string{"progress", "intermediate"} {
		if _, err := loadSource(t, path, setTOMLField(source, "telegram.status_bar", kind, `[["status", "git"]]`)); err == nil {
			t.Fatalf("git was accepted in %s", kind)
		}
	}
	if _, err := loadSource(t, path, setTOMLField(source, "telegram.status_bar", "final", `["git", "git"]`)); err == nil {
		t.Fatal("duplicate git was accepted in final")
	}
}

func TestStatusBarRejectsInvalidSchema(t *testing.T) {
	path, source := configFixture(t)
	for _, kind := range []string{"progress", "intermediate", "final"} {
		for _, value := range []string{`true`, `"model"`, `42`, `["model", 7]`, `[false]`, `{field = "model"}`, `[["model", 7]]`, `[[["model"]]]`, `[{field = "model"}]`} {
			t.Run(kind+"/"+value, func(t *testing.T) {
				if _, err := loadSource(t, path, setTOMLField(source, "telegram.status_bar", kind, value)); err == nil {
					t.Fatalf("invalid %s array type %s was accepted", kind, value)
				}
			})
		}
	}
	if _, err := loadSource(t, path, setTOMLField(source, "telegram", "status_bar", `false`)); err == nil {
		t.Fatal("invalid status-bar table was accepted")
	}
}

func TestLegacyProviderUsageIsIgnored(t *testing.T) {
	path, source := configFixture(t)
	for _, value := range []string{`true`, `false`, `"${IGNORED_PROVIDER_USAGE_UNSET}"`} {
		t.Run(value, func(t *testing.T) {
			input := setTOMLField(source, "telegram", "provider_usage", value)
			c, err := loadSource(t, path, input)
			if err != nil {
				t.Fatal(err)
			}
			if c.StatusBar.Uses("quota") {
				t.Fatal("legacy provider_usage enabled quota")
			}
			input = setTOMLField(input, "telegram.status_bar", "final", `["quota", "git"]`)
			c, err = loadSource(t, path, input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.StatusBar.Final, [][]string{{"quota"}, {"git"}}) || c.StatusBar.Has("progress", "quota") || c.StatusBar.Has("intermediate", "quota") {
				t.Fatal("legacy provider_usage changed explicit status-bar selections")
			}
		})
	}
}

func TestStatusBarGitCommandRejectsInvalidArgv(t *testing.T) {
	path, source := configFixture(t)
	for _, value := range []string{`[""]`, `["  "]`, `["git", "private\u0000argument"]`, `"git log -1"`, `["git", 7]`} {
		t.Run(value, func(t *testing.T) {
			input := setTOMLField(source, "telegram.status_bar", "git_command", value)
			_, err := loadSource(t, path, input)
			if err == nil {
				t.Fatal("invalid git_command was accepted")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("validation exposed command arguments: %v", err)
			}
		})
	}
}

func TestStatusBarGitTimeoutLiteralDurations(t *testing.T) {
	path, source := configFixture(t)
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"omitted", "", 450 * time.Millisecond},
		{"default", `"450ms"`, 450 * time.Millisecond},
		{"large_repository", `"2s"`, 2 * time.Second},
		{"minimum", `"1ns"`, time.Nanosecond},
		{"fractional", `"1.5ms"`, 1500 * time.Microsecond},
		{"compound", `"1m2s"`, 62 * time.Second},
		{"maximum", `"9223372036854775807ns"`, time.Duration(1<<63 - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := source
			if tc.value != "" {
				input = setTOMLField(input, "telegram.status_bar", "git_timeout", tc.value)
			}
			c, err := loadSource(t, path, input)
			if err != nil {
				t.Fatal(err)
			}
			if c.StatusBar.GitTimeout != tc.want {
				t.Fatalf("Git timeout = %v, want %v", c.StatusBar.GitTimeout, tc.want)
			}
		})
	}
}

func TestStatusBarGitTimeoutRejectsInvalidValuesWithoutDisclosure(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_TEST_PRIVATE_GIT_TIMEOUT", "2s")
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"empty", `""`},
		{"zero", `"0"`},
		{"zero_with_unit", `"0s"`},
		{"negative", `"-1ns"`},
		{"subnanosecond", `"0.1ns"`},
		{"missing_unit", `"450"`},
		{"whitespace", `" 2s "`},
		{"invalid", `"private-invalid-duration"`},
		{"overflow", `"9223372036854775808ns"`},
		{"environment", `"${BRIDGE_TEST_PRIVATE_GIT_TIMEOUT}"`},
		{"bare_environment", `"$BRIDGE_TEST_PRIVATE_GIT_TIMEOUT"`},
		{"integer", `450`},
		{"float", `0.45`},
		{"boolean", `false`},
		{"array", `["private-invalid-duration"]`},
		{"table", `{private = "private-invalid-duration"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := setTOMLField(source, "telegram.status_bar", "git_timeout", tc.value)
			_, err := loadSource(t, path, input)
			if err == nil {
				t.Fatal("invalid Git timeout was accepted even with Git unselected")
			}
			if strings.Contains(err.Error(), "private-invalid-duration") || strings.Contains(err.Error(), "BRIDGE_TEST_PRIVATE_GIT_TIMEOUT") {
				t.Fatalf("Git timeout error disclosed input: %v", err)
			}
		})
	}
}
