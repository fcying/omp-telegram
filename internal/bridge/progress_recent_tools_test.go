package bridge

import (
	"fmt"
	"strings"
	"testing"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
)

func TestConfiguredRecentToolsKeepChronologicalNewestHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "zero config keeps default", want: 5},
		{name: "single outcome", limit: 1, want: 1},
		{name: "small override", limit: 3, want: 3},
		{name: "larger override", limit: 8, want: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "verbose", ProgressRecentTools: tc.limit}}), taskState: taskState{active: 10, busy: true}})
			total := tc.want*3 + 2
			for i := range total {
				id := fmt.Sprintf("tool-%02d", i)
				w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":%q,"toolName":%q}`, id, id)))
				w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":%q,"isError":%t}`, id, i == total-1)))
			}
			if len(w.progress.RecentTools) != tc.want || w.progress.EarlierTools != total-tc.want || len(w.progress.ActiveTools) != 0 {
				t.Fatalf("configured history did not retain newest %d outcomes: %+v", tc.want, w.progress)
			}
			got := w.renderProgress()
			if !strings.Contains(got, w.tr(i18n.Msg.EarlierOmitted, total-tc.want)) {
				t.Fatalf("rendered history omitted the evicted count: %q", got)
			}
			previous := -1
			for i := range total {
				name := fmt.Sprintf("- tool-%02d:", i)
				position := strings.Index(got, name)
				if i < total-tc.want {
					if position >= 0 {
						t.Fatalf("evicted outcome %d remains visible: %q", i, got)
					}
					continue
				}
				if position <= previous {
					t.Fatalf("newest history is missing or not chronological at %d: %q", i, got)
				}
				previous = position
				tool := w.progress.RecentTools[(w.progress.EarlierTools+i-(total-tc.want))%tc.want]
				if tool.Name != fmt.Sprintf("tool-%02d", i) || tool.Failed != (i == total-1) {
					t.Fatalf("ring retained the wrong outcome at %d: %+v", i, tool)
				}
			}
		})
	}
}

func TestConfiguredRecentToolsOnlyApplyToVerboseHistory(t *testing.T) {
	for _, mode := range []string{"summary", "off"} {
		t.Run(mode, func(t *testing.T) {
			w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: mode, ProgressRecentTools: 1}}), taskState: taskState{active: 10, busy: true}})
			for i := range 4 {
				id := fmt.Sprintf("done-%d", i)
				w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":%q,"toolName":%q}`, id, id)))
				w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":%q}`, id)))
			}
			if len(w.progress.RecentTools) != 0 || w.progress.EarlierTools != 0 || len(w.progress.ActiveTools) != 0 {
				t.Fatalf("nonverbose mode retained completed history: %+v", w.progress)
			}
			if got := w.renderProgress(); strings.Contains(got, "done-") || strings.Contains(got, w.tr(i18n.Msg.RecentTools)) {
				t.Fatalf("nonverbose rendering exposed recent history: %q", got)
			}
		})
	}
}

func TestLargeRecentToolsRenderingKeepsNewestBoundedSuffix(t *testing.T) {
	w := testWorker(t, &worker{b: testBridge(t, &Bridge{cfg: config.Config{ProgressMode: "verbose", ProgressRecentTools: 100}}), taskState: taskState{active: 10, busy: true}})
	const total = 125
	for i := range total {
		id := fmt.Sprintf("recent-%03d", i)
		name := id + "-" + strings.Repeat("😀", 40)
		w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":%q,"toolName":%q,"args":{"command":%q}}`, id, name, strings.Repeat("`", 120))))
		w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_end","toolCallId":%q,"isError":%t,"result":%q}`, id, i == total-1, strings.Repeat("😀", 120))))
	}
	for i := range 8 {
		id := fmt.Sprintf("active-%d", i)
		w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_start","toolCallId":%q,"toolName":%q,"args":%q}`, id, id, strings.Repeat("`", 120))))
		w.event([]byte(fmt.Sprintf(`{"type":"tool_execution_update","toolCallId":%q,"partialResult":%q}`, id, strings.Repeat("`", 120))))
	}
	w.stream.WriteString(strings.Repeat("assistant output 😀", 500))
	got := w.renderProgress()
	if units := utf16Length(got); units > maxProgressUnits {
		t.Fatalf("large history exceeded Telegram progress budget: %d", units)
	}
	if len(w.progress.RecentTools) != 100 || w.progress.EarlierTools != total-100 {
		t.Fatalf("rendering changed the retained history: %+v", w.progress)
	}
	if strings.Count(got, "- active-") != 6 || !strings.Contains(got, w.tr(i18n.Msg.More, 2)) {
		t.Fatalf("recent limit changed the independent active tools limit: %q", got)
	}
	visible, previous := 0, -1
	for i := range total {
		position := strings.Index(got, fmt.Sprintf("- recent-%03d-", i))
		if position < 0 {
			if visible > 0 {
				t.Fatalf("rendered history is not a newest contiguous suffix at %d: %q", i, got)
			}
			continue
		}
		if position <= previous {
			t.Fatalf("large history reordered outcome %d: %q", i, got)
		}
		visible++
		previous = position
	}
	if visible == 0 || visible >= 100 || !strings.Contains(got, w.tr(i18n.Msg.EarlierOmitted, total-visible)) {
		t.Fatalf("large history did not account for display omissions: visible=%d, text=%q", visible, got)
	}
	latest := w.progress.RecentTools[(total-1)%100]
	if !latest.Failed || !strings.Contains(got, w.tr(i18n.Msg.RecentToolOutcome, latest.Name, w.tr(i18n.Msg.Failed), latest.Duration)) {
		t.Fatalf("large history hid the latest outcome: %q", got)
	}
}
