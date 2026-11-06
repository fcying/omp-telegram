package telegram

import (
	"encoding/xml"
	"html"
	"io"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestConvertMarkdownWideTableFallsBackToLabelledLines(t *testing.T) {
	in := "| Tool | Zweck | Beispiel |\n|---|---|---|\n| `read` | Dateien, Archive, SQLite, URLs und mehr | `read(\"x.ts:1-2\")` |\n| `bash` | Echte Binaries | `df -h` |"
	want := "Tool: read · Zweck: Dateien, Archive, SQLite, URLs und mehr · Beispiel: read(\"x.ts:1-2\")\nTool: bash · Zweck: Echte Binaries · Beispiel: df -h"
	got := ConvertMarkdown(in)
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "<pre>") {
		t.Fatal("wide table must not use a fixed-width block")
	}
}

func TestConvertMarkdownWideTableEscapesCompactHeaders(t *testing.T) {
	in := "| <b>x</b> & y | tool with a very long purpose text | second tool purpose text here |\n|---|---|---|\n| q | r | s |"
	got := ConvertMarkdown(in)
	if strings.Contains(got, "<b>x</b>") {
		t.Fatalf("compact fallback injected header markup: %s", got)
	}
	if !strings.Contains(got, "&lt;b&gt;x&lt;/b&gt; &amp; y: q") {
		t.Fatalf("header not escaped into compact text: %s", got)
	}
}

func TestSplitForTelegramKeepsEveryPartConvertible(t *testing.T) {
	var b strings.Builder
	b.WriteString("Einleitung mit **fett** und `code`.\n\n")
	b.WriteString("| Tool | Zweck | Beispiel |\n|---|---|---|\n")
	for i := 0; i < 40; i++ {
		b.WriteString("| read | Dateien, Archive, SQLite, URLs und lange Erklaerungen | read(\"x.ts:1-2\") |\n")
	}
	b.WriteString("\n```go\n")
	b.WriteString(strings.Repeat("fmt.Println(\"zeile\")\n", 120))
	b.WriteString("```\n\n")
	b.WriteString(strings.Repeat("Prosa mit A & B < C und *betontem* Text. ", 200))
	md := b.String()

	parts := SplitForTelegram(md, MaxMessageUTF16)
	if len(parts) < 2 {
		t.Fatalf("expected the message to split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		if n := convertLen(part); n > MaxMessageUTF16 {
			t.Fatalf("part %d renders to %d units, over the %d limit", i, n, MaxMessageUTF16)
		}
		if strings.Count(part, "```")%2 != 0 {
			t.Fatalf("part %d has an unbalanced code fence", i)
		}
	}
	if got := strings.Count(strings.Join(parts, "\n\n"), "read | Dateien"); got != 40 {
		t.Fatalf("table rows lost: %d of 40 survived", got)
	}
	if got := strings.Count(strings.Join(parts, "\n"), "fmt.Println"); got != 120 {
		t.Fatalf("code lines lost: %d of 120 survived", got)
	}
}

func TestSplitForTelegramRepeatsTableHeaderAcrossParts(t *testing.T) {
	var b strings.Builder
	b.WriteString("| name | note |\n|---|---|\n")
	for i := 0; i < 30; i++ {
		b.WriteString("| zeile | ")
		b.WriteString(strings.Repeat("x", 60))
		b.WriteString(" |\n")
	}
	parts := SplitForTelegram(b.String(), 400)
	if len(parts) < 2 {
		t.Fatalf("expected a split, got %d part(s)", len(parts))
	}
	for i, part := range parts {
		lines := strings.Split(part, "\n")
		if len(lines) < 3 {
			t.Fatalf("part %d lost its table shape: %q", i, part)
		}
		if lines[0] != "| name | note |" || !tableDelim.MatchString(lines[1]) {
			t.Fatalf("part %d has no repeated header: %q / %q", i, lines[0], lines[1])
		}
		rendered := ConvertMarkdown(part)
		text := html.UnescapeString(strings.TrimSuffix(strings.TrimPrefix(rendered, "<pre>"), "</pre>"))
		if n := utf16Len(text); n > 400 {
			t.Fatalf("part %d renders to %d units, over the 400 limit", i, n)
		}
	}
}

