package i18n

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The catalog uses sequential string, integer, and float directives. Reject new
// syntax until the check understands it rather than silently skipping arguments.
var catalogDirective = regexp.MustCompile(`^%[-+# 0]*[0-9]*(?:\.[0-9]+)?[sdf]`)

func formatDirectives(template string) ([]string, error) {
	var directives []string
	for {
		index := strings.IndexByte(template, '%')
		if index < 0 {
			return directives, nil
		}
		template = template[index:]
		if strings.HasPrefix(template, "%%") {
			template = template[2:]
			continue
		}
		directive := catalogDirective.FindString(template)
		if directive == "" {
			return nil, fmt.Errorf("unsupported format directive at %q", template)
		}
		directives = append(directives, directive)
		template = template[len(directive):]
	}
}

func catalogFormatErrors(catalog any) []error {
	var failures []error
	var visit func(reflect.Value, string)
	visit = func(value reflect.Value, name string) {
		if value.Type() != reflect.TypeFor[Message]() {
			if value.Kind() != reflect.Struct {
				failures = append(failures, fmt.Errorf("%s is neither a message nor a group", name))
				return
			}
			for index := range value.NumField() {
				visit(value.Field(index), name+"."+value.Type().Field(index).Name)
			}
			return
		}
		msg := value.Interface().(Message)
		if msg.en == "" || msg.zh == "" {
			failures = append(failures, fmt.Errorf("message %s is missing a language", name))
			return
		}
		original, err := formatDirectives(msg.en)
		if err != nil {
			failures = append(failures, fmt.Errorf("English template %s: %w", name, err))
			return
		}
		localized, err := formatDirectives(msg.zh)
		if err != nil {
			failures = append(failures, fmt.Errorf("Chinese template %s: %w", name, err))
			return
		}
		if !slices.Equal(original, localized) {
			failures = append(failures, fmt.Errorf("message %s changed format arguments: English %q, Chinese %q", name, original, localized))
		}
	}
	visit(reflect.ValueOf(catalog), "Msg")
	return failures
}

func TestChineseTranslationsPreserveFormatDirectives(t *testing.T) {
	for _, err := range catalogFormatErrors(Msg) {
		t.Error(err)
	}
}

func TestCatalogChecksNestedMessages(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		msg       Message
		wantError string
	}{
		{"valid", message("Value: %s", "值: %s"), ""},
		{"missing language", Message{en: "Value"}, "missing a language"},
		{"format mismatch", message("Value: %s", "值: %d"), "changed format arguments"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var catalog struct {
				Task struct {
					SubmissionUncertain Message
				}
			}
			catalog.Task.SubmissionUncertain = scenario.msg
			failures := catalogFormatErrors(catalog)
			if scenario.wantError == "" {
				if len(failures) != 0 {
					t.Fatalf("valid nested message was rejected: %v", failures)
				}
				return
			}
			if len(failures) != 1 || !strings.Contains(failures[0].Error(), "Msg.Task.SubmissionUncertain") || !strings.Contains(failures[0].Error(), scenario.wantError) {
				t.Fatalf("nested message error = %v, want %s for Msg.Task.SubmissionUncertain", failures, scenario.wantError)
			}
		})
	}
}

func TestFormatCompatibilityDetectsConsumerVisibleMismatches(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		source     string
		translated string
		compatible bool
	}{
		{"multiline", "Workspace: %s\nQueued: %d\nFree: %.1f", "工作区: %s\n排队: %d\n可用: %.1f", true},
		{"escaped percent", "%s at 100%%", "100%%: %s", true},
		{"missing argument", "%s: %d", "%s", false},
		{"extra argument", "%s", "%s %d", false},
		{"wrong type", "%s", "%d", false},
		{"reordered types", "%s %d", "%d %s", false},
		{"changed precision", "%.1f", "%.2f", false},
		{"changed width", "%d", "%02d", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			source, err := formatDirectives(scenario.source)
			if err != nil {
				t.Fatal(err)
			}
			translated, err := formatDirectives(scenario.translated)
			if err != nil {
				t.Fatal(err)
			}
			if compatible := slices.Equal(source, translated); compatible != scenario.compatible {
				t.Fatalf("format compatibility = %t, want %t", compatible, scenario.compatible)
			}
		})
	}
	for _, template := range []string{"trailing %", "%[2]s", "%*s", "%q"} {
		if _, err := formatDirectives(template); err == nil {
			t.Errorf("unsupported syntax %q was silently accepted", template)
		}
	}
}

func TestTypedMessageFormatsOnlyTheTemplate(t *testing.T) {
	msg := message("Value: %s; count: %d; free: %.1f", "值: %s; 数量: %d; 可用: %.1f")
	value := "Cancel on /Workspace/中文"
	for _, scenario := range []struct {
		language Language
		want     string
	}{
		{English, "Value: Cancel on /Workspace/中文; count: 3; free: 1.2"},
		{Chinese, "值: Cancel on /Workspace/中文; 数量: 3; 可用: 1.2"},
	} {
		if got := Text(scenario.language, msg, value, 3, 1.25); got != scenario.want {
			t.Errorf("language %s changed an argument or selected the wrong template: %q", scenario.language, got)
		}
	}
}

func TestMessageRejectsMissingLanguage(t *testing.T) {
	for _, scenario := range []struct {
		name string
		en   string
		zh   string
	}{
		{"English", "", "中文"},
		{"Chinese", "English", ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("message with an empty language was accepted")
				}
			}()
			message(scenario.en, scenario.zh)
		})
	}
}
