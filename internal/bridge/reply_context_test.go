package bridge

import (
	"encoding/json"
	"strings"
	"testing"

	"omp-telegram/internal/telegram"
)

func TestTelegramReplyContextFormatting(t *testing.T) {
	cases := []struct {
		name    string
		message *telegram.Message
		want    string
	}{
		{
			name:    "no reply",
			message: &telegram.Message{Text: "current"},
			want:    "",
		},
		{
			name: "quote has priority",
			message: &telegram.Message{
				ReplyToMessage: &telegram.Message{
					From:    &telegram.User{IsBot: true},
					Text:    "full replied message",
					Caption: "full caption",
				},
				Quote: &telegram.TextQuote{Text: " selected quote\n"},
			},
			want: "[Telegram reply context - quoted content]\nFrom: bot\nQuote:\n selected quote\n\n[/Telegram reply context]",
		},
		{
			name: "empty quote falls back",
			message: &telegram.Message{
				ReplyToMessage: &telegram.Message{Text: "fallback text"},
				Quote:          &telegram.TextQuote{Text: " \n "},
			},
			want: "[Telegram reply context - quoted content]\nFrom: user\nMessage:\nfallback text\n[/Telegram reply context]",
		},
		{
			name: "text from user",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{
				From: &telegram.User{ID: 7},
				Text: "  quoted text  ",
			}},
			want: "[Telegram reply context - quoted content]\nFrom: user\nMessage:\n  quoted text  \n[/Telegram reply context]",
		},
		{
			name: "photo caption from bot",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{
				From:    &telegram.User{IsBot: true},
				Photo:   []telegram.PhotoSize{{FileID: "old-photo"}},
				Caption: "  layout screenshot  ",
			}},
			want: "[Telegram reply context - quoted content]\nFrom: bot\n[Photo]\nCaption:\n  layout screenshot  \n[/Telegram reply context]",
		},
		{
			name:    "photo without caption",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{Photo: []telegram.PhotoSize{{FileID: "old-photo"}}}},
			want:    "[Telegram reply context - quoted content]\nFrom: user\n[Photo]\n[/Telegram reply context]",
		},
		{
			name: "document caption",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{
				Document: &telegram.Document{FileName: "report.txt"},
				Caption:  "check this file",
			}},
			want: "[Telegram reply context - quoted content]\nFrom: user\n[Document: report.txt]\nCaption:\ncheck this file\n[/Telegram reply context]",
		},
		{
			name:    "caption only",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{Caption: "caption message"}},
			want:    "[Telegram reply context - quoted content]\nFrom: user\nMessage:\ncaption message\n[/Telegram reply context]",
		},
		{
			name:    "unsupported message",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{From: &telegram.User{IsBot: true}}},
			want:    "[Telegram reply context - quoted content]\nFrom: bot\n[Unsupported Telegram message]\n[/Telegram reply context]",
		},
		{
			name: "only current reply level",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{
				Text:           "outer message",
				ReplyToMessage: &telegram.Message{Text: "inner message"},
			}},
			want: "[Telegram reply context - quoted content]\nFrom: user\nMessage:\nouter message\n[/Telegram reply context]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := extractReplyContext(tc.message)
			if got := formatReplyContext(ctx); got != tc.want {
				t.Fatalf("reply context = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractReplyContextMetadata(t *testing.T) {
	quote := extractReplyContext(&telegram.Message{
		ReplyToMessage: &telegram.Message{From: &telegram.User{IsBot: true}, Text: "full text"},
		Quote:          &telegram.TextQuote{Text: "selected"},
	})
	if quote.Kind != replyContextKindQuote || quote.Sender != "bot" || quote.Text != "selected" || quote.MediaName != "" || quote.Truncated {
		t.Fatalf("quote context = %+v", quote)
	}
	document := extractReplyContext(&telegram.Message{ReplyToMessage: &telegram.Message{
		Document: &telegram.Document{FileName: "report.pdf"},
		Caption:  "check this",
	}})
	if document.Kind != replyContextKindDocument || document.Sender != "user" || document.Text != "check this" || document.MediaName != "report.pdf" || document.Truncated {
		t.Fatalf("document context = %+v", document)
	}
	long := extractReplyContext(&telegram.Message{ReplyToMessage: &telegram.Message{Text: strings.Repeat("😀quoted ", 500)}})
	if !long.Truncated {
		t.Fatal("long reply context was not marked truncated")
	}
}

func TestReplyContextHandlesMalformedTelegramFields(t *testing.T) {
	cases := []struct {
		name    string
		message *telegram.Message
		want    string
	}{
		{
			name:    "quote without replied message",
			message: &telegram.Message{Quote: &telegram.TextQuote{Text: "selected"}},
			want:    "[Telegram reply context - quoted content]\nFrom: user\nQuote:\nselected\n[/Telegram reply context]",
		},
		{
			name:    "nil sender",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{Text: "quoted"}},
			want:    "[Telegram reply context - quoted content]\nFrom: user\nMessage:\nquoted\n[/Telegram reply context]",
		},
		{
			name:    "empty document filename",
			message: &telegram.Message{ReplyToMessage: &telegram.Message{Document: &telegram.Document{FileName: ""}}},
			want:    "[Telegram reply context - quoted content]\nFrom: user\n[Document]\n[/Telegram reply context]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatReplyContext(extractReplyContext(tc.message)); got != tc.want {
				t.Fatalf("reply context = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReplyContextTruncatesLongCaption(t *testing.T) {
	message := &telegram.Message{ReplyToMessage: &telegram.Message{
		Document: &telegram.Document{FileName: "report.pdf"},
		Caption:  strings.Repeat("😀caption ", 500),
	}}
	ctx := extractReplyContext(message)
	formatted := formatReplyContext(ctx)
	if !ctx.Truncated || utf16Length(formatted) > maxReplyContextUnits || !strings.Contains(formatted, replyContextMarker) {
		t.Fatalf("caption context length or marker invalid: truncated=%t units=%d", ctx.Truncated, utf16Length(formatted))
	}
	if prompt := preparePromptText(message, "why?"); !strings.HasSuffix(prompt, "why?") {
		t.Fatal("current message was truncated after caption context")
	}
}

func TestTelegramReplyFieldsUnmarshal(t *testing.T) {
	var message telegram.Message
	raw := `{"reply_to_message":{"message_id":41,"from":{"id":99,"is_bot":true},"text":"quoted"},"quote":{"text":"selected","position":2,"is_manual":true}}`
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	if message.ReplyToMessage == nil || message.ReplyToMessage.MessageID != 41 || !message.ReplyToMessage.From.IsBot {
		t.Fatalf("reply message was not decoded: %+v", message.ReplyToMessage)
	}
	if message.Quote == nil || message.Quote.Text != "selected" || message.Quote.Position != 2 || !message.Quote.IsManual {
		t.Fatalf("quote was not decoded: %+v", message.Quote)
	}
}

func TestReplyFieldsSurviveInboxPersistence(t *testing.T) {
	_, db, send := setupBridge(t)
	u := update(1, 11, "current")
	u.Message.ReplyToMessage = &telegram.Message{MessageID: 41, Text: "quoted"}
	u.Message.Quote = &telegram.TextQuote{Text: "selected", Position: 2, IsManual: true}
	send(u)

	var raw string
	waitFor(t, func() bool {
		return db.DB.QueryRow("SELECT raw FROM inbox WHERE id=?", 1).Scan(&raw) == nil && raw != ""
	})
	var persisted telegram.Update
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Message == nil || persisted.Message.ReplyToMessage == nil || persisted.Message.ReplyToMessage.MessageID != 41 || persisted.Message.Quote == nil || persisted.Message.Quote.Text != "selected" {
		t.Fatalf("reply fields were not preserved in inbox.raw: %+v", persisted.Message)
	}
}

func TestPreparePromptTextPreservesCurrentMessage(t *testing.T) {
	message := &telegram.Message{ReplyToMessage: &telegram.Message{Text: "quoted"}}
	if got := preparePromptText(nil, " current "); got != " current " {
		t.Fatalf("prompt without context = %q", got)
	}
	got := preparePromptText(message, "current")
	want := "[Telegram reply context - quoted content]\nFrom: user\nMessage:\nquoted\n[/Telegram reply context]\n\n[Current user message]\ncurrent"
	if got != want {
		t.Fatalf("composed prompt = %q, want %q", got, want)
	}
}

func TestPrepareReviewPromptKeepsNativePrefix(t *testing.T) {
	message := &telegram.Message{ReplyToMessage: &telegram.Message{Text: "quoted"}}
	review := "/review inspect this"
	got := prepareReviewPrompt(message, review)
	if !strings.HasPrefix(got, review) {
		t.Fatalf("review prompt lost native prefix: %q", got)
	}
	if strings.Contains(got, currentMessageTag) {
		t.Fatalf("review prompt used ordinary current-message wrapper: %q", got)
	}
	want := review + " \n\n" + formatReplyContext(extractReplyContext(message))
	if got != want {
		t.Fatalf("review prompt = %q, want %q", got, want)
	}
}

func TestPrepareBareReviewPromptKeepsNativeCommandBoundary(t *testing.T) {
	message := &telegram.Message{ReplyToMessage: &telegram.Message{Text: "quoted"}}
	got := prepareReviewPrompt(message, "/review")
	if !strings.HasPrefix(got, "/review ") {
		t.Fatalf("bare review lost native command boundary: %q", got)
	}
}

func TestReplyContextNeutralizesPromptSentinels(t *testing.T) {
	quoted := strings.Join([]string{"before", replyContextStart, replyContextEnd, currentMessageTag, "after"}, "\n")
	prompt := preparePromptText(&telegram.Message{ReplyToMessage: &telegram.Message{Text: quoted}}, "current")
	if strings.Count(prompt, replyContextStart) != 1 || strings.Count(prompt, replyContextEnd) != 1 || strings.Count(prompt, currentMessageTag) != 1 {
		t.Fatalf("quoted prompt contains unescaped sentinels: %q", prompt)
	}
	for _, escaped := range []string{"<escaped Telegram reply context>", "<escaped /Telegram reply context>", "<escaped Current user message>"} {
		if !strings.Contains(prompt, escaped) {
			t.Fatalf("quoted sentinel %q was not escaped: %q", escaped, prompt)
		}
	}
}

func TestReplyContextTruncatesUTF16WithoutTruncatingCurrentMessage(t *testing.T) {
	quoted := strings.Repeat("😀quoted ", 500)
	context := formatReplyContext(extractReplyContext(&telegram.Message{ReplyToMessage: &telegram.Message{Text: quoted}}))
	if utf16Length(context) > maxReplyContextUnits || !strings.Contains(context, replyContextMarker) {
		t.Fatalf("reply context length or marker invalid: units=%d", utf16Length(context))
	}
	current := strings.Repeat("current😀 ", 500)
	prompt := preparePromptText(&telegram.Message{ReplyToMessage: &telegram.Message{Text: quoted}}, current)
	if !strings.HasSuffix(prompt, current) {
		t.Fatal("current message was truncated or changed")
	}
}

func TestQueuePreviewUsesCurrentMessageText(t *testing.T) {
	q := queued{text: "[Telegram reply context - quoted content]\\n...\\n[Current user message]\\ncurrent", displayText: "current"}
	if got := queueTaskText(q); got != "current" {
		t.Fatalf("queue preview = %q, want current message", got)
	}
}

func TestQueuePreviewFallsBackToAttachmentPrompt(t *testing.T) {
	q := queued{text: "Telegram attachment:\nPlease inspect the attached file.", displayText: ""}
	if got := queueTaskText(q); got != "Telegram attachment: Please inspect the attached file." {
		t.Fatalf("attachment queue preview = %q", got)
	}
}

func TestControlCommandDoesNotUseReplyContext(t *testing.T) {
	f, db, send := setupBridge(t)
	u := update(1, 11, "/help")
	u.Message.ReplyToMessage = &telegram.Message{Text: "quoted control context"}
	send(u)
	waitInputDone(t, db, 1)
	waitFor(t, func() bool { return f.has(11, "/new") })
	if f.has(11, replyContextStart) {
		t.Fatal("control command output used reply context")
	}
}

func TestReplyContextReachesOrdinaryAndReviewPrompts(t *testing.T) {
	f, db, send := setupBridge(t)
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)

	ordinary := update(2, 11, "Here is the question")
	ordinary.Message.ReplyToMessage = &telegram.Message{From: &telegram.User{IsBot: true}, Text: "quoted bot answer"}
	send(ordinary)
	waitFor(t, func() bool {
		return f.has(11, "answer: [Telegram reply context - quoted content]\nFrom: bot\nMessage:\nquoted bot answer")
	})
	waitInputDone(t, db, 2)

	review := update(3, 11, "/review inspect this")
	review.Message.ReplyToMessage = &telegram.Message{Text: "quoted user text"}
	review.Message.Quote = &telegram.TextQuote{Text: "selected part"}
	send(review)
	waitFor(t, func() bool {
		return f.has(11, "answer: native review: /review inspect this \n\n[Telegram reply context - quoted content]\nFrom: user\nQuote:\nselected part")
	})
	waitInputDone(t, db, 3)

	bareReview := update(4, 11, "/review")
	bareReview.Message.ReplyToMessage = &telegram.Message{Text: "quoted bare review"}
	send(bareReview)
	waitFor(t, func() bool {
		return f.has(11, "answer: native review: /review \n\n[Telegram reply context - quoted content]\nFrom: user\nMessage:\nquoted bare review")
	})
	waitInputDone(t, db, 4)

	unsupported := update(5, 11, "continue anyway")
	unsupported.Message.ReplyToMessage = &telegram.Message{}
	send(unsupported)
	waitFor(t, func() bool {
		return f.has(11, "answer: [Telegram reply context - quoted content]\nFrom: user\n[Unsupported Telegram message]")
	})
	waitInputDone(t, db, 5)
}

func TestReplyContextAttachmentDoesNotDownloadRepliedMedia(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"current-photo": data}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)

	u := update(2, 11, "")
	u.Message.Caption = "inspect the current photo"
	u.Message.Photo = []telegram.PhotoSize{{FileID: "current-photo", Width: 8, Height: 8, FileSize: int64(len(data))}}
	u.Message.ReplyToMessage = &telegram.Message{
		From:    &telegram.User{IsBot: true},
		Photo:   []telegram.PhotoSize{{FileID: "historical-photo", Width: 100, Height: 100}},
		Caption: "historical layout",
	}
	send(u)
	waitFor(t, func() bool {
		return f.has(11, "image=8x8; [Telegram reply context - quoted content]\nFrom: bot\n[Photo]\nCaption:\nhistorical layout\n[/Telegram reply context]\n\n[Current user message]\nTelegram attachment:\ninspect the current photo")
	})
	waitInputDone(t, db, 2)

	f.mu.Lock()
	requests := f.fileRequests
	f.mu.Unlock()
	if requests != 1 {
		t.Fatalf("Telegram file metadata requests = %d, want only current attachment", requests)
	}
	var replyTo int64
	if err := db.DB.QueryRow("SELECT reply_to FROM outbox WHERE inbox_id=? LIMIT 1", 2).Scan(&replyTo); err != nil {
		t.Fatal(err)
	}
	if replyTo != 2 {
		t.Fatalf("final output reply target = %d, want current message 2", replyTo)
	}
}