func TestSplitForTelegramAlignedTablesRepeatHeadersWithinBudget(t *testing.T) {
	const head = "|key|value|\n|-|-|"
	const row = "|row|abcdefghijkl|"
	const limit = 95
	parts := SplitForTelegram(head+"\n"+strings.Repeat(row+"\n", 19), limit)
	if len(parts) < 2 {
		t.Fatalf("expected an aligned table to split, got %d part(s)", len(parts))
	}
	rows := 0
	for i, part := range parts {
		if !strings.HasPrefix(part, head+"\n") {
			t.Fatalf("part %d lost its original table header: %q", i, part)
		}
		rendered := ConvertMarkdown(part)
		if !strings.HasPrefix(rendered, "<pre>") || !strings.HasSuffix(rendered, "</pre>") {
			t.Fatalf("part %d lost its aligned table rendering: %q", i, rendered)
		}
		text := html.UnescapeString(strings.TrimSuffix(strings.TrimPrefix(rendered, "<pre>"), "</pre>"))
		if n := utf16Len(text); n > limit {
			t.Fatalf("part %d renders to %d units, over the %d limit", i, n, limit)
		}
		rows += strings.Count(part, row)
	}
	if rows != 19 {
		t.Fatalf("table rows changed: got %d, want 19", rows)
	}
}

func TestSplitForTelegramKeepsSmallBlocksTogether(t *testing.T) {
	md := "alpha\n\nbeta `x` gamma"
	parts := SplitForTelegram(md, MaxMessageUTF16)
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want 1", len(parts))
	}
	if ConvertMarkdown(parts[0]) != ConvertMarkdown(md) {
		t.Fatalf("single part must convert identically, got %q", ConvertMarkdown(parts[0]))
	}
	if SplitForTelegram("   \n\n", MaxMessageUTF16) != nil {
		t.Fatal("blank input must yield no parts")
	}
}

func TestClipConvertibleFitsAndMarksTheCut(t *testing.T) {
	long := strings.Repeat("**x** & `y` ", 700) // 700 * 10 = 7000 units
	got := ClipConvertible(long, 1200)
	if n := convertLen(got); n > 1200 {
		t.Fatalf("clipped text renders to %d units, over the 1200 limit", n)
	}
	if !strings.HasSuffix(got, "\n…") {
		t.Fatalf("clipped text must mark the cut, got %q", got[len(got)-20:])
	}
	if short := "kurz `x`"; ClipConvertible(short, MaxMessageUTF16) != short {
		t.Fatal("short text must pass through untouched")
	}
}

func TestSplitForTelegramSplitsOversizedFenceLines(t *testing.T) {
	fits := strings.Repeat("x", MaxMessageUTF16-16)
	oversized := strings.Repeat("*a", MaxMessageUTF16) // 8192 literal code units
	md := "```\n" + fits + "\n" + oversized + "\n```\n"
	parts := SplitForTelegram(md, MaxMessageUTF16)
	if len(parts) < 2 {
		t.Fatalf("an oversized code line must split, got %d part(s)", len(parts))
	}
	joined := strings.Join(parts, "\n")
	if !strings.Contains(joined, fits) {
		t.Fatal("a code line within the limit must stay whole")
	}
	if got := strings.Count(joined, "*a"); got != MaxMessageUTF16 {
		t.Fatalf("oversized code line lost: %d of %d pairs survived", got, MaxMessageUTF16)
	}
	for i, part := range parts {
		if n := convertLen(part); n > MaxMessageUTF16 {
			t.Fatalf("part %d renders to %d units, over the %d limit", i, n, MaxMessageUTF16)
		}
		if strings.Count(part, "```")%2 != 0 {
			t.Fatalf("part %d has an unbalanced code fence", i)
		}
	}
}

func TestSplitForTelegramPreservesLiteralFenceContents(t *testing.T) {
	for _, test := range []struct {
		name, marker, body string
		limit              int
	}{
		{"backtick remainder", "```", strings.Repeat("a", 4096) + "```", 4096},
		{"tilde remainder", "~~~", strings.Repeat("a", 4096) + "~~~", 4096},
		{"trimmed remainder", "```", strings.Repeat("a", 4096) + " \t```\t ", 4096},
		{"emoji remainder", "```", strings.Repeat("😀", 2048) + "```", 4096},
		{"multiple marker lengths", "```", strings.Repeat("a", 4096) + "```\n````\n~~~", 4096},
		{"line boundaries", "```", "lead\n" + strings.Repeat("x", 96) + "\n" + strings.Repeat("<&😀*`", 60) + "\nend\n", 96},
	} {
		t.Run(test.name, func(t *testing.T) {
			md := test.marker + "\n" + test.body + "\n" + test.marker
			parts := SplitForTelegram(md, test.limit)
			if len(parts) < 2 {
				t.Fatalf("expected literal contents to split, got %d part(s)", len(parts))
			}
			if got := splitLiteralPayload(t, parts, test.limit); got != test.body {
				t.Fatalf("literal content changed: got %q, want %q", got, test.body)
			}
			if test.name == "line boundaries" {
				wholeLine := strings.Repeat("x", test.limit)
				if !strings.Contains(strings.Join(parts, "\n"), wholeLine) {
					t.Fatal("a fitting code line must not be split")
				}
			}
		})
	}
}

