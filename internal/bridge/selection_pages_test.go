package bridge

import (
	"fmt"
	"html"
	"strings"
	"testing"
	"unicode/utf8"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/telegram"
)

func TestSelectionPagesPreserveLongUTF8Fields(t *testing.T) {
	for name, option := range map[string]askOption{
		"label": {
			Label: strings.Repeat("完整😀e\u0301", 600),
		},
		"description-and-preview": {
			Label:       "  Label\n\t ",
			Description: strings.Repeat("说明 **bold**\n\n\t😀  \n", 180),
			Preview:     strings.Repeat("```\n原始 <preview>\n```\n\n", 160) + "  tail\t ",
		},
		"exact-byte-boundary": {
			Label: strings.Repeat("x", selectionPagePayloadBudget-len("1. ")),
		},
		"one-byte-over": {
			Label: strings.Repeat("x", selectionPagePayloadBudget-len("1. ")+1),
		},
		"newline-boundary": {
			Label: "head\n" + strings.Repeat("界", 1000),
		},
	} {
		t.Run(name, func(t *testing.T) {
			pages := selectionPages([]askOption{option})
			var restored strings.Builder
			for index, page := range pages {
				if page.start != 0 || page.end != 1 || len(page.text) > selectionPagePayloadBudget || !utf8.ValidString(page.text) {
					t.Fatalf("page %d has invalid payload or mapping: %+v", index, page)
				}
				content, found := strings.CutPrefix(page.text, "1. ")
				if !found {
					t.Fatalf("page %d lost its global option number", index)
				}
				restored.WriteString(content)
			}
			want := option.Label
			if option.Description != "" {
				want += "\n" + option.Description
			}
			if option.Preview != "" {
				want += "\n" + option.Preview
			}
			if restored.String() != want {
				t.Fatal("continuation changed label, description, preview, or whitespace")
			}
			if name == "exact-byte-boundary" && len(pages) != 1 {
				t.Fatalf("exactly fitting option occupies %d pages", len(pages))
			}
			if name != "exact-byte-boundary" && len(pages) < 2 {
				t.Fatal("oversized option did not continue onto another page")
			}
			if name == "newline-boundary" && pages[0].text != "1. head\n" {
				t.Fatal("continuation ignored an available newline boundary")
			}
		})
	}
}

func TestSelectionPagesMapGlobalIndexesAcrossContentAndCountLimits(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			options := make([]askOption, 24)
			for index := range options {
				options[index].Label = fmt.Sprintf("option-%d", index+1)
			}
			if oversized {
				options[4].Label = strings.Repeat("界😀", 1000)
				options[15].Label = strings.Repeat("x", selectionPagePayloadBudget-len("16. "))
			}
			pages := selectionPages(options)
			restored := make([]string, len(options))
			previousEnd, continued, contentPage := 0, false, false
			for pageIndex, page := range pages {
				if page.start < 0 || page.end > len(options) || page.end <= page.start || page.end-page.start > uiSelectPageSize || len(page.text) > selectionPagePayloadBudget || !utf8.ValidString(page.text) {
					t.Fatalf("invalid page %d: %+v", pageIndex, page)
				}
				if page.start != previousEnd && (pageIndex == 0 || page.start != previousEnd-1) {
					t.Fatalf("page %d skips or rewinds options: previous end %d, page %+v", pageIndex, previousEnd, page)
				}
				continued = continued || pageIndex > 0 && page.start == previousEnd-1
				contentPage = contentPage || pageIndex < len(pages)-1 && page.end-page.start < uiSelectPageSize
				parts := strings.Split(page.text, "\n")
				if len(parts) != page.end-page.start {
					t.Fatalf("page %d lost a distinct option", pageIndex)
				}
				for offset, part := range parts {
					index := page.start + offset
					content, found := strings.CutPrefix(part, fmt.Sprintf("%d. ", index+1))
					if !found {
						t.Fatalf("page %d has incorrect global number for index %d", pageIndex, index)
					}
					restored[index] += content
				}
				previousEnd = page.end
			}
			if previousEnd != len(options) {
				t.Fatal("last page omitted trailing options")
			}
			for index, option := range options {
				if restored[index] != option.Label {
					t.Fatalf("index %d changed or disappeared across pages", index)
				}
			}
			if oversized && (!continued || !contentPage) {
				t.Fatal("large options did not produce content-based continuation pages")
			}
			if !oversized && (len(pages) != 3 || pages[0].end != 10 || pages[1].end != 20) {
				t.Fatal("short options did not obey the ten-option page boundary")
			}
		})
	}
	pages := selectionPages(nil)
	if len(pages) != 1 || pages[0].text != "" || pages[0].start != 0 || pages[0].end != 0 {
		t.Fatalf("empty options have no usable empty page: %+v", pages)
	}
}

