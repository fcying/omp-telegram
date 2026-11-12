package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/telegram"
)

func askOptionLabel(index int, label string) string {
	return fmt.Sprintf("%d. %s", index+1, label)
}

// Bound the encoded source too: a literal containing backticks needs longer fences.
func askLiteral(text string, budget int) string {
	if budget < 16 {
		return "..."
	}
	clipped := len(text) > budget-8
	if clipped {
		text = truncateUTF8(text, budget-11) + "..."
	}
	for {
		literal := progressToolLiteral(text)
		// Edge spaces and backticks need fences; long inline delimiters can open blocks.
		if strings.TrimSpace(text) != text || strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") || strings.Contains(text, "``") {
			marker := "```"
			for strings.Contains(text, marker) {
				marker += "`"
			}
			literal = "\n" + marker + "\n" + text + "\n" + marker
		}
		if len(literal) <= budget {
			return literal
		}
		text = truncateUTF8(text, len(text)/2) + "..."
	}
}

// Join generated fragments without trimming whitespace inside literal payloads.
func askLine(rendered string) string {
	return "\n" + strings.TrimPrefix(rendered, "\n")
}

// Reserve the footer before spending space on optional explanations and previews.
func (w *worker) askQuestionText(q askQuestion, start, end int, footer string) string {
	return w.askQuestionTextBudget(q, start, end, footer, 3700, true)
}

func (w *worker) askQuestionTextBudget(q askQuestion, start, end int, footer string, limit int, completed bool) string {
	text := "**" + w.tr(i18n.Msg.AskQuestion) + "**" + askLine(askLiteral(q.Question, 700))
	if q.Header != "" {
		text += askLine(askLiteral(q.Header, 200))
	}
	start, end = max(0, start), min(end, len(q.Options))
	omitted := false
	if completed && start < end {
		full, fits := completeAskQuestionText(text, q, start, end, footer, w.tr(i18n.Msg.AskOptions), limit)
		if fits {
			return full
		}
		omitted = true
	}
	if completed && start < end {
		text += "\n**" + w.tr(i18n.Msg.AskOptions) + "**"
		budget := limit - len(text) - len(footer) - 16
		labelBudget := min(200, max(16, budget/max(1, end-start)-4))
		labels := make([]string, 0, min(end-start, max(0, budget/8)))
		for index := start; index < end; index++ {
			if budget < 18 {
				break
			}
			label := askLine(askLiteral(askOptionLabel(index, q.Options[index].Label), min(labelBudget, budget-2)))
			labels = append(labels, label)
			budget -= len(label)
		}
		for offset, label := range labels {
			text += label
			detail := askOptionDetail(q.Options[start+offset])
			if detail != "" && budget >= 24 {
				literal := askLine(askLiteral(detail, min(300, budget-1)))
				text += literal
				budget -= len(literal)
			}
		}
		if omitted {
			text += "\n..."
		}
	}
	if footer != "" {
		text += askLine(footer)
	}
	return text
}

func completeAskQuestionText(base string, q askQuestion, start, end int, footer, optionsHeading string, sourceLimit int) (string, bool) {
	var text strings.Builder
	text.Grow(len(base) + len(footer))
	text.WriteString(base)
	text.WriteString("\n**" + optionsHeading + "**")
	for index := start; index < end; index++ {
		label := askOptionLabel(index, q.Options[index].Label)
		text.WriteString(askLine(askLiteral(label, 3*len(label)+16)))
		if detail := askOptionDetail(q.Options[index]); detail != "" {
			text.WriteString(askLine(askLiteral(detail, 3*len(detail)+16)))
		}
	}
	if footer != "" {
		text.WriteString(askLine(footer))
	}
	if text.Len() > sourceLimit {
		return "", false
	}
	rendered := text.String()
	if utf16Length(telegram.ConvertMarkdown(rendered)) > telegram.MaxMessageUTF16 {
		return "", false
	}
	return rendered, true
}

func askOptionDetail(option askOption) string {
	detail := option.Description
	if option.Preview != "" {
		if detail != "" {
			detail += "\n"
		}
		detail += option.Preview
	}
	return detail
}

func (w *worker) askAnswerFooter(a *askRequest) string {
	if a.method != "ask" {
		footer := "**" + w.tr(i18n.Msg.AskSubmission) + "**"
		if a.custom == "" {
			return footer + "\n" + w.tr(i18n.Msg.AskEmptyValue)
		}
		return footer + askLine(askLiteral(a.custom, 1400))
	}
	question := a.questions[a.question]
	var values []string
	for index, option := range question.Options {
		if a.selected[index] {
			values = append(values, askOptionLabel(index, option.Label))
		}
	}
	if a.custom != "" {
		values = append(values, w.tr(i18n.Msg.AskCustom)+": "+a.custom)
	}
	value := strings.Join(values, "\n")
	if value == "" {
		value = w.tr(i18n.Msg.AskNoSelection)
	}
	return "**" + w.tr(i18n.Msg.AskChoice) + "**" + askLine(askLiteral(value, 1400))
}