func TestSplitForTelegramOversizedTablesPreserveLabelledData(t *testing.T) {
	longHeader := strings.Repeat("head😀&<", 600)
	cellSource := strings.Repeat("<&😀*q*`code`", 40)
	cellText := strings.Repeat("<&😀*q*code", 40)
	for _, test := range []struct {
		name, source, want string
		limit              int
	}{
		{
			"production first row", "|key|\n|-|\n|" + strings.Repeat("a", 4092) + "|",
			"key: " + strings.Repeat("a", 4092), 4096,
		},
		{
			"production emoji row", "|key|\n|-|\n|" + strings.Repeat("😀", 2046) + "|",
			"key: " + strings.Repeat("😀", 2046), 4096,
		},
		{
			"production long header", "|`" + longHeader + "`|\n|-|\n|tail & < *keep*|",
			longHeader + ": tail & < *keep*", 4096,
		},
		{
			"first page multi row and column", "|`Key & <`|note|\n|-|-|\n|" + cellSource + "|*one*|\n|last & <|`two`|",
			"Key & <: " + cellText + " · note: *one*\nKey & <: last & < · note: two", 73,
		},
		{
			"first page long header", "|" + longHeader + "|next|\n|-|-|\n|a|b|\n|c|d|",
			longHeader + ": a · next: b\n" + longHeader + ": c · next: d", 257,
		},
		{
			"aligned emoji table", "|" + strings.Repeat("😀", 12) + "|\n|-|\n|" + strings.Repeat("😀", 12) + "|",
			strings.Repeat("😀", 12) + ": " + strings.Repeat("😀", 12), 48,
		},
		{
			"additional column labels", "|key||\n|-|-|\n|" + strings.Repeat("a", 90) + "|& <|`third`|\n|last||end|",
			"key: " + strings.Repeat("a", 90) + " · col2: & < · col3: third\nkey: last · col2:  · col3: end", 64,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parts := SplitForTelegram(test.source, test.limit)
			if len(parts) < 2 {
				t.Fatalf("expected labelled data to split, got %d part(s)", len(parts))
			}
			if got := splitLiteralPayload(t, parts, test.limit); got != test.want {
				t.Fatalf("table data changed: got %q, want %q", got, test.want)
			}
		})
	}
}

func splitLiteralPayload(t *testing.T, parts []string, limit int) string {
	t.Helper()
	var payload strings.Builder
	for i, part := range parts {
		rendered := ConvertMarkdown(part)
		if !strings.HasPrefix(rendered, "<pre>") || !strings.HasSuffix(rendered, "</pre>") {
			t.Fatalf("part %d is not literal text: %q", i, rendered)
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(rendered, "<pre>"), "</pre>")
		if strings.Contains(inner, "<") {
			t.Fatalf("part %d contains interpreted markup: %q", i, rendered)
		}
		text := html.UnescapeString(inner)
		if n := utf16Len(text); n > limit {
			t.Fatalf("part %d renders to %d units, over the %d limit", i, n, limit)
		}
		payload.WriteString(text)
	}
	return payload.String()
}

func TestTailForTelegramKeepsRecentTextAndCodeContext(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		limit              int
	}{
		{"emoji", "discarded prefix😀keep", "<pre>😀keep</pre>", 14},
		{"whole line", "discarded prefix\n**current**\nlast", "<b>current</b>\nlast", 20},
		{"tilde fence", "intro\n~~~go\nold\nline\n~~~\nend", "<pre>line</pre>\nend", 16},
		{"open fence", "intro\n```\nold\n**literal**", "<pre>**literal**</pre>", 15},
		{"literal markers", "discarded prefix```~~~**&😀", "<pre>```~~~**&amp;😀</pre>", 21},
	} {
		t.Run(test.name, func(t *testing.T) {
			tail := TailForTelegram(test.source, test.limit)
			html := ConvertMarkdown(tail)
			if html != test.want || utf16Len(tail) > test.limit || renderedUTF16Len(html) > test.limit {
				t.Fatalf("tail lost text, context or UTF-16 bound: %q, want %q", html, test.want)
			}
		})
	}
}

