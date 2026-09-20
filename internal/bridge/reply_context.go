package bridge

import (
	"fmt"
	"strings"

	"omp-telegram/internal/telegram"
)

const (
	maxReplyContextUnits = 3000
	replyContextMarker   = "...[truncated]"
	replyContextStart    = "[Telegram reply context - quoted content]"
	replyContextEnd      = "[/Telegram reply context]"
	currentMessageTag    = "[Current user message]"

	replyContextKindQuote       = "quote"
	replyContextKindText        = "text"
	replyContextKindPhoto       = "photo"
	replyContextKindDocument    = "document"
	replyContextKindUnsupported = "unsupported"
)

type ReplyContext struct {
	Kind      string
	Sender    string
	Text      string
	MediaName string
	Truncated bool
}

func extractReplyContext(message *telegram.Message) ReplyContext {
	if message == nil {
		return ReplyContext{}
	}
	reply := message.ReplyToMessage
	quote := ""
	if message.Quote != nil {
		quote = nonEmptyReplyField(message.Quote.Text)
	}
	if reply == nil && quote == "" {
		return ReplyContext{}
	}

	ctx := ReplyContext{Sender: "user"}
	if reply != nil && reply.From != nil && reply.From.IsBot {
		ctx.Sender = "bot"
	}
	replyText, caption := "", ""
	if reply != nil {
		replyText = nonEmptyReplyField(reply.Text)
		caption = nonEmptyReplyField(reply.Caption)
	}
	switch {
	case quote != "":
		ctx.Kind = replyContextKindQuote
		ctx.Text = quote
	case reply == nil:
		return ReplyContext{}
	case replyText != "":
		ctx.Kind = replyContextKindText
		ctx.Text = replyText
	case len(reply.Photo) != 0:
		ctx.Kind = replyContextKindPhoto
		ctx.Text = caption
	case reply.Document != nil:
		ctx.Kind = replyContextKindDocument
		ctx.MediaName = reply.Document.FileName
		ctx.Text = caption
	case caption != "":
		ctx.Kind = replyContextKindText
		ctx.Text = caption
	default:
		ctx.Kind = replyContextKindUnsupported
	}
	ctx.Truncated = replyContextNeedsTruncation(ctx)
	return ctx
}

func formatReplyContext(ctx ReplyContext) string {
	body := replyContextBody(ctx)
	if body == "" {
		return ""
	}
	prefix, suffix := replyContextEnvelope(ctx.Sender)
	return prefix + truncateReplyContext(body, maxReplyContextUnits-utf16Length(prefix)-utf16Length(suffix)) + suffix
}

func preparePromptText(message *telegram.Message, current string) string {
	context := formatReplyContext(extractReplyContext(message))
	if context == "" {
		return current
	}
	return context + "\n\n" + currentMessageTag + "\n" + current
}

func prepareReviewPrompt(message *telegram.Message, review string) string {
	context := formatReplyContext(extractReplyContext(message))
	if context == "" {
		return review
	}
	return review + " \n\n" + context
}

func nonEmptyReplyField(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return text
}

func replyContextBody(ctx ReplyContext) string {
	text := escapeReplyContextField(ctx.Text)
	switch ctx.Kind {
	case replyContextKindQuote:
		return "Quote:\n" + text
	case replyContextKindText:
		return "Message:\n" + text
	case replyContextKindPhoto:
		body := "[Photo]"
		if text != "" {
			body += "\nCaption:\n" + text
		}
		return body
	case replyContextKindDocument:
		body := "[Document]"
		if name := strings.TrimSpace(ctx.MediaName); name != "" {
			body = "[Document: " + escapeReplyContextField(ctx.MediaName) + "]"
		}
		if text != "" {
			body += "\nCaption:\n" + text
		}
		return body
	case replyContextKindUnsupported:
		return "[Unsupported Telegram message]"
	default:
		return ""
	}
}

func escapeReplyContextField(text string) string {
	text = strings.ReplaceAll(text, replyContextStart, "<escaped Telegram reply context>")
	text = strings.ReplaceAll(text, replyContextEnd, "<escaped /Telegram reply context>")
	return strings.ReplaceAll(text, currentMessageTag, "<escaped Current user message>")
}

func replyContextEnvelope(sender string) (string, string) {
	if sender == "" {
		sender = "user"
	}
	return fmt.Sprintf("%s\nFrom: %s\n", replyContextStart, sender), "\n" + replyContextEnd
}

func replyContextNeedsTruncation(ctx ReplyContext) bool {
	prefix, suffix := replyContextEnvelope(ctx.Sender)
	return utf16Length(replyContextBody(ctx)) > maxReplyContextUnits-utf16Length(prefix)-utf16Length(suffix)
}

func truncateReplyContext(text string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf16Length(text) <= max {
		return text
	}
	markerUnits := utf16Length(replyContextMarker)
	if max <= markerUnits {
		return clipUTF16(replyContextMarker, max)
	}
	return clipUTF16(text, max-markerUnits) + replyContextMarker
}
