package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type inlineKind uint8

const (
	inlineText inlineKind = iota
	inlineEscape
	inlineDelimiter
	inlineCode
	inlineLink
)

type inlineMatch struct {
	open  int
	width int
}

type inlineToken struct {
	kind        inlineKind
	raw         string
	body        string
	href        string
	label       []inlineToken
	image       bool
	closed      bool
	marker      byte
	remaining   int
	canOpen     bool
	canClose    bool
	previewOpen bool
	opens       []int
	closes      []inlineMatch
}

// convertInline parses source before escaping it. Code and links are opaque to
// delimiter matching, so neither source text nor URLs can impersonate tokens.
func convertInline(s string) string {
	if !strings.ContainsAny(s, "\\`[*_~") {
		return escapeHTML(s)
	}
	var r inlineRenderer
	r.render(parseInline(s, true))
	r.sync(nil)
	return r.out.String()
}

// convertHeadingInline applies the heading's bold style only to non-code text.
func convertHeadingInline(s string) string {
	var r inlineRenderer
	r.active = append(r.active, inlineTag{name: "b"})
	r.render(parseInline(s, true))
	r.sync(nil)
	return r.out.String()
}

func parseInline(s string, links bool) []inlineToken {
	var tokens []inlineToken
	plain := 0
	var linkMatches [][6]int
	if links {
		linkMatches = inlineLinks(s)
	}
	linkIndex := 0
	flush := func(end int) {
		if plain < end {
			tokens = append(tokens, inlineToken{kind: inlineText, raw: s[plain:end]})
		}
	}
	for i := 0; i < len(s); {
		for linkIndex < len(linkMatches) && linkMatches[linkIndex][0] < i {
			linkIndex++
		}
		if s[i] == '\\' && i+1 < len(s) && inlinePunctuation(s[i+1]) {
			flush(i)
			tokens = append(tokens, inlineToken{kind: inlineEscape, raw: s[i : i+2], body: s[i+1 : i+2]})
			i += 2
			plain = i
			continue
		}
		if s[i] == '`' {
			flush(i)
			width := inlineRun(s, i)
			end := inlineCodeEnd(s, i+width, width)
			if end < 0 {
				tokens = append(tokens, inlineToken{kind: inlineCode, raw: s[i:], body: s[i+width:], previewOpen: !inlineEscaped(s, i)})
				i = len(s)
			} else {
				body := s[i+width : end]
				if len(body) >= 2 && body[0] == ' ' && body[len(body)-1] == ' ' && strings.Trim(body, " ") != "" {
					body = body[1 : len(body)-1]
				}
				tokens = append(tokens, inlineToken{kind: inlineCode, raw: s[i : end+width], body: body, closed: true})
				i = end + width
			}
			plain = i
			continue
		}
		if linkIndex < len(linkMatches) && linkMatches[linkIndex][0] == i {
			match := linkMatches[linkIndex]
			flush(i)
			end := match[1]
			raw := s[i:end]
			tokens = append(tokens, inlineToken{
				kind: inlineLink, raw: raw, href: s[match[4]:match[5]],
				label: parseInline(s[match[2]:match[3]], false), image: raw[0] == '!',
			})
			i = end
			plain = i
			continue
		}
		if s[i] == '*' || s[i] == '_' || s[i] == '~' {
			width := inlineRun(s, i)
			if s[i] != '~' || width >= 2 {
				flush(i)
				open, close, previewOpen := inlineFlanking(s, i, width)
				if len(tokens) > 0 && tokens[len(tokens)-1].kind == inlineEscape && strings.HasSuffix(tokens[len(tokens)-1].body, s[i:i+1]) {
					previewOpen = false
				}
				tokens = append(tokens, inlineToken{
					kind: inlineDelimiter, raw: s[i : i+width], marker: s[i], remaining: width,
					canOpen: open, canClose: close, previewOpen: previewOpen,
				})
				i += width
				plain = i
				continue
			}
			i += width
			continue
		}
		i++
	}
	flush(len(s))
	matchInlineDelimiters(tokens)
	return tokens
}

