package telegram

import (
	"encoding/json"
	"net/http"
	"strings"
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

func TestConvertMarkdownNestedInlineRuns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"italic", "*x*", "<i>x</i>"},
		{"bold", "**x**", "<b>x</b>"},
		{"triple", "***x***", "<i><b>x</b></i>"},
		{"italic inside bold", "**a *b***", "<b>a <i>b</i></b>"},
		{"bold inside italic", "*a **b***", "<i>a <b>b</b></i>"},
		{"separate nested close", "**a *b* c**", "<b>a <i>b</i> c</b>"},
		{"underscore triple", "___x___", "<i><b>x</b></i>"},
		{"intraword bold underscores", "a__b__c", "a<b>b</b>c"},
		{"strikethrough", "~~a **b**~~", "<s>a <b>b</b></s>"},
		{"identifier", "snake_case and snake__case", "snake_case and snake__case"},
		{"unicode", "**中文 *内容***", "<b>中文 <i>内容</i></b>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(tc.in); got != tc.want {
				t.Fatalf("got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestConvertMarkdownDelimiterSearchBounds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unmatched marker runs", "*a *a a_ a_", "*a *a a_ a_"},
		{"failed closer becomes opener", "a*b*c", "a<i>b</i>c"},
		{"failed strike closer becomes opener", "a~~b~~c", "a<s>b</s>c"},
		{"new opener after failed search", "a* b *c*", "a* b <i>c</i>"},
		{"underscore search keeps star opener", "*a b_ c*", "<i>a b_ c</i>"},
		{"star search keeps underscore opener", "_a b* c_", "<i>a b* c</i>"},
		{"strike search keeps star opener", "**a b~~ c**", "<b>a b~~ c</b>"},
		{"closer opening capability is independent", "*a ~~b**c~~ d**", "<i>a <s>b**c</s> d</i>*"},
		{"remainder one is independent", "*a ~~b**c~~ d*e", "<i>a <s>b**c</s> d</i>e"},
		{"remainder zero is independent", "**a ~~b*c~~ d***e", "<b>a <s>b*c</s> d</b>*e"},
		{"rule of three uses original run lengths", "x***a**b**", "x<i><b>a</b>b</i>*"},
		{"strike residuals stay literal", "text ~~~x~~~ ~~~y~~~", "text ~<s>x</s>~ ~<s>y</s>~"},
		{"crossing opener stays literal", "**a _b** c_", "<b>a _b</b> c_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(tc.in); got != tc.want {
				t.Fatalf("source: %q\ngot: %s\nwant: %s", tc.in, got, tc.want)
			}
		})
	}
}

func BenchmarkConvertMarkdownDelimiterSearch(b *testing.B) {
	for _, size := range []struct {
		name string
		runs int
	}{
		{"1K", 1024},
		{"8K", 8192},
		{"32K", 32768},
	} {
		for _, input := range []struct {
			name string
			text string
		}{
			{"unmatched", strings.Repeat("*a ", size.runs) + strings.Repeat("a_ ", size.runs)},
			{"strike residuals", "text " + strings.Repeat("~~~x~~~ ", size.runs)},
		} {
			b.Run(input.name+"/"+size.name, func(b *testing.B) {
				b.SetBytes(int64(len(input.text)))
				b.ReportAllocs()
				for range b.N {
					ConvertMarkdown(input.text)
				}
			})
		}
	}
}

