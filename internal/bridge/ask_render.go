package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/i18n"
)

const askOptionButtonLimit = 100

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
		// Inline code trims edge spaces and adjacent backticks can merge delimiters.
		if strings.TrimSpace(text) != text || strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
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

// Reserve the footer before spending space on optional explanations and previews.
func (w *worker) askQuestionText(q askQuestion, start, end int, footer string) string {
	return w.askQuestionTextBudget(q, start, end, footer, 3700, true)
}

func (w *worker) askQuestionTextBudget(q askQuestion, start, end int, footer string, limit int, completed bool) string {
	text := "**" + w.tr(i18n.Msg.AskQuestion) + "**\n" + askLiteral(q.Question, 700)
	if q.Header != "" {
		text += "\n" + askLiteral(q.Header, 200)
	}
	start, end = max(0, start), min(end, len(q.Options))
	if completed && start < end {
		text += "\n\n**" + w.tr(i18n.Msg.AskOptions) + "**"
		budget := limit - len(text) - len(footer) - 16
		labelBudget := min(200, max(16, budget/max(1, end-start)-4))
		labels := make([]string, 0, min(end-start, max(0, budget/8)))
		for index := start; index < end; index++ {
			if budget < 18 {
				break
			}
			label := "\n\n" + askLiteral(askOptionLabel(index, q.Options[index].Label), min(labelBudget, budget-2))
			labels = append(labels, label)
			budget -= len(label)
		}
		for offset, label := range labels {
			text += label
			detail := askOptionDetail(q.Options[start+offset])
			if detail != "" && budget >= 24 {
				literal := askLiteral(detail, min(300, budget-1))
				text += "\n" + literal
				budget -= len(literal) + 1
			}
		}
		if len(labels) < end-start {
			text += "\n\n..."
		}
	}
	if !completed {
		budget := limit - len(text) - len(footer) - 16
		var labels []string
		omitted := false
		// Reserve overflow labels before optional details can consume their space.
		for index := start; index < end; index++ {
			label := askOptionLabel(index, q.Options[index].Label)
			buttonLabel := label
			if q.Multi {
				// Checked and unchecked markers have the same UTF-16 width.
				buttonLabel = "☐ " + buttonLabel
			}
			if len(buttonLabel) <= askOptionButtonLimit || utf16Length(menuText(buttonLabel, 0)) <= askOptionButtonLimit {
				continue
			}
			if budget < 18 {
				omitted = true
				continue
			}
			if labels == nil {
				labels = make([]string, end-start)
			}
			labels[index-start] = "\n\n" + askLiteral(label, budget-2)
			budget -= len(labels[index-start])
		}
		for index := start; index < end; index++ {
			if labels != nil {
				text += labels[index-start]
			}
			detail := askOptionDetail(q.Options[index])
			if detail == "" {
				continue
			}
			if budget < 24 {
				omitted = true
				continue
			}
			if labels == nil || labels[index-start] == "" {
				detail = askOptionLabel(index, detail)
			}
			block := "\n\n" + askLiteral(detail, min(300, budget-2))
			text += block
			budget -= len(block)
		}
		if omitted {
			text += "\n\n..."
		}
	}
	if footer != "" {
		text += "\n\n" + footer
	}
	return text
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
		footer := "**" + w.tr(i18n.Msg.AskSubmission) + "**\n"
		if a.custom == "" {
			return footer + w.tr(i18n.Msg.AskEmptyValue)
		}
		return footer + askLiteral(a.custom, 1400)
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
	return "**" + w.tr(i18n.Msg.AskChoice) + "**\n" + askLiteral(value, 1400)
}