// inlineLinks indexes every eligible opener once, including overlapping labels.
// The parser then skips candidates consumed by code or an earlier opaque link.
// This avoids restarting a suffix search after each escaped bracket or code.
func inlineLinks(s string) [][6]int {
	if !strings.Contains(s, "[") {
		return nil
	}
	destinations := inlineLinkDestinations(s)
	var matches [][6]int
	var pending []int
	for i := 0; i < len(s); {
		if s[i] == '\\' && i+1 < len(s) {
			if s[i+1] == '\n' {
				// The existing label grammar's escaped character excludes LF.
				pending = pending[:0]
			} else if s[i+1] == '!' && i+2 < len(s) && s[i+2] == '[' {
				// An escaped image opener must not become a plain link.
				i++
			}
			i += 2
			continue
		}
		if s[i] == '[' {
			pending = append(pending, i)
		}
		if s[i] == ']' {
			if destination, ok := destinations[i]; ok {
				for _, open := range pending {
					start := open
					if start > 0 && s[start-1] == '!' {
						start--
					}
					matches = append(matches, [6]int{start, destination[0], open + 1, i, destination[1], destination[2]})
				}
			}
			pending = pending[:0]
		}
		i++
	}
	return matches
}

// Resolve the existing destination and optional quoted-title grammar backwards.
// Constant-size suffix states make every ]( lookup O(1), even when a candidate
// inside code overlaps many later openers or destinations contain brackets.
func inlineLinkDestinations(s string) map[int][3]int {
	var destinations map[int][3]int
	closeEnd, spaceTail := 0, 0
	doubleCloseEnd, singleCloseEnd := 0, 0
	urlEnd, urlFinish := len(s), 0
	hrefStart, hrefEnd, end := 0, 0, 0
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c == '(' && i > 0 && s[i-1] == ']' && end > 0 {
			if destinations == nil {
				destinations = make(map[int][3]int)
			}
			destinations[i-1] = [3]int{end, hrefStart, hrefEnd}
		}
		titleEnd := 0
		switch c {
		case '"':
			titleEnd, doubleCloseEnd = doubleCloseEnd, closeEnd
		case '\'':
			titleEnd, singleCloseEnd = singleCloseEnd, closeEnd
		}
		space := inlineLinkSpace(c)
		if !space {
			switch c {
			case ')':
				closeEnd, spaceTail = i+1, i+1
			case '"', '\'':
				closeEnd, spaceTail = 0, titleEnd
			default:
				closeEnd, spaceTail = 0, 0
			}
		}
		if space || c == ')' {
			urlEnd, urlFinish = i, spaceTail
		}
		if !space {
			if c == ')' {
				end = 0
			} else {
				hrefStart, hrefEnd, end = i, urlEnd, urlFinish
			}
		}
	}
	return destinations
}

func inlineLinkSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