func TestTailForTelegramPendingCodeAtBudgetDoesNotInventText(t *testing.T) {
	for _, source := range []string{"`" + strings.Repeat("x", 31), "`" + strings.Repeat("😀", 15) + "x"} {
		tail := TailForTelegram(source, 32)
		rendered := ConvertMarkdown(tail)
		if utf16Len(tail) > 32 || renderedUTF16Len(rendered) > 32 || strings.Contains(rendered, "`") {
			t.Fatalf("bounded preview exposed a synthetic closing delimiter: %q", rendered)
		}
	}
}

func TestTailForTelegramRespectsQuoteFenceScopes(t *testing.T) {
	for _, test := range []struct {
		name, source, wantTail, wantHTML string
	}{
		{
			"top-level code keeps quote-like closing fence and emphasis",
			"```\n> ```\n**pending",
			"```\n> ```\n**pending",
			"<pre>&gt; ```\n**pending</pre>",
		},
		{
			"top-level code keeps quote-like closing fence and inline code",
			"```\n> ```\n`pending",
			"```\n> ```\n`pending",
			"<pre>&gt; ```\n`pending</pre>",
		},
		{
			"top-level code keeps nested quote-like closing fence",
			"```\n> > ```\n**pending",
			"```\n> > ```\n**pending",
			"<pre>&gt; &gt; ```\n**pending</pre>",
		},
		{
			"blank line ends quoted fence",
			"> ```\n> literal\n\n**pending",
			"> ```\n> literal\n\npending",
			"<blockquote><pre>literal</pre></blockquote>\n\npending",
		},
		{
			"ordinary line ends quoted fence",
			"> ```\n> literal\n`pending",
			"> ```\n> literal\n`pending`",
			"<blockquote><pre>literal</pre></blockquote>\n<code>pending</code>",
		},
		{
			"quoted fence keeps emphasis literal",
			"> ```\n> **pending",
			"> ```\n> **pending",
			"<blockquote><pre>**pending</pre></blockquote>",
		},
		{
			"quoted code keeps deeper quote-like closing fence",
			"> ```\n> > ```\n> `pending",
			"> ```\n> > ```\n> `pending",
			"<blockquote><pre>&gt; ```\n`pending</pre></blockquote>",
		},
		{
			"nested quoted fence keeps emphasis literal",
			" > > ```\n > > **pending",
			" > > ```\n > > **pending",
			"<blockquote><blockquote><pre>**pending</pre></blockquote></blockquote>",
		},
		{
			"outer quote continues after nested fence scope ends",
			"> > ```\n> > literal\n> **pending",
			"> > ```\n> > literal\n> pending",
			"<blockquote><blockquote><pre>literal</pre></blockquote>\npending</blockquote>",
		},
		{
			"ordinary line ends nested fence scope",
			"> > ```\n> > literal\n**pending",
			"> > ```\n> > literal\npending",
			"<blockquote><blockquote><pre>literal</pre></blockquote></blockquote>\npending",
		},
		{
			"nested quoted fence closes normally",
			"> > ```\n> > literal\n> > ```\n> > **pending",
			"> > ```\n> > literal\n> > ```\n> > pending",
			"<blockquote><blockquote><pre>literal</pre>\npending</blockquote></blockquote>",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const limit = 128
			tail := TailForTelegram(test.source, limit)
			if tail != test.wantTail {
				t.Fatalf("preview changed the wrong block context: got %q, want %q", tail, test.wantTail)
			}
			if got := ConvertMarkdown(tail); got != test.wantHTML {
				t.Fatalf("preview rendered incorrectly: got %q, want %q", got, test.wantHTML)
			}
			if got := utf16Len(tail); got > limit {
				t.Fatalf("preview source uses %d UTF-16 units, over the %d limit", got, limit)
			}
		})
	}
}

