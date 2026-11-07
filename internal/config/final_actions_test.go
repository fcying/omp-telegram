package config

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFinalActionsDisabledByDefault(t *testing.T) {
	path, source := configFixture(t)
	for _, configured := range []string{source, setTOMLField(source, "telegram", "final_actions", "[]")} {
		cfg, err := loadSource(t, path, configured)
		if err != nil || len(cfg.FinalActions) != 0 {
			t.Fatalf("completion shortcuts must default to disabled: count=%d err=%v", len(cfg.FinalActions), err)
		}
	}
}

func TestFinalActionsLiteralOrderAndBoundaries(t *testing.T) {
	path, source := configFixture(t)
	t.Setenv("BRIDGE_ACTION_LITERAL", "expanded-secret")
	label := strings.Repeat("😀", 32)
	actions := []FinalAction{
		{Label: label, Command: "/compact"},
		{Label: "$BRIDGE_ACTION_LITERAL", Command: "/name A; B && C || ${BRIDGE_ACTION_LITERAL}"},
		{Label: "Path", Command: "/new /tmp/absolute-project"},
		{Label: "Colon arguments", Command: "/handoff:focus on A; B && C!"},
		{Label: "Future catalog", Command: "/not_installed_yet"},
		{Label: "Queue", Command: "/queue"},
	}
	var entries []string
	for _, action := range actions {
		entries = append(entries, "{label = "+strconv.Quote(action.Label)+", command = "+strconv.Quote(action.Command)+"}")
	}
	cfg, err := loadSource(t, path, setTOMLField(source, "telegram", "final_actions", "["+strings.Join(entries, ",")+"]"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.FinalActions, actions) {
		t.Fatal("literal shortcuts must preserve order, labels and command arguments without expanding environment references")
	}
}

func TestFinalActionsRejectInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"too many", "[" + strings.Repeat(`{label="secret-label",command="/queue"},`, 7) + "]"},
		{"empty label", `[{label="",command="/queue"}]`},
		{"blank label", `[{label="   ",command="/queue"}]`},
		{"label over UTF16 boundary", `[{label=` + strconv.Quote(strings.Repeat("😀", 32)+"a") + `,command="/queue"}]`},
		{"label newline", `[{label="secret-label\nnext",command="/queue"}]`},
		{"label unicode newline", `[{label="secret-label\u2028next",command="/queue"}]`},
		{"label control", `[{label="secret-label\u0000",command="/queue"}]`},
		{"empty command", `[{label="secret-label",command=""}]`},
		{"not slash", `[{label="secret-label",command="secret-command"}]`},
		{"missing name", `[{label="secret-label",command="/"}]`},
		{"empty token", `[{label="secret-label",command="/ secret-command"}]`},
		{"leading whitespace", `[{label="secret-label",command=" /secret-command"}]`},
		{"joined token", `[{label="secret-label",command="/compact;/secret-command"}]`},
		{"command newline", `[{label="secret-label",command="/queue\n/secret-command"}]`},
		{"command CR", `[{label="secret-label",command="/queue\r/secret-command"}]`},
		{"command tab", `[{label="secret-label",command="/name\tsecret-command"}]`},
		{"command control", `[{label="secret-label",command="/name secret-command\u007F"}]`},
		{"command NUL", `[{label="secret-label",command="/name secret-command\u0000"}]`},
		{"command unicode newline", `[{label="secret-label",command="/name secret-command\u2029next"}]`},
		{"wrong command type", `[{label="secret-label",command=17}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, source := configFixture(t)
			_, err := loadSource(t, path, setTOMLField(source, "telegram", "final_actions", tc.value))
			if err == nil {
				t.Fatal("invalid shortcut configuration accepted")
			}
			if strings.Contains(err.Error(), "secret-label") || strings.Contains(err.Error(), "secret-command") {
				t.Fatal("configuration errors must not expose shortcut contents")
			}
		})
	}
}