// Markdown escapes only ASCII punctuation, never code bodies or destinations.
func inlinePunctuation(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func inlineRun(s string, start int) int {
	end := start + 1
	for end < len(s) && s[end] == s[start] {
		end++
	}
	return end - start
}

func inlineCodeEnd(s string, start, width int) int {
	for start < len(s) {
		next := strings.IndexByte(s[start:], '`')
		if next < 0 {
			break
		}
		start += next
		run := inlineRun(s, start)
		if run == width {
			return start
		}
		start += run
	}
	return -1
}

func inlineFlanking(s string, start, width int) (open, close, previewOpen bool) {
	var before, after rune
	if start > 0 {
		before, _ = utf8.DecodeLastRuneInString(s[:start])
	}
	if start+width < len(s) {
		after, _ = utf8.DecodeRuneInString(s[start+width:])
	}
	beforeSpace := start == 0 || unicode.IsSpace(before)
	afterSpace := start+width == len(s) || unicode.IsSpace(after)
	beforePunct := unicode.IsPunct(before) || unicode.IsSymbol(before)
	afterPunct := unicode.IsPunct(after) || unicode.IsSymbol(after)
	left := !afterSpace && (!afterPunct || beforeSpace || beforePunct)
	right := !beforeSpace && (!beforePunct || afterSpace || afterPunct)
	open, close = left, right
	if s[start] == '_' && width == 1 {
		open = left && (!right || beforePunct)
		close = right && (!left || afterPunct)
	}
	// An incomplete delimiter inside a word may just be multiplication or an
	// identifier. Only suppress incomplete runs at a plausible opening boundary.
	previewOpen = open && (beforeSpace || beforePunct) && !afterPunct && !inlineEscaped(s, start)
	return
}

func inlineEscaped(s string, start int) bool {
	escaped := false
	for i := start - 1; i >= 0 && s[i] == '\\'; i-- {
		escaped = !escaped
	}
	return escaped
}

// Matches consume the right edge of opener runs and the left edge of closer
// runs. Keeping each run intact handles both ***x*** and **a *b*** naturally.
func matchInlineDelimiters(tokens []inlineToken) {
	var stack []int
	var floors [3][2][3]int
	for i := range tokens {
		current := &tokens[i]
		if current.kind != inlineDelimiter {
			continue
		}
		if current.canClose {
			marker := 0
			switch current.marker {
			case '_':
				marker = 1
			case '~':
				marker = 2
			}
			canOpen := 0
			if current.canOpen {
				canOpen = 1
			}
			// Matching eligibility uses immutable raw lengths, so a failed
			// search stays valid for this closer class as runs are consumed.
			floor := &floors[marker][canOpen][len(current.raw)%3]
			for current.remaining > 0 {
				if current.marker == '~' && current.remaining < 2 {
					break
				}
				found := -1
				for j := len(stack) - 1; j >= 0 && stack[j] >= *floor; j-- {
					opener := &tokens[stack[j]]
					if opener.marker != current.marker || opener.remaining == 0 {
						continue
					}
					if current.marker != '~' && (opener.canClose || current.canOpen) &&
						(len(opener.raw)+len(current.raw))%3 == 0 &&
						(len(opener.raw)%3 != 0 || len(current.raw)%3 != 0) {
						continue
					}
					found = j
					break
				}
				if found < 0 {
					// Use token indices, not stack offsets: truncating the stack
					// must not hide new openers, including this token itself.
					*floor = i
					break
				}
				index := stack[found]
				opener := &tokens[index]
				width := 1
				if opener.remaining >= 2 && current.remaining >= 2 {
					width = 2
				}
				opener.opens = append(opener.opens, width)
				current.closes = append(current.closes, inlineMatch{open: index, width: width})
				opener.remaining -= width
				current.remaining -= width
				// Unmatched openers above this match stay literal rather than
				// becoming crossing HTML tags.
				stack = stack[:found+1]
				if opener.remaining == 0 || opener.marker == '~' && opener.remaining < 2 {
					stack = stack[:found]
				}
			}
		}
		if current.canOpen && current.remaining > 0 && (current.marker != '~' || current.remaining >= 2) {
			stack = append(stack, i)
		}
	}
}

type inlineTag struct {
	name string
	href string
}

type inlineRenderer struct {
	out     strings.Builder
	active  []inlineTag
	emitted []inlineTag
	spans   *[]inlineSpan
}

func (r *inlineRenderer) sync(want []inlineTag) {
	common := 0
	for common < len(want) && common < len(r.emitted) && want[common] == r.emitted[common] {
		common++
	}
	for i := len(r.emitted) - 1; i >= common; i-- {
		r.out.WriteString("</" + r.emitted[i].name + ">")
	}
	for _, tag := range want[common:] {
		if tag.name == "a" {
			r.out.WriteString(`<a href="`)
			r.out.WriteString(strings.ReplaceAll(escapeHTML(tag.href), `"`, "&quot;"))
			r.out.WriteString(`">`)
		} else {
			r.out.WriteString("<" + tag.name + ">")
		}
	}
	r.emitted = append(r.emitted[:0], want...)
}

func (r *inlineRenderer) text(s string) {
	if s == "" {
		return
	}
	if r.spans != nil {
		appendInlineSpan(r.spans, inlineSpan{body: s, tags: r.active})
		return
	}
	r.sync(r.active)
	r.out.WriteString(escapeHTML(s))
}

func (r *inlineRenderer) render(tokens []inlineToken) {
	for _, token := range tokens {
		switch token.kind {
		case inlineText:
			r.text(token.raw)
		case inlineEscape:
			r.text(token.body)
		case inlineDelimiter:
			for range token.closes {
				r.active = r.active[:len(r.active)-1]
			}
			r.text(token.raw[:token.remaining])
			for i := len(token.opens) - 1; i >= 0; i-- {
				tag := "i"
				if token.opens[i] == 2 {
					tag = "b"
				}
				if token.marker == '~' {
					tag = "s"
				}
				r.active = append(r.active, inlineTag{name: tag})
			}
		case inlineCode:
			if !token.closed {
				r.text(token.raw)
				continue
			}
			if r.spans != nil {
				appendInlineSpan(r.spans, inlineSpan{body: token.body, code: true})
				continue
			}
			// Telegram forbids code combined with other entities, including
			// links. Resume the desired styles only when normal text follows.
			r.sync(nil)
			r.out.WriteString("<code>")
			r.out.WriteString(escapeHTML(token.body))
			r.out.WriteString("</code>")
		case inlineLink:
			r.active = append(r.active, inlineTag{name: "a", href: token.href})
			if token.image {
				r.text("🖼")
				if len(token.label) > 0 {
					r.text(" ")
				}
			}
			r.render(token.label)
			r.active = r.active[:len(r.active)-1]
		}
	}
}

// previewInline returns Markdown source, never HTML. Complete formatting is
// preserved; only evidenced unfinished openers are hidden in this projection.
// Final conversion always parses the untouched original source instead.
func previewInline(line string) string {
	if !strings.ContainsAny(line, "`*_~") {
		return line
	}
	tokens := parseInline(line, true)
	pending := false
	for _, token := range tokens {
		if token.kind == inlineDelimiter && token.remaining > 0 && token.previewOpen ||
			token.kind == inlineCode && !token.closed && token.body != "" && token.previewOpen {
			pending = true
			break
		}
	}
	if !pending {
		return line
	}
	var out strings.Builder
	for _, token := range tokens {
		switch token.kind {
		case inlineDelimiter:
			for _, match := range token.closes {
				opener := &tokens[match.open]
				if opener.remaining == 0 || !opener.previewOpen {
					out.WriteString(token.raw[:match.width])
				}
			}
			hide := token.remaining > 0 && token.previewOpen
			if !hide {
				out.WriteString(token.raw[:token.remaining])
				for i := len(token.opens) - 1; i >= 0; i-- {
					out.WriteString(token.raw[:token.opens[i]])
				}
			}
		case inlineCode:
			out.WriteString(token.raw)
			if !token.closed && token.body != "" && token.previewOpen {
				width := inlineRun(token.raw, 0)
				trailing := 0
				for i := len(token.body) - 1; i >= 0 && token.body[i] == '`'; i-- {
					trailing++
				}
				if trailing > 0 && trailing < width {
					out.WriteString(token.raw[:width-trailing])
				} else {
					if trailing > 0 {
						out.WriteByte(' ')
					}
					out.WriteString(token.raw[:width])
				}
			}
		default:
			out.WriteString(token.raw)
		}
	}
	return out.String()
}

// inlineSpan is a rendered payload with the same entity state as inlineRenderer.
// It never contains HTML or source-controlled synthetic placeholders.
type inlineSpan struct {
	body string
	tags []inlineTag
	code bool
}

func appendInlineSpan(spans *[]inlineSpan, span inlineSpan) {
	if span.body == "" {
		return
	}
	if span.code && strings.Contains(span.body, "\n") {
		// A code payload may be adjacent to another code-only link label.
		// Keep its newlines separate so coalescing adjacent code cannot expose
		// a blank source line inside an inline delimiter pair.
		for {
			line, rest, newline := strings.Cut(span.body, "\n")
			appendInlineSpan(spans, inlineSpan{body: line, code: true})
			if !newline {
				break
			}
			appendInlineSpan(spans, inlineSpan{body: "\n"})
			span.body = rest
		}
		return
	}
	// Canonical ordering merges equivalent nested emphasis without producing
	// adjacent copies of the same delimiter or crossing link boundaries.
	var tags []inlineTag
	for _, name := range []string{"a", "b", "i", "s"} {
		for _, tag := range span.tags {
			if tag.name == name {
				tags = append(tags, tag)
				break
			}
		}
	}
	span.tags = tags
	*spans = append(*spans, span)
}

// splitInlineParagraph cuts rendered payload, not delimiter source. Every
// emitted part reopens only matched entities and can be converted on its own.
func splitInlineParagraph(text string, limit int) []string {
	var spans []inlineSpan
	r := inlineRenderer{spans: &spans}
	r.render(parseInline(text, true))
	var out []string
	var chunk []inlineSpan
	units := 0
	flush := func() {
		if len(chunk) > 0 {
			out = append(out, serializeInlineSpans(chunk, limit)...)
			chunk = nil
			units = 0
		}
	}
	appendSpan := func(span inlineSpan) {
		for span.body != "" {
			piece := clipUnits(span.body, limit-units)
			if piece == "" {
				flush()
				// A supplementary rune cannot fit a one-unit budget. Keep the
				// rune intact, as hardSplitUnits does for literal payloads.
				if limit == 1 {
					_, size := utf8.DecodeRuneInString(span.body)
					piece = span.body[:size]
				} else {
					continue
				}
			}
			chunk = append(chunk, inlineSpan{body: piece, tags: span.tags, code: span.code})
			units += literalUTF16Len(piece)
			span.body = span.body[len(piece):]
			if units >= limit {
				flush()
			}
		}
	}
	var line []inlineSpan
	lineUnits := 0
	appendLine := func() {
		if lineUnits > limit {
			// Only a line that cannot fit by itself may be hard-cut. Start it
			// in a fresh message rather than consuming an earlier line's tail.
			flush()
			for _, span := range line {
				appendSpan(span)
			}
		} else {
			if units+lineUnits > limit {
				flush()
			}
			chunk = append(chunk, line...)
			units += lineUnits
			if units >= limit {
				flush()
			}
		}
		line = line[:0]
		lineUnits = 0
	}
	for _, span := range spans {
		for {
			body, rest, newline := strings.Cut(span.body, "\n")
			if body != "" {
				line = append(line, inlineSpan{body: body, tags: span.tags, code: span.code})
				lineUnits += literalUTF16Len(body)
			}
			if !newline {
				break
			}
			appendLine()
			// Newlines are original payload, not message separators. Keep one
			// even when the preceding fitting line used the entire budget.
			appendSpan(inlineSpan{body: "\n", tags: span.tags, code: span.code})
			span.body = rest
		}
	}
	appendLine()
	flush()
	return out
}

type inlineMarkerEvent struct {
	offset   int
	width    int
	boundary int
	closing  bool
}

func inlineTagMarker(tag inlineTag, bold, italic string) string {
	switch tag.name {
	case "b":
		return bold
	case "i":
		return italic
	case "s":
		return "~~"
	case "a":
		return "["
	}
	return ""
}

func inlineCodeMarker(body string) string {
	runs := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '`' {
			runs++
			i += inlineRun(body, i) - 1
		}
	}
	if runs == 0 {
		return "`"
	}
	// Among n distinct runs, at least one of widths 1 through n+1 is absent.
	used := make([]bool, runs+2)
	for i := 0; i < len(body); i++ {
		if body[i] == '`' {
			width := inlineRun(body, i)
			if width < len(used) {
				used[width] = true
			}
			i += width - 1
		}
	}
	for width := 1; ; width++ {
		if !used[width] {
			return strings.Repeat("`", width)
		}
	}
}

