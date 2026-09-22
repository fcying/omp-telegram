package telegram

import (
	"encoding/json"
	"net/http"
	"testing"
	"unicode/utf8"
)

func TestConvertMarkdownTableAligned(t *testing.T) {
	in := "Hier, als Test:\n\n| Backend | Host | VRAM (ca.) |\n|---|---|---|\n| STRIX/pwilkin | M2 | ~100+ GiB |\n| CIRU | M2 | — |"
	want := "Hier, als Test:\n\n<pre>Backend       | Host | VRAM (ca.)\n------------- | ---- | ----------\nSTRIX/pwilkin | M2   | ~100+ GiB\nCIRU          | M2   | —</pre>"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConvertMarkdownTableAlignmentDirectives(t *testing.T) {
	in := "| name | n |\n|:---|---:|\n| a | 1 |\n| bbbb | 22222 |"
	want := "<pre>name |     n\n---- | -----\na    |     1\nbbbb | 22222</pre>"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConvertMarkdownTableInlineMarksStripped(t *testing.T) {
	in := "| Flag | Note |\n|---|---|\n| **on** | use `x` now |"
	want := "<pre>Flag | Note\n---- | ---------\non   | use x now</pre>"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got: %s want: %s", got, want)
	}
}

func TestConvertMarkdownTableKeepsEveryLinkLabel(t *testing.T) {
	in := "| Links | Note |\n|---|---|\n| [A](https://a.example) [B](https://b.example) | ok |"
	want := "<pre>Links | Note\n----- | ----\nA B   | ok</pre>"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got: %s want: %s", got, want)
	}
}

func TestConvertMarkdownInlineFormatting(t *testing.T) {
	in := "A **bold** B *ital* C ~~gone~~ D `code(x)` E [link](https://example.com) done"
	want := "A <b>bold</b> B <i>ital</i> C <s>gone</s> D <code>code(x)</code> E <a href=\"https://example.com\">link</a> done"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:  %s\nwant: %s", got, want)
	}
}

func TestConvertMarkdownEscapesHTMLEntities(t *testing.T) {
	in := "if a < b && c > d { } <script>alert(1)</script> & co"
	want := "if a &lt; b &amp;&amp; c &gt; d { } &lt;script&gt;alert(1)&lt;/script&gt; &amp; co"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:  %s\nwant: %s", got, want)
	}
}

func TestConvertMarkdownCodeFenceAndUnclosedFence(t *testing.T) {
	in := "```go\nif a < b {\n\tfmt.Println(\"<b>\")\n}\n```\nafter"
	want := "<pre>if a &lt; b {\n\tfmt.Println(\"&lt;b&gt;\")\n}</pre>\nafter"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:  %s\nwant: %s", got, want)
	}
	in2 := "```\nx = 1"
	want2 := "<pre>x = 1</pre>"
	if got := ConvertMarkdown(in2); got != want2 {
		t.Fatalf("unclosed fence: got %s want %s", got, want2)
	}
}

func TestConvertMarkdownHeadingsRulesQuotes(t *testing.T) {
	in := "## Title `t`\n\n----\n\n> quoted **text**\n> second line"
	want := "<b>Title <code>t</code></b>\n\n──────────\n\n<blockquote>quoted <b>text</b>\nsecond line</blockquote>"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:  %s\nwant: %s", got, want)
	}
}

func TestConvertMarkdownLists(t *testing.T) {
	in := "- one\n- two **bold**\n  continuation\n1. first\n2. second"
	want := "- one\n- two <b>bold</b>\n  continuation\n1. first\n2. second"
	if got := ConvertMarkdown(in); got != want {
		t.Fatalf("got:  %s\nwant: %s", got, want)
	}
}

func TestConvertMarkdownPlainTextUnchanged(t *testing.T) {
	in := "just text\nsecond line"
	if got := ConvertMarkdown(in); got != in {
		t.Fatalf("plain text changed: %s", got)
	}
}

func FuzzConvertMarkdown(f *testing.F) {
	for _, seed := range []string{"plain", "```go\nvalue < 1\n```", "| a | b |\n|---|---|\n| 1 | 2 |", "> quote\n> text"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if output := ConvertMarkdown(input); !utf8.ValidString(output) {
			t.Fatal("markdown conversion produced invalid UTF-8")
		}
	})
}

func FuzzSplitForTelegram(f *testing.F) {
	for _, seed := range []string{
		"plain",
		"emoji 😀😀😀",
		"```go\nvalue < 1\n```",
		"| a | b |\n|---|---|\n| 1 | 2 |",
		"> quote\n> text",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		for index, part := range SplitForTelegram(input, MaxMessageUTF16) {
			if got := renderedUTF16Len(ConvertMarkdown(part)); got > MaxMessageUTF16 {
				t.Fatalf("part %d rendered length = %d, limit %d", index, got, MaxMessageUTF16)
			}
		}
	})
}

func TestSendAddsHTMLParseMode(t *testing.T) {
	var bodies []map[string]any
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 7}})
	})
	if _, err := client.Send(t.Context(), 1, 0, "**hi**", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if bodies[0]["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %v, want HTML", bodies[0]["parse_mode"])
	}
	if bodies[0]["text"] != "<b>hi</b>" {
		t.Fatalf("text = %v", bodies[0]["text"])
	}
}

func TestSendFallsBackToPlainOnEntityParseError(t *testing.T) {
	seen := 0
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen++
		if seen == 1 {
			if body["parse_mode"] != "HTML" {
				t.Fatalf("first send should use HTML, got %v", body["parse_mode"])
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: can't parse entities: unsupported start tag \"code\" at byte offset 3"})
			return
		}
		if _, ok := body["parse_mode"]; ok {
			t.Fatalf("fallback must not set parse_mode: %v", body["parse_mode"])
		}
		if body["text"] != "a <`code`> b" {
			t.Fatalf("fallback text = %v, want original source", body["text"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 8}})
	})
	if _, err := client.Send(t.Context(), 1, 0, "a <`code`> b", SendOptions{}); err != nil {
		t.Fatalf("expected successful plain fallback, got %v", err)
	}
	if seen != 2 {
		t.Fatalf("requests = %d, want 2", seen)
	}
}

func TestSendPlainWhenConvertedExceedsLimit(t *testing.T) {
	var bodies []map[string]any
	client := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 9}})
	})
	long := ""
	for i := 0; i < 2000; i++ {
		long += "<>&"
	}
	if _, err := client.Send(t.Context(), 1, 0, long, SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	if _, ok := bodies[0]["parse_mode"]; ok {
		t.Fatalf("over-length conversion must send plain, parse_mode = %v", bodies[0]["parse_mode"])
	}
	if bodies[0]["text"] != long {
		t.Fatal("over-length fallback must preserve original text")
	}
}