func TestSelectionPageTextKeepsLiteralPayloadWithinTelegramBudget(t *testing.T) {
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		t.Run(string(language), func(t *testing.T) {
			w := &worker{b: &Bridge{}}
			w.b.setLanguage(language)
			question := askQuestion{
				Question: strings.Repeat("question **raw** <tag> 😀\n", 100),
				Header:   strings.Repeat("header `raw`\n", 100),
			}
			for name, option := range map[string]askOption{
				"multiline": {
					Label:       "  **bold** [Other](tg://user?id=8) <script>😀\n\n label  ",
					Description: strings.Repeat("```\n  <b>raw</b> & description\n````\n\n", 100),
					Preview:     "\n\t# heading\n> quote\n- list\n\n  preview  \n",
				},
				"long-backtick-run": {Label: strings.Repeat("`", 2000)},
				"inline-backtick":   {Label: "**raw** `code` [link](tg://user?id=8) <script>😀"},
				"trailing-backtick": {Label: "**raw** <b>not bold</b> `"},
				"unicode-hard-cut":  {Label: strings.Repeat("😀界e\u0301", 1000)},
			} {
				t.Run(name, func(t *testing.T) {
					pages := selectionPages([]askOption{option})
					for index, page := range pages {
						for _, context := range []struct{ history, footer string }{
							{history: strings.Repeat("chosen **raw** 😀\n", 100), footer: "Cancel"},
							{footer: askLiteral(strings.Repeat("custom 😀\n", 100), 500)},
						} {
							text := w.selectionPageText(question, page, index, len(pages), context.history, context.footer)
							rendered := telegram.ConvertMarkdown(text)
							_, optionsHTML, found := strings.Cut(rendered, "<b>"+w.tr(i18n.Msg.AskOptions)+"</b>")
							optionsHTML = strings.TrimLeft(optionsHTML, "\n")
							literal := ""
							for _, tag := range []string{"pre", "code"} {
								if body, ok := strings.CutPrefix(optionsHTML, "<"+tag+">"); ok {
									literal, _, _ = strings.Cut(body, "</"+tag+">")
								}
							}
							if !found || html.UnescapeString(literal) != page.text {
								t.Fatalf("page %d did not survive ConvertMarkdown as an exact literal", index)
							}
							if strings.Contains(rendered, "<a href=") || strings.Contains(rendered, "<script>") {
								t.Fatal("option Markdown or HTML became active")
							}
							if units := utf16Length(replyMetricsPlain(text)); units > telegram.MaxMessageUTF16 || !utf8.ValidString(rendered) {
								t.Fatalf("page %d exceeds Telegram display budget: %d units", index, units)
							}
							if parts := telegram.SplitForTelegram(text, telegram.MaxMessageUTF16); len(parts) != 1 {
								t.Fatalf("page %d unexpectedly splits into %d Telegram messages", index, len(parts))
							}
							if len(pages) > 1 && !strings.Contains(replyMetricsPlain(text), strings.TrimSpace(w.tr(i18n.Msg.Page, index+1, len(pages)))) {
								t.Fatalf("page %d lost its localized navigation indicator", index)
							}
						}
					}
				})
			}
		})
	}
}

func TestSelectionPageKeepsCompactSpacingAndOriginalBlankLines(t *testing.T) {
	w := &worker{b: &Bridge{}}
	w.b.setLanguage(i18n.English)
	options := make([]askOption, 11)
	for index := range options {
		options[index].Label = fmt.Sprintf("Option %d", index+1)
	}
	options[0].Label = "First\n\n  original second line"
	pages := selectionPages(options)
	text := w.selectionPageText(askQuestion{Question: "Choose", Header: "Ask · 1/1"}, pages[0], 0, len(pages), "Earlier choice", "**Custom answer**\ncustom")
	rendered := telegram.ConvertMarkdown(text)
	before, rest, found := strings.Cut(rendered, "<pre>")
	payload, after, closed := strings.Cut(rest, "</pre>")
	if !found || !closed {
		t.Fatal("pending options lost their literal block")
	}
	want := "1. First\n\n  original second line"
	for index := 1; index < 10; index++ {
		want += fmt.Sprintf("\n%d. Option %d", index+1, index+1)
	}
	if html.UnescapeString(payload) != want {
		t.Fatalf("option spacing changed original content or added blank lines: %q", payload)
	}
	if strings.Contains(before+"options"+after, "\n\n") {
		t.Fatalf("pending sections retained generated blank lines: %q", rendered)
	}
}