func encodeInlineSpans(spans []inlineSpan, bold, italic string) (string, []inlineMarkerEvent) {
	var out strings.Builder
	var active []inlineTag
	linePrefixSpace := true
	var events []inlineMarkerEvent
	boundary := 0
	writeMarker := func(marker string, closing bool, at int) {
		events = append(events, inlineMarkerEvent{offset: out.Len(), width: len(marker), boundary: at, closing: closing})
		out.WriteString(marker)
		linePrefixSpace = false
	}
	sync := func(want []inlineTag) {
		common := 0
		for common < len(active) && common < len(want) && active[common] == want[common] {
			common++
		}
		for i := len(active) - 1; i >= common; i-- {
			if active[i].name == "a" {
				out.WriteString("](")
				out.WriteString(active[i].href)
				out.WriteByte(')')
				linePrefixSpace = false
			} else {
				writeMarker(inlineTagMarker(active[i], bold, italic), true, boundary)
			}
		}
		for _, tag := range want[common:] {
			if tag.name == "a" {
				out.WriteByte('[')
				linePrefixSpace = false
			} else {
				writeMarker(inlineTagMarker(tag, bold, italic), false, boundary)
			}
		}
		active = want
	}
	writeText := func(text string, tags []inlineTag) {
		if text == "" {
			return
		}
		sync(tags)
		for i := range len(text) {
			if inlinePunctuation(text[i]) {
				out.WriteByte('\\')
			}
			out.WriteByte(text[i])
		}
		if newline := strings.LastIndexByte(text, '\n'); newline >= 0 {
			linePrefixSpace = strings.TrimSpace(text[newline+1:]) == ""
		} else if linePrefixSpace {
			linePrefixSpace = strings.TrimSpace(text) == ""
		}
	}
	for index := 0; index < len(spans); index++ {
		boundary = index
		span := spans[index]
		if span.code {
			sync(nil)
			if index+1 < len(spans) && spans[index+1].code {
				var body strings.Builder
				body.WriteString(span.body)
				for index+1 < len(spans) && spans[index+1].code {
					index++
					body.WriteString(spans[index].body)
				}
				span.body = body.String()
			}
			marker := inlineCodeMarker(span.body)
			body := span.body
			if strings.HasPrefix(body, "`") || strings.HasSuffix(body, "`") ||
				strings.HasPrefix(body, " ") && strings.HasSuffix(body, " ") && strings.Trim(body, " ") != "" {
				body = " " + body + " "
			}
			// Only a real source-line boundary can turn a long code delimiter
			// into a block fence. Intraword code must not receive a shell.
			shell := ""
			if len(marker) >= 3 && linePrefixSpace {
				var after byte
				if index+1 < len(spans) && len(spans[index+1].tags) > 0 {
					after = inlineTagMarker(spans[index+1].tags[0], bold, italic)[0]
				}
				for _, candidate := range []string{"**", "__", "~~"} {
					if candidate[0] != after {
						shell = candidate
						break
					}
				}
			}
			if shell != "" {
				writeMarker(shell, false, boundary)
			}
			out.WriteString(marker)
			out.WriteString(body)
			out.WriteString(marker)
			linePrefixSpace = false
			if shell != "" {
				writeMarker(shell, true, index+1)
			}
			continue
		}
		// A new message can begin or end inside a whitespace run. Markdown
		// emphasis cannot flank whitespace, so leave only edge whitespace
		// outside emphasis while retaining its exact visible bytes and links.
		var edgeTags []inlineTag
		if len(span.tags) > 0 && span.tags[0].name == "a" {
			edgeTags = span.tags[:1]
		}
		left := strings.TrimLeftFunc(span.body, unicode.IsSpace)
		middle := strings.TrimRightFunc(left, unicode.IsSpace)
		writeText(span.body[:len(span.body)-len(left)], edgeTags)
		writeText(middle, span.tags)
		writeText(left[len(middle):], edgeTags)
	}
	boundary = len(spans)
	sync(nil)
	result := out.String()
	return result, events
}