func TestTailForTelegramProjectsOnlyTheFinalInlineScope(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{
			"table body keeps literal single emphasis marker",
			"|a|\n|-|\n|*literal|",
			"<pre>a\n--------\n*literal</pre>",
		},
		{
			"table code marker cannot open code in the next paragraph",
			"|h|\n|-|\n|`x|\n**z**",
			"<pre>h\n-\nx</pre>\n<b>z</b>",
		},
		{
			"ordinary paragraph after a table still projects unfinished emphasis",
			"|h|\n|-|\n|`x|\n**pending",
			"<pre>h\n-\nx</pre>\npending",
		},
		{
			"quoted table is not an inline paragraph",
			"> |a|\n> |-|\n> |*literal|",
			"<blockquote><pre>a\n--------\n*literal</pre></blockquote>",
		},
		{
			"quoted multiline code closes in its complete paragraph",
			"> `a\n> b`c",
			"<blockquote><code>a\nb</code>c</blockquote>",
		},
		{
			"nested quoted multiline code keeps original prefix widths",
			" > > `a\n\t>   > b`c",
			"<blockquote><blockquote><code>a\nb</code>c</blockquote></blockquote>",
		},
		{
			"nested quoted paragraph projects only its pending opener",
			"> > **a\n> > b** **pending",
			"<blockquote><blockquote><b>a\nb</b> pending</blockquote></blockquote>",
		},
		{
			"list continuation ends before the final paragraph",
			"- item\n  **literal\n**done**",
			"- item\n  **literal\n<b>done</b>",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const limit = 4096
			tail := TailForTelegram(test.source, limit)
			if got := ConvertMarkdown(tail); got != test.want {
				t.Fatalf("preview changed a block's visible text or formatting: got %q, want %q", got, test.want)
			}
			if utf16Len(tail) > limit {
				t.Fatalf("preview source exceeds its UTF-16 budget: %q", tail)
			}
		})
	}
}

func TestTailForTelegramRestoresClippedBlockScopes(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		limit              int
	}{
		{
			"open quoted fence at an exact source line boundary",
			"prefix\n> ```\n> old\n> **literal",
			"<blockquote><pre>**literal</pre></blockquote>",
			17,
		},
		{
			"closed quoted fence at an exact source line boundary",
			"> ```\n> old\n> **literal\n> ```",
			"<blockquote><pre>**literal</pre></blockquote>",
			23,
		},
		{
			"quoted code does not reinterpret deeper quote-like fence text",
			"prefix\n> ```\n> old\n> > ```\n> **literal",
			"<blockquote><pre>&gt; ```\n**literal</pre></blockquote>",
			25,
		},
		{
			"nested quoted fence restores both quote scopes",
			"> > ~~~\n> > old\n> > **literal",
			"<blockquote><blockquote><pre>**literal</pre></blockquote></blockquote>",
			21,
		},
		{
			"removed closing fence does not become an opener",
			"> ```\n> old\n> ```\n> **pending",
			"<blockquote>pending</blockquote>",
			18,
		},
		{
			"nested fence scope ends before retained outer quote text",
			"> > ```\n> > old\n> **pending",
			"<blockquote>pending</blockquote>",
			12,
		},
		{
			"quoted multiline inline code survives a real source cut",
			"discarded prefix\n> `a\n> b`c",
			"<blockquote><code>a\nb</code>c</blockquote>",
			10,
		},
		{
			"clipped list continuation does not leak into the next paragraph",
			"- item\n  **literal\n**done**",
			"<b>done</b>",
			20,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if utf16Len(test.source) <= test.limit {
				t.Fatal("regression must exercise a real source cut")
			}
			tail := TailForTelegram(test.source, test.limit)
			got := ConvertMarkdown(tail)
			if got != test.want {
				t.Fatalf("clipped preview lost its original block scope: got %q, want %q", got, test.want)
			}
			if utf16Len(tail) > test.limit || renderedUTF16Len(got) > test.limit {
				t.Fatalf("clipped preview exceeds its UTF-16 budget: %q", tail)
			}
		})
	}
}

func TestTailForTelegramQuotedCodeFallbackUsesOnlyOriginalPayload(t *testing.T) {
	for _, test := range []struct {
		name, source, payload string
		limit                 int
	}{
		{"minimal open fence", "> ```\n> old\n> **literal", "old\n**literal", 11},
		{"production source boundary", "> ```\n> old\n> " + strings.Repeat("x", 2188) + " **literal", "old\n" + strings.Repeat("x", 2188) + " **literal", 2200},
		{"nested emoji payload", "> > ```\n> > old\n> > " + strings.Repeat("😀", 1093) + " **literal", "old\n" + strings.Repeat("😀", 1093) + " **literal", 2200},
		{"closed fence only fits its closer", "> ```\n> **literal\n> ```", "**literal", 11},
		{"closed fence with trailing newline", "> ```\n> **literal\n> ```\n", "**literal\n", 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			tail := TailForTelegram(test.source, test.limit)
			got := ConvertMarkdown(tail)
			visible := plainHTMLText(got)
			if visible == "" || !strings.HasSuffix(test.payload, visible) {
				t.Fatalf("fallback exposed source wrappers instead of original code: %q", got)
			}
			if !strings.HasPrefix(got, "<pre>") || !strings.HasSuffix(got, "</pre>") {
				t.Fatalf("fallback reinterpreted literal code as inline formatting: %q", got)
			}
			if test.limit == 2200 && !strings.HasSuffix(visible, " **literal") {
				t.Fatalf("fallback deleted literal code emphasis markers: %q", visible)
			}
			if utf16Len(tail) > test.limit || renderedUTF16Len(got) > test.limit || !utf8.ValidString(tail) {
				t.Fatalf("fallback exceeds its UTF-16 budget or splits a rune: %q", tail)
			}
		})
	}
}