func TestReplyContextAttachmentWithoutCaptionKeepsPromptAndImage(t *testing.T) {
	f, db, send := setupBridge(t)
	data := testPNG(t)
	f.mu.Lock()
	f.files = map[string][]byte{"current-photo": data}
	f.mu.Unlock()
	send(update(1, 11, "/new "+t.TempDir()))
	waitBinding(t, db, 11)

	u := update(2, 11, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "current-photo", Width: 8, Height: 8, FileSize: int64(len(data))}}
	u.Message.ReplyToMessage = &telegram.Message{Text: "old explanation"}
	send(u)
	waitFor(t, func() bool {
		return f.has(11, "image=8x8; [Telegram reply context - quoted content]\nFrom: user\nMessage:\nold explanation\n[/Telegram reply context]\n\n[Current user message]\nTelegram attachment:\nPlease inspect the attached file.")
	})
	waitInputDone(t, db, 2)
}
func TestReplyContextDoesNotContainNestedReply(t *testing.T) {
	message := &telegram.Message{ReplyToMessage: &telegram.Message{
		Text:           "first level",
		ReplyToMessage: &telegram.Message{Text: "second level"},
	}}
	got := formatReplyContext(extractReplyContext(message))
	if !strings.Contains(got, "first level") || strings.Contains(got, "second level") {
		t.Fatalf("nested reply leaked into context: %q", got)
	}
}