func protectInlineWhitespace(result string) string {
	// Protect whitespace-only lines without changing their visible payload.
	lines := strings.Split(result, "\n")
	protected := false
	for i, line := range lines {
		if line != "" && strings.TrimSpace(line) == "" {
			lines[i] = "`" + line + "`"
			protected = true
		}
	}
	if protected {
		result = strings.Join(lines, "\n")
	}
	if strings.TrimSpace(result) == "" {
		return literalFence(result, "```")
	}
	return result
}

// inlineSpansMismatch compares rendered text and non-whitespace entities, not
// delimiter spelling or tag nesting order. Spans already use canonical tags.
func inlineSpansMismatch(want, got []inlineSpan) int {
	wi, gi, wp, gp := 0, 0, 0, 0
	for wi < len(want) && gi < len(got) {
		wr, wn := utf8.DecodeRuneInString(want[wi].body[wp:])
		gr, gn := utf8.DecodeRuneInString(got[gi].body[gp:])
		if wr != gr {
			return wi
		}
		if !unicode.IsSpace(wr) {
			if want[wi].code != got[gi].code || len(want[wi].tags) != len(got[gi].tags) {
				return wi
			}
			for i, tag := range want[wi].tags {
				if tag != got[gi].tags[i] {
					return wi
				}
			}
		}
		wp += wn
		gp += gn
		if wp == len(want[wi].body) {
			wi++
			wp = 0
		}
		if gp == len(got[gi].body) {
			gi++
			gp = 0
		}
	}
	if wi != len(want) || gi != len(got) {
		return min(wi, len(want)-1)
	}
	return -1
}