func TestTailForTelegramTableContextFallbackKeepsBoundedOriginalRows(t *testing.T) {
	for _, test := range []struct {
		name, header, rows string
		limit              int
	}{
		{"aligned table expansion", "|long|\n|-|\n", "|one|\n|*literal|", 21},
		{"oversized compact table header", "|" + strings.Repeat("header", 4096) + "|\n|-|\n", strings.Repeat("|row|\n", 512) + "|*literal|", 2200},
	} {
		for _, quote := range []string{"", "> > "} {
			t.Run(test.name+quote, func(t *testing.T) {
				source := test.header + test.rows
				if quote != "" {
					source = quote + strings.ReplaceAll(source, "\n", "\n"+quote)
				}
				tail := TailForTelegram(source, test.limit)
				got := ConvertMarkdown(tail)
				visible := plainHTMLText(got)
				if !strings.HasSuffix(visible, "|*literal|") || !strings.HasSuffix(test.rows, visible) {
					t.Fatalf("table fallback expanded labels or leaked enclosing quote syntax: %q", got)
				}
				if !strings.HasPrefix(got, "<pre>") || !strings.HasSuffix(got, "</pre>") {
					t.Fatalf("table fallback reinterpreted the original literal rows: %q", got)
				}
				if utf16Len(tail) > test.limit || renderedUTF16Len(got) > test.limit || !utf8.ValidString(tail) {
					t.Fatalf("table fallback exceeded its source budget or split a rune: %q", tail)
				}
			})
		}
	}
}

func TestTailForTelegramClippedListContinuationDoesNotJoinInlineScopes(t *testing.T) {
	const source = "- item\n  `a\n  b`c"
	const limit = 10
	tail := TailForTelegram(source, limit)
	got := ConvertMarkdown(tail)
	visible := plainHTMLText(got)
	if visible == "" || !strings.HasSuffix(plainHTMLText(ConvertMarkdown(source)), visible) {
		t.Fatalf("clipped list continuation changed its literal text: %q", got)
	}
	if strings.Contains(got, "<code>") || utf16Len(tail) > limit || renderedUTF16Len(got) > limit {
		t.Fatalf("clipped list continuation paired separate inline scopes or exceeded its budget: %q", got)
	}
}

func TestTailForTelegramDoesNotExposeProjectedFenceSyntax(t *testing.T) {
	const payload = "x ```a\n`"
	for _, quote := range []string{"", "> ", "> > "} {
		t.Run(quote, func(t *testing.T) {
			source := quote + strings.ReplaceAll(payload, "\n", "\n"+quote)
			const limit = 128
			tail := TailForTelegram(source, limit)
			got := ConvertMarkdown(tail)
			if visible := plainHTMLText(got); visible != payload {
				t.Fatalf("inline projection exposed a new fence or invented closing text: got %q, want %q", got, payload)
			}
			if !strings.HasPrefix(got, "<pre>") || !strings.HasSuffix(got, "</pre>") {
				t.Fatalf("unsafe inline projection did not preserve the original literal payload: %q", got)
			}
			if utf16Len(tail) > limit || renderedUTF16Len(got) > limit || !utf8.ValidString(tail) {
				t.Fatalf("safe projection fallback exceeded its budget or split a rune: %q", tail)
			}
		})
	}
}