func TestConvertMarkdownInlineCodeSuspendsOtherEntities(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bold around code", "**a `b` c**", "<b>a </b><code>b</code><b> c</b>"},
		{"heading around code", "## a `b` c", "<b>a </b><code>b</code><b> c</b>"},
		{"only code", "**`b`**", "<code>b</code>"},
		{"bold between code spans", "**`whatsapp-ng` continues `whatsapp-pi`**", "<code>whatsapp-ng</code><b> continues </b><code>whatsapp-pi</code>"},
		{"nested around code", "***a `b` c***", "<i><b>a </b></i><code>b</code><i><b> c</b></i>"},
		{"literal code", "`**<b>&_x_**`", "<code>**&lt;b&gt;&amp;_x_**</code>"},
		{"literal backtick", "``a ` **b**``", "<code>a ` **b**</code>"},
		{"link label code", "[a `b` c](https://example.com)", `<a href="https://example.com">a </a><code>b</code><a href="https://example.com"> c</a>`},
		{"bold link code", "**[a `b` c](https://example.com)**", `<b><a href="https://example.com">a </a></b><code>b</code><b><a href="https://example.com"> c</a></b>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(tc.in); got != tc.want {
				t.Fatalf("got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestConvertMarkdownLinksKeepURLsAndEscapeLabels(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`[**<label>&**](https://example.com/a_*?q=1&x="v")`, `<a href="https://example.com/a_*?q=1&amp;x=&quot;v&quot;"><b>&lt;label&gt;&amp;</b></a>`},
		{"**a [*b*](https://example.com/a_*) c**", `<b>a <a href="https://example.com/a_*"><i>b</i></a> c</b>`},
		{"![**alt**](https://example.com/image)", `<a href="https://example.com/image">🖼 <b>alt</b></a>`},
		{"\x010\x02 [link](https://example.com)", "\x010\x02 " + `<a href="https://example.com">link</a>`},
		{"`[hidden](https://hidden.example)` [shown](https://shown.example)", `<code>[hidden](https://hidden.example)</code> <a href="https://shown.example">shown</a>`},
	}
	for _, tc := range cases {
		if got := ConvertMarkdown(tc.in); got != tc.want {
			t.Fatalf("source: %q\ngot: %s\nwant: %s", tc.in, got, tc.want)
		}
	}
}

func TestPreviewInlineRendersReadableIncompleteSource(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"open bold", "**x", "x"},
		{"partial bold close", "**x*", "x"},
		{"open nested run", "***x**", "x"},
		{"open outer emphasis", "**a *b*", "a <i>b</i>"},
		{"complete bold", "**x**", "<b>x</b>"},
		{"complete nested", "**a *b***", "<b>a <i>b</i></b>"},
		{"open code", "`**x", "<code>**x</code>"},
		{"open code with complete emphasis", "`**x**", "<code>**x**</code>"},
		{"partial code close", "``**x`", "<code>**x</code>"},
		{"complete code", "`**x**`", "<code>**x**</code>"},
		{"code inside open bold", "**a `**x", "a <code>**x</code>"},
		{"escaped preview code", "`<b>&**x", "<code>&lt;b&gt;&amp;**x</code>"},
		{"multiplication", "2 * 3 and a*b", "2 * 3 and a*b"},
		{"punctuation", "hello *", "hello *"},
		{"punctuation after star", "*!", "*!"},
		{"escaped opener", `\**literal`, `**literal`},
		{"escaped code opener", "\\`literal", "`literal"},
		{"identifier", "snake_case", "snake_case"},
		{"link URL", "[x](https://example.com/a_*)", `<a href="https://example.com/a_*">x</a>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(previewInline(tc.in)); got != tc.want {
				t.Fatalf("preview got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestConvertMarkdownFinalDoesNotUsePreviewProjection(t *testing.T) {
	for _, input := range []string{"**unfinished", "`**literal**"} {
		if got := ConvertMarkdown(input); got != input {
			t.Fatalf("final conversion of %q = %q, want literal incomplete source", input, got)
		}
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
	want := "<b>Title </b><code>t</code>\n\n──────────\n\n<blockquote>quoted <b>text</b>\nsecond line</blockquote>"
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

func TestConvertMarkdownPreservesLooseListParagraphs(t *testing.T) {
	for _, source := range []string{
		"- first\n\n- second",
		"- first\n\n\n- second",
		"1. first\n\n2. second",
		"- parent\n\n  - child\n\n- next",
		"- first\n\n  continuation",
	} {
		if got := ConvertMarkdown(source); got != source {
			t.Fatalf("explicit list paragraphs changed: got %q, want %q", got, source)
		}
	}
	for _, part := range SplitForTelegram("- first\n\n- second", MaxMessageUTF16) {
		if got := ConvertMarkdown(part); got != "- first\n\n- second" {
			t.Fatalf("final reply compacted list paragraphs: %q", got)
		}
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

func TestConvertMarkdownSoftNewlineParagraphs(t *testing.T) {
	cases := []struct {
		name, source, want string
	}{
		{"bold", "**first\nsecond**", "<b>first\nsecond</b>"},
		{"nested", "**first *inner\nsecond* last**", "<b>first <i>inner\nsecond</i> last</b>"},
		{"code", "`first\nsecond` tail", "<code>first\nsecond</code> tail"},
		{"link", "[first\nsecond](https://example.com)", "<a href=\"https://example.com\">first\nsecond</a>"},
		{"explicit blank lines", "**first\n\n\nsecond**", "**first\n\n\nsecond**"},
		{"heading boundary", "**first\n# second**", "**first\n<b>second**</b>"},
		{"list boundary", "**first\n- second**", "**first\n- second**"},
		{"quote boundary", "**first\n> second**", "**first\n<blockquote>second**</blockquote>"},
		{"fence boundary", "**first\n```\nsecond**\n```", "**first\n<pre>second**</pre>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(tc.source); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConvertMarkdownParagraphStopsAtTableBoundary(t *testing.T) {
	rendered := ConvertMarkdown("**first\n|a|\n|-|\n|second**|")
	before, table, found := strings.Cut(rendered, "\n<pre>")
	if !found || before != "**first" || !strings.HasSuffix(table, "</pre>") {
		t.Fatalf("ordinary paragraph swallowed the table boundary: %q", rendered)
	}
	if strings.Contains(rendered, "<b>") || !strings.Contains(table, "second") {
		t.Fatalf("emphasis matched across a table instead of preserving its cells: %q", rendered)
	}
}

func TestTailForTelegramPreservesCompleteMultilineInline(t *testing.T) {
	for _, source := range []string{
		"**first\nsecond** tail", "*first\nsecond* tail", "`first\nsecond` tail",
		"[**first\nsecond**](https://example.com) tail", "**first *inner\nsecond* last**",
	} {
		if got, want := ConvertMarkdown(TailForTelegram(source, 4096)), ConvertMarkdown(source); got != want {
			t.Fatalf("source %q: preview %q, final %q", source, got, want)
		}
	}
}

func TestConvertMarkdownBackslashEscapes(t *testing.T) {
	cases := []struct {
		source, want string
	}{
		{`\*x\*`, `*x*`},
		{`\**bold**`, `*<i>bold</i>*`},
		{`\\**bold**`, `\<b>bold</b>`},
		{`\\\**bold**`, `\*<i>bold</i>*`},
		{`\[label](https://example.com)`, `[label](https://example.com)`},
		{`\![label](https://example.com)`, `![label](https://example.com)`},
		{"\\`literal\\`", "`literal`"},
		{"`a\\*b\\`", "<code>a\\*b\\</code>"},
		{`[a\*b\* \] c](https://example.com/a\*b)`, `<a href="https://example.com/a\*b">a*b* ] c</a>`},
		{`path\name \a`, `path\name \a`},
		{"`` `body` ``", "<code>`body`</code>"},
		{"`   `", "<code>   </code>"},
		{"`  body  `", "<code> body </code>"},
	}
	for _, tc := range cases {
		if got := ConvertMarkdown(tc.source); got != tc.want {
			t.Fatalf("source %q: got %q, want %q", tc.source, got, tc.want)
		}
	}
	const punctuation = "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~"
	var source strings.Builder
	for i := range len(punctuation) {
		source.WriteByte('\\')
		source.WriteByte(punctuation[i])
	}
	if got, want := ConvertMarkdown(source.String()), escapeHTML(punctuation); got != want {
		t.Fatalf("escaped punctuation: got %q, want %q", got, want)
	}
}

func TestSplitForTelegramSerializesIntrawordEntities(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		limit        int
		parts        int
	}{
		{"nested stars", "a***b***c", 2, 2},
		{"nested underscores", "a___b___c", 2, 2},
		{"nested style transition", "a**b *c***d", 3, 0},
		{"nested strike transition", "a***b~~c~~d***e", 4, 0},
		{"code beside letters", "a``` `` ```b", 3, 2},
		{"code beside both styles", "**a**``` `` ```*b*c", 3, 0},
		{"code with literal backticks", "a`` `x` ``b", 4, 2},
		{"multiple code run widths", "a```` ` `` ``` ````b" + strings.Repeat("x", 12), 5, 0},
		{"code at a new message boundary", strings.Repeat("x", 7) + "````a`b``c````d" + strings.Repeat("x", 30), 8, 0},
		{"soft newline styles", "prefix\n***b\nc***d\n'***e***' \"***f***\"" + strings.Repeat("x", 10), 7, 0},
		{"quote punctuation", "a'***b***'c \"***d***\"e" + strings.Repeat("x", 10), 4, 0},
		{"linked code delimiter", "[````a```b````](https://example.com)c" + strings.Repeat("x", 100), 32, 0},
		{"production nested emphasis", strings.Repeat("a", MaxMessageUTF16-1) + "***b***c", MaxMessageUTF16, 2},
		{"production adjacent code", strings.Repeat("a", MaxMessageUTF16-2) + "``` `` ```b", MaxMessageUTF16, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := assertRichSplitEquivalent(t, tc.source, tc.limit)
			if tc.parts > 0 && len(parts) != tc.parts {
				t.Fatalf("ordinary inline context required unnecessary messages: got %d, want %d", len(parts), tc.parts)
			}
		})
	}
}

func TestConvertMarkdownEscapedBracketPrefixesKeepLaterLinks(t *testing.T) {
	prefix := strings.Repeat(`\[`, 8192)
	for _, tc := range []struct {
		name, source, want string
	}{
		{"escaped openers", prefix + "x](u)", strings.Repeat("[", 8192) + "x](u)"},
		{"later valid link", prefix + `x](u) [ok](https://example.com)`, strings.Repeat("[", 8192) + `x](u) <a href="https://example.com">ok</a>`},
		{"escaped image", `\![image](u) [ok](v)`, `![image](u) <a href="v">ok</a>`},
		{"escaped image label closer", `\![a\]b](u) [ok](v)`, `![a]b](u) <a href="v">ok</a>`},
		{"escaped label closer", `[a\]b](u) [ok](v)`, `<a href="u">a]b</a> <a href="v">ok</a>`},
		{"code is opaque", "`[fake](u)` [real](v)", `<code>[fake](u)</code> <a href="v">real</a>`},
		{"overlapping code candidate", "[`bad] [inside](u)` [last](v)", `[<code>bad] [inside](u)</code> <a href="v">last</a>`},
		{"URL is opaque", `[first](u[inside](v)) [last](z)`, `<a href="u[inside](v">first</a>) <a href="z">last</a>`},
		{"quoted titles", `[double](u "title") [single](v 'title') [spaced](  w  )`, `<a href="u">double</a> <a href="v">single</a> <a href="w">spaced</a>`},
		{"invalid title keeps later link", `[bad](u "unterminated) [ok](v)`, `[bad](u "unterminated) <a href="v">ok</a>`},
		{"escaped LF cannot extend a label", "[bad\\\n[ok](v)", "[bad\\\n<a href=\"v\">ok</a>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ConvertMarkdown(tc.source); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	codePrefix := strings.Repeat("`[x` ", 1024)
	if got, want := ConvertMarkdown(codePrefix+"[ok](v)"), strings.Repeat("<code>[x</code> ", 1024)+`<a href="v">ok</a>`; got != want {
		t.Fatalf("overlapping code candidates hid the later real link: got %q, want %q", got, want)
	}
}

func BenchmarkConvertMarkdownEscapedBracketSearch(b *testing.B) {
	for _, size := range []struct {
		name string
		runs int
	}{
		{"1K", 1024},
		{"8K", 8192},
		{"32K", 32768},
	} {
		for _, input := range []struct {
			name, source string
		}{
			{"escaped brackets", strings.Repeat(`\[`, size.runs) + "x](u) [ok](https://example.com)"},
			{"escaped images", strings.Repeat(`\![`, size.runs) + "x](u) [ok](https://example.com)"},
			{"overlapping code", strings.Repeat("`[x` ", size.runs) + "[ok](https://example.com)"},
		} {
			b.Run(input.name+"/"+size.name, func(b *testing.B) {
				b.SetBytes(int64(len(input.source)))
				b.ReportAllocs()
				for range b.N {
					ConvertMarkdown(input.source)
				}
			})
		}
	}
}

func TestSplitForTelegramKeepsNULWithinEntities(t *testing.T) {
	for _, test := range []struct {
		name, body string
		limit      int
		parts      int
	}{
		{"single glyph", "a\x00b", 1, 3},
		{"production boundary", strings.Repeat("a", 4095) + "\x00b", MaxMessageUTF16, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := SplitForTelegram("**"+test.body+"**", test.limit)
			if len(parts) != test.parts {
				t.Fatalf("NUL expanded a bounded body into %d messages, want %d", len(parts), test.parts)
			}
			var visible strings.Builder
			for i, part := range parts {
				rendered := ConvertMarkdown(part)
				if renderedUTF16Len(rendered) > test.limit || !strings.HasPrefix(rendered, "<b>") || !strings.HasSuffix(rendered, "</b>") {
					t.Fatalf("part %d lost its bounded bold entity: %q", i, rendered)
				}
				visible.WriteString(plainHTMLText(rendered))
			}
			if visible.String() != test.body {
				t.Fatal("NUL pagination changed or truncated the original body")
			}
		})
	}
}