// A failed generated delimiter identifies the precise payload boundary that
// needs a new message. Inspect all events in one pass, including link labels.
func inlineMarkerBoundaries(tokens []inlineToken, events []inlineMarkerEvent, count int) []bool {
	cuts := make([]bool, count+1)
	event := 0
	mark := func(e inlineMarkerEvent) {
		if e.boundary > 0 && e.boundary < count {
			cuts[e.boundary] = true
		}
	}
	var visit func([]inlineToken, int)
	visit = func(tokens []inlineToken, offset int) {
		for _, token := range tokens {
			end := offset + len(token.raw)
			if token.kind == inlineLink {
				visit(token.label, offset+strings.IndexByte(token.raw, '[')+1)
			} else if token.kind == inlineDelimiter && event < len(events) && events[event].offset == offset {
				first := event
				var want, got [2][3]int
				for event < len(events) && events[event].offset < end {
					e := events[event]
					side := 0
					if e.closing {
						side = 1
					}
					want[side][e.width]++
					event++
				}
				for _, width := range token.opens {
					got[0][width]++
				}
				for _, match := range token.closes {
					got[1][match.width]++
				}
				if token.remaining != 0 || want != got {
					for _, e := range events[first:event] {
						mark(e)
					}
				}
			}
			for event < len(events) && events[event].offset < end {
				mark(events[event])
				event++
			}
			offset = end
		}
	}
	visit(tokens, 0)
	return cuts
}