func TestTailForTelegramLongListContinuationsKeepRecentContent(t *testing.T) {
	const limit = 2200
	continuations := strings.Repeat(" x\n", 100000)
	for _, suffix := range []string{"", "**recent**"} {
		t.Run(suffix, func(t *testing.T) {
			source := "- i\n" + continuations + suffix
			tail := TailForTelegram(source, limit)
			got := ConvertMarkdown(tail)
			if suffix == "" {
				visible := plainHTMLText(got)
				if !strings.Contains(visible, "x") || !strings.HasSuffix(continuations, visible) {
					t.Fatalf("clipped list lost its recent literal continuation text: %q", got)
				}
			} else if got != "<b>recent</b>" {
				t.Fatalf("clipped list swallowed the following independent paragraph: %q", got)
			}
			if utf16Len(tail) > limit || renderedUTF16Len(got) > limit || !utf8.ValidString(tail) {
				t.Fatalf("long-list preview exceeded its budget or split a rune: %q", tail)
			}
		})
	}
}

func BenchmarkTailForTelegramLongListContinuations(b *testing.B) {
	for _, size := range []struct {
		name  string
		lines int
	}{
		{"10k", 10000},
		{"100k", 100000},
	} {
		b.Run(size.name, func(b *testing.B) {
			source := "- i\n" + strings.Repeat(" x\n", size.lines)
			b.ReportAllocs()
			b.SetBytes(int64(len(source)))
			b.ResetTimer()
			for range b.N {
				TailForTelegram(source, 2200)
			}
		})
	}
}

type renderedInlineGlyph struct {
	char                       rune
	bold, italic, strike, code bool
	href                       string
}

// Read generated HTML rather than inspecting synthetic Markdown delimiters.
// Whitespace emphasis is intentionally not part of the visual contract.
func renderedInlineGlyphs(t *testing.T, rendered string) []renderedInlineGlyph {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader("<root>" + rendered + "</root>"))
	var glyphs []renderedInlineGlyph
	var active []xml.StartElement
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid rendered HTML %q: %v", rendered, err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			active = append(active, token)
		case xml.EndElement:
			active = active[:len(active)-1]
		case xml.CharData:
			for _, char := range string(token) {
				glyph := renderedInlineGlyph{char: char}
				if !unicode.IsSpace(char) {
					for _, tag := range active {
						switch tag.Name.Local {
						case "b":
							glyph.bold = true
						case "i":
							glyph.italic = true
						case "s":
							glyph.strike = true
						case "code":
							glyph.code = true
						case "a":
							for _, attr := range tag.Attr {
								if attr.Name.Local == "href" {
									glyph.href = attr.Value
								}
							}
						}
					}
				}
				glyphs = append(glyphs, glyph)
			}
		}
	}
	return glyphs
}

func assertRichSplitEquivalent(t *testing.T, source string, limit int) []string {
	t.Helper()
	parts := SplitForTelegram(source, limit)
	if len(parts) < 2 {
		t.Fatalf("expected an oversized paragraph to split, got %d parts", len(parts))
	}
	want := renderedInlineGlyphs(t, ConvertMarkdown(source))
	var got []renderedInlineGlyph
	for i, part := range parts {
		if !utf8.ValidString(part) {
			t.Fatalf("part %d split a UTF-8 character", i)
		}
		rendered := ConvertMarkdown(part)
		if units := renderedUTF16Len(rendered); units > limit {
			t.Fatalf("part %d has %d visible UTF-16 units, limit %d", i, units, limit)
		}
		got = append(got, renderedInlineGlyphs(t, rendered)...)
	}
	if len(got) != len(want) {
		t.Fatalf("visible text length changed: got %d glyphs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("glyph %d changed: got %+v, want %+v", i, got[i], want[i])
		}
	}
	return parts
}

func TestSplitForTelegramPreservesProductionLimitBold(t *testing.T) {
	parts := assertRichSplitEquivalent(t, "**"+strings.Repeat("x", 5000)+"**", MaxMessageUTF16)
	if len(parts) != 2 {
		t.Fatalf("5000 visible ASCII units require two messages, got %d", len(parts))
	}
}

