package bridge

import (
	"strings"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/telegram"
)

const selectionPagePayloadBudget = 1600

const selectionButtonColumns = 5

// The index counts choices only, so controls always start on a separate row.
func appendSelectionButton(keyboard *telegram.Keyboard, index int, button telegram.Button) {
	if index%selectionButtonColumns == 0 {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, make([]telegram.Button, 0, selectionButtonColumns))
	}
	last := len(keyboard.InlineKeyboard) - 1
	keyboard.InlineKeyboard[last] = append(keyboard.InlineKeyboard[last], button)
}

// A continued option can appear on several pages with the same selectable index.
type selectionPage struct {
	text       string
	start, end int
}

func selectionPages(options []askOption) []selectionPage {
	var pages []selectionPage
	var body strings.Builder
	start, end := 0, 0
	flush := func() {
		pages = append(pages, selectionPage{text: body.String(), start: start, end: end})
		body.Reset()
	}
	for index, option := range options {
		content := option.Label
		if detail := askOptionDetail(option); detail != "" {
			content += "\n" + detail
		}
		prefix := askOptionLabel(index, "")
		for {
			separator := ""
			if body.Len() != 0 {
				separator = "\n"
			}
			available := selectionPagePayloadBudget - body.Len() - len(separator) - len(prefix)
			if body.Len() != 0 && (end-start >= uiSelectPageSize || len(content) > available) {
				flush()
				continue
			}
			if body.Len() == 0 {
				start = index
			}
			part := content
			if len(part) > available {
				part = truncateUTF8(part, available)
				// Keep the newline in the payload so continuation never drops spacing.
				if newline := strings.LastIndexByte(part, '\n'); newline >= 0 {
					part = part[:newline+1]
				}
			}
			body.WriteString(separator)
			body.WriteString(prefix)
			body.WriteString(part)
			end = index + 1
			content = content[len(part):]
			if content == "" {
				break
			}
			flush()
		}
	}
	if body.Len() != 0 || len(pages) == 0 {
		flush()
	}
	return pages
}

func (w *worker) selectionPageText(q askQuestion, p selectionPage, page, count int, history, footer string) string {
	text := ""
	if history != "" {
		text = strings.TrimPrefix(askLiteral(history, 600), "\n") + "\n"
	}
	text += "**" + w.tr(i18n.Msg.AskQuestion) + "**" + askLine(askLiteral(q.Question, 700))
	if q.Header != "" {
		text += askLine(askLiteral(q.Header, 200))
	}
	if p.text != "" {
		// Long backtick runs can triple encoded fence size; this budget never clips.
		text += "\n**" + w.tr(i18n.Msg.AskOptions) + "**" + askLine(askLiteral(p.text, 3*len(p.text)+16))
	}
	if count > 1 {
		text += askLine(askLiteral(strings.TrimSpace(w.tr(i18n.Msg.Page, page+1, count)), 1500))
	}
	if footer != "" {
		text += askLine(footer)
	}
	return text
}