func chooseInlineSerialization(spans []inlineSpan, limit int) (string, []bool) {
	var best []bool
	bestCount := len(spans) + 1
	for _, markers := range [][2]string{{"**", "*"}, {"__", "_"}, {"**", "_"}, {"__", "*"}} {
		raw, events := encodeInlineSpans(spans, markers[0], markers[1])
		tokens := parseInline(raw, true)
		var rendered []inlineSpan
		r := inlineRenderer{spans: &rendered}
		r.render(tokens)
		mismatch := inlineSpansMismatch(spans, rendered)
		if mismatch < 0 {
			source := protectInlineWhitespace(raw)
			html := ConvertMarkdown(source)
			if renderedUTF16Len(html) <= limit {
				// Block parsing must not reinterpret synthetic inline source.
				if html == convertInline(source) {
					return source, nil
				}
				if strings.TrimSpace(raw) == "" && strings.HasPrefix(html, "<pre>") && strings.HasSuffix(html, "</pre>") && html[len("<pre>"):len(html)-len("</pre>")] == escapeHTML(raw) {
					return source, nil
				}
			}
		}
		cuts := inlineMarkerBoundaries(tokens, events, len(spans))
		cutCount := 0
		for _, cut := range cuts {
			if cut {
				cutCount++
			}
		}
		if cutCount == 0 && len(spans) > 1 {
			// A block boundary or a different valid delimiter matching can
			// disagree without leaving unmatched markers. Isolate that edge.
			cuts[max(1, mismatch)] = true
			cutCount = 1
		}
		if cutCount < bestCount {
			best, bestCount = cuts, cutCount
		}
	}
	return "", best
}