func TestSplitForTelegramKeepsFittingParagraphLinesWhole(t *testing.T) {
	for _, tc := range []struct {
		name, sourceLine, visibleLine string
		lines, limit                  int
	}{
		{"reserved first page", "body line", "body line", 600, MaxMessageUTF16 - 32},
		{"multiple entity spans", "**body** *line* [link](https://example.com) `code` 😀", "body line link code 😀", 120, 67},
		{"cross-line emphasis", "**body\nline**", "", 120, 67},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := make([]string, tc.lines)
			for i := range lines {
				lines[i] = tc.sourceLine
			}
			parts := assertRichSplitEquivalent(t, strings.Join(lines, "\n"), tc.limit)
			count := 0
			for _, part := range parts {
				var text strings.Builder
				for _, glyph := range renderedInlineGlyphs(t, ConvertMarkdown(part)) {
					text.WriteRune(glyph.char)
				}
				for _, line := range strings.Split(text.String(), "\n") {
					if line == "" {
						continue
					}
					if tc.visibleLine == "" {
						if line != "body" && line != "line" {
							t.Fatalf("a fitting emphasized line was cut: %q", line)
						}
					} else if line != tc.visibleLine {
						t.Fatalf("a fitting paragraph line was cut: %q, want %q", line, tc.visibleLine)
					}
					count++
				}
			}
			want := tc.lines
			if tc.visibleLine == "" {
				want *= 2
			}
			if count != want {
				t.Fatalf("fitting lines lost or duplicated: got %d, want %d", count, want)
			}
		})
	}
}

func TestSplitForTelegramPreservesRichParagraphEntities(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		limit        int
	}{
		{"nested emoji", "***" + strings.Repeat("😀<& word ", 100) + "***", 97},
		{"code interrupts emphasis", "**before " + "`" + strings.Repeat("😀 ** _ \\ ", 60) + "`" + " after *nested* end**", 96},
		{"long link label", "**[" + strings.Repeat("😀 **bold** \\*literal\\* ", 80) + "](https://example.com/a\\*b?x=1&y=2)**", 101},
		{"long image label", "![" + strings.Repeat("**image😀** `code` ", 60) + "](https://example.com/image)", 99},
		{"long code delimiter beside styles", "***before ```` " + strings.Repeat("a` b`` c``` 😀 ", 30) + " ```` after***", 91},
		{"adjacent multiline code labels", "[``" + strings.Repeat("a", 80) + "\n ``](https://example.com/a)[`` \n" + strings.Repeat("b", 80) + "``](https://example.com/b)", 64},
		{"long whitespace boundary", "**" + strings.Repeat("x", 63) + strings.Repeat(" ", 150) + "tail**", 64},
		{"soft newlines", "**" + strings.Repeat("first\nsecond ", 50) + "last**", 95},
		{"mixed nested transitions", "*first **" + strings.Repeat("inner ", 30) + "** tail* ~~" + strings.Repeat("gone ", 30) + "~~", 67},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertRichSplitEquivalent(t, tc.source, tc.limit)
		})
	}
}

func TestSplitForTelegramPreservesInlineCodePayloads(t *testing.T) {
	for _, body := range []string{
		"`" + strings.Repeat("😀 a` ** \\ ", 40) + "`",
		strings.Repeat(" ", 200),
		"  " + strings.Repeat("body😀 ", 40) + "  ",
		strings.Repeat("` `` ``` 😀 ", 40),
	} {
		marker := "````"
		padded := body
		if strings.Trim(body, " ") != "" {
			padded = " " + body + " "
		}
		source := "**" + marker + padded + marker + "**"
		parts := assertRichSplitEquivalent(t, source, 65)
		for i, part := range parts {
			rendered := ConvertMarkdown(part)
			if !strings.Contains(rendered, "<code>") || strings.Contains(rendered, "<pre>") {
				t.Fatalf("part %d changed inline code semantics: %q", i, rendered)
			}
		}
	}
}

func TestSplitForTelegramDoesNotExposeNewBlockSyntax(t *testing.T) {
	for _, suffix := range []string{"# title", "> quote", "- item", "1. item", "```literal", "~~~literal", "***", "|a|"} {
		source := strings.Repeat("x", 64) + suffix
		assertRichSplitEquivalent(t, source, 64)
	}
}

func TestTailForTelegramKeepsFollowingCompleteListItems(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
		limit              int
		contains           bool
	}{
		{"small boundary", "- a\n x\n- b\nz", "- b\nz", 8, false},
		{"production boundary", "- a\n" + strings.Repeat(" x\n", 1000) + "- b\nz", "- b\nz", 2200, false},
		{"quoted later item", "> - a\n" + strings.Repeat(">  x\n", 1000) + "> - b\n> z", "- b\nz", 2200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tail := TailForTelegram(test.source, test.limit)
			rendered := ConvertMarkdown(tail)
			matches := rendered == test.want
			if test.contains {
				matches = strings.Contains(rendered, test.want)
			}
			if !matches || literalUTF16Len(tail) > test.limit {
				t.Fatalf("complete following list item was discarded: source tail=%q HTML=%q want=%q", tail, rendered, test.want)
			}
		})
	}
}