func serializeInlineSpans(spans []inlineSpan, limit int) []string {
	type part struct {
		spans  []inlineSpan
		source string
	}
	parts := []part{{spans: spans}}
	// A boundary cut can expose one additional fence-sensitive code opener.
	// Keep retries bounded: unresolved regions become independent entity runs,
	// never a recursive search over progressively shorter paragraph suffixes.
	for pass := range 4 {
		var next []part
		pending := false
		for _, p := range parts {
			if p.source != "" {
				next = append(next, p)
				continue
			}
			source, cuts := chooseInlineSerialization(p.spans, limit)
			if source != "" {
				next = append(next, part{source: source})
				continue
			}
			if len(p.spans) == 1 {
				span := p.spans[0]
				for pos := 0; pos < len(span.body); {
					_, size := utf8.DecodeRuneInString(span.body[pos:])
					glyph := inlineSpan{body: span.body[pos : pos+size], tags: span.tags, code: span.code}
					next = append(next, part{source: serializeInlineGlyph(glyph)})
					pos += size
				}
				continue
			}
			start := 0
			for i := 1; i < len(p.spans); i++ {
				if cuts[i] || pass >= 2 {
					next = append(next, part{spans: p.spans[start:i]})
					start = i
				}
			}
			next = append(next, part{spans: p.spans[start:]})
			pending = true
		}
		parts = next
		if !pending {
			break
		}
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.source
	}
	return out
}

// The terminal representation contains exactly one original rune. Code needs
// at most two backticks, so it cannot become a fence. For ordinary glyphs the
// outer markers flank message edges; all inner markers flank punctuation or
// the same marker run. Whitespace needs no entity state and is kept literally
// inside code. Thus this path preserves text/entities without another retry or
// a source-dependent assertion, including an indivisible supplementary rune.
func serializeInlineGlyph(span inlineSpan) string {
	char, _ := utf8.DecodeRuneInString(span.body)
	if char == '\n' {
		return literalFence(span.body, "```")
	}
	if span.code || unicode.IsSpace(char) {
		if char == '`' {
			return "`` ` ``"
		}
		return "`" + span.body + "`"
	}
	var out strings.Builder
	for _, tag := range span.tags {
		out.WriteString(inlineTagMarker(tag, "**", "*"))
	}
	for i := range len(span.body) {
		if inlinePunctuation(span.body[i]) {
			out.WriteByte('\\')
		}
		out.WriteByte(span.body[i])
	}
	for i := len(span.tags) - 1; i >= 0; i-- {
		tag := span.tags[i]
		if tag.name == "a" {
			out.WriteString("](")
			out.WriteString(tag.href)
			out.WriteByte(')')
		} else {
			out.WriteString(inlineTagMarker(tag, "**", "*"))
		}
	}
	return out.String()
}
