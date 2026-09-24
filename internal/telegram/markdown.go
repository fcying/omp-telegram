package telegram

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// ConvertMarkdown converts GFM-style markdown into the HTML subset accepted by
// the Telegram Bot API (parse_mode=HTML). Unsupported constructs degrade to
// readable text: tables become aligned blocks in <pre>, headings become bold,
// lists keep their markers. Text is HTML-escaped, so unformatted input with
// stray angle brackets passes through unchanged in appearance.
func ConvertMarkdown(md string) string {
	lines := strings.Split(md, "\n")
	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			out = append(out, "")
			i++
		case isFence(trimmed):
			body, next := collectFence(lines, i)
			out = append(out, "<pre>"+escapeHTML(strings.Join(body, "\n"))+"</pre>")
			i = next
		case isTableStart(lines, i):
			body, next := collectTable(lines, i)
			out = append(out, renderTable(body))
			i = next
		case headingRe.MatchString(line):
			out = append(out, "<b>"+convertInline(strings.TrimSpace(headingRe.FindStringSubmatch(line)[2]))+"</b>")
			i++
		case strings.HasPrefix(trimmed, ">"):
			body, next := collectQuote(lines, i)
			out = append(out, "<blockquote>"+ConvertMarkdown(strings.Join(body, "\n"))+"</blockquote>")
			i = next
		case ruleRe.MatchString(line):
			out = append(out, "──────────")
			i++
		case listItemRe.MatchString(line):
			out = append(out, convertList(lines, &i))
		default:
			out = append(out, convertInline(line))
			i++
		}
	}
	return strings.Join(out, "\n")
}

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+)$`)
	ruleRe     = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)
	listItemRe = regexp.MustCompile(`^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$`)
	tableDelim = regexp.MustCompile(`^\s*\|?[\s:|-]*-[\s:|-]*\|?\s*$`)
)

func isFence(trimmed string) bool {
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func fenceMarker(trimmed string) string {
	char := trimmed[0]
	n := 0
	for n < len(trimmed) && trimmed[n] == char {
		n++
	}
	return strings.Repeat(string(char), n)
}

// collectFence returns the code lines (without fences) and the index after the
// closing fence. An unclosed fence runs to the end of input.
func collectFence(lines []string, start int) ([]string, int) {
	marker := fenceMarker(strings.TrimSpace(lines[start]))
	var body []string
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == marker {
			return body, i + 1
		}
		body = append(body, lines[i])
	}
	return body, len(lines)
}

func isTableStart(lines []string, i int) bool {
	if i+1 >= len(lines) || !tableDelim.MatchString(lines[i+1]) {
		return false
	}
	return strings.Contains(strings.TrimSpace(lines[i]), "|") && strings.Contains(strings.TrimSpace(lines[i+1]), "|")
}

// collectTable returns the table lines (header, delimiter, body rows) and the
// index after the table.
func collectTable(lines []string, i int) ([]string, int) {
	var rows []string
	for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
		rows = append(rows, strings.TrimSpace(lines[i]))
		i++
	}
	return rows, i
}

func tableCells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	parts := strings.Split(row, "|")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(stripInlineMarks(p))
	}
	return parts
}

func columnAligns(delimiter []string) []string {
	aligns := make([]string, len(delimiter))
	for i, d := range delimiter {
		left := strings.HasPrefix(d, ":")
		right := strings.HasSuffix(d, ":")
		switch {
		case left && right:
			aligns[i] = "center"
		case right:
			aligns[i] = "right"
		case left:
			aligns[i] = "left"
		}
	}
	return aligns
}

// renderTable formats pipe-table lines as a space-aligned block inside <pre>,
// mirroring the layout the agent intended; Telegram has no table entity.
func renderTable(lines []string) string {
	if len(lines) < 2 {
		return "<pre>" + escapeHTML(strings.Join(lines, "\n")) + "</pre>"
	}
	header := tableCells(lines[0])
	aligns := columnAligns(tableCells(lines[1]))
	rows := [][]string{header}
	for _, l := range lines[2:] {
		rows = append(rows, tableCells(l))
	}
	columns := 0
	for _, r := range rows {
		if len(r) > columns {
			columns = len(r)
		}
	}
	if columns == 0 {
		return ""
	}
	widths := make([]int, columns)
	for _, r := range rows {
		for c, cell := range r {
			if n := utf8.RuneCountInString(cell); n > widths[c] {
				widths[c] = n
			}
		}
	}
	total := 0
	for _, w := range widths {
		total += w
	}
	if total+3*(columns-1) > maxTableWidth {
		return compactTable(rows, header, columns)
	}

	pad := func(cell string, c int) string {
		missing := widths[c] - utf8.RuneCountInString(cell)
		align := ""
		if c < len(aligns) {
			align = aligns[c]
		}
		switch align {
		case "right":
			return strings.Repeat(" ", missing) + cell
		case "center":
			left := missing / 2
			return strings.Repeat(" ", left) + cell + strings.Repeat(" ", missing-left)
		default:
			return cell + strings.Repeat(" ", missing)
		}
	}
	format := func(r []string) string {
		cells := make([]string, columns)
		for c := range cells {
			if c < len(r) {
				cells[c] = pad(r[c], c)
			} else {
				cells[c] = strings.Repeat(" ", widths[c])
			}
		}
		return strings.TrimRight(strings.Join(cells, " | "), " ")
	}
	var b strings.Builder
	b.WriteString(format(header))
	seps := make([]string, columns)
	for c, w := range widths {
		seps[c] = strings.Repeat("-", w)
	}
	b.WriteString("\n" + strings.Join(seps, " | "))
	for _, r := range rows[1:] {
		b.WriteString("\n" + format(r))
	}
	return "<pre>" + escapeHTML(b.String()) + "</pre>"
}

func collectQuote(lines []string, start int) ([]string, int) {
	var body []string
	for i := start; i < len(lines); i++ {
		trimmed := strings.TrimLeft(lines[i], " \t")
		if !strings.HasPrefix(trimmed, ">") {
			return body, i
		}
		body = append(body, strings.TrimLeft(strings.TrimPrefix(trimmed, ">"), " \t"))
	}
	return body, len(lines)
}

// convertList renders consecutive list items, keeping ordering markers and
// indenting nested items. Continuation lines (no marker, indented) join the
// open item with matching padding.
func convertList(lines []string, i *int) string {
	var b strings.Builder
	openLen := 0
	for *i < len(lines) {
		line := lines[*i]
		if strings.TrimSpace(line) == "" {
			// A blank line ends the list unless the next line continues an item.
			if *i+1 < len(lines) && listItemRe.MatchString(lines[*i+1]) {
				*i++
				continue
			}
			break
		}
		if m := listItemRe.FindStringSubmatch(line); m != nil {
			indent := strings.ReplaceAll(m[1], "\t", "    ")
			ordered := m[2][0] >= '0' && m[2][0] <= '9'
			marker := "- "
			if ordered {
				marker = strings.TrimRight(m[2], ".)") + ". "
			}
			prefix := indent + marker
			openLen = len([]rune(prefix))
			body := convertInline(strings.TrimRight(m[3], " \t"))
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(prefix + body)
		} else if openLen > 0 && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
			continuation := convertInline(strings.TrimSpace(line))
			b.WriteString("\n" + strings.Repeat(" ", openLen) + continuation)
		} else {
			break
		}
		*i++
	}
	return b.String()
}

var (
	codeRe = regexp.MustCompile("`+")
	linkRe = regexp.MustCompile(`!?\[([^\]]*)\]\(\s*([^\s)]+)(?:\s+("[^"]*"|'[^']*'))?\s*\)`)
)

func convertInline(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	for len(s) > 0 {
		loc := codeRe.FindStringIndex(s)
		if loc == nil {
			b.WriteString(convertRichText(s))
			break
		}
		b.WriteString(convertRichText(s[:loc[0]]))
		marker := s[loc[0]:loc[1]]
		rest := s[loc[1]:]
		end := strings.Index(rest, marker)
		if end < 0 {
			b.WriteString(escapeHTML(marker))
			s = rest
			continue
		}
		b.WriteString("<code>" + escapeHTML(rest[:end]) + "</code>")
		s = rest[end+len(marker):]
	}
	return b.String()
}

// convertRichText applies links and text emphasis to one non-code segment.
// Escaping runs first so markdown markers survive it; tags are injected only
// afterwards, which keeps the emitted HTML valid no matter what the source
// contained. Links are parked behind placeholders first so emphasis scanning
// never rewrites a URL, and vice versa.
func convertRichText(s string) string {
	if !strings.ContainsAny(s, "[*_~<>&`") {
		return escapeHTML(s)
	}
	s = escapeHTML(s)
	var links []string
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := linkRe.FindStringSubmatch(m)
		title := applyEmphasis(groups[1])
		if strings.HasPrefix(m, "!") { // images have no entity: keep a labeled link
			if title != "" {
				title = "🖼 " + title
			} else {
				title = "🖼"
			}
		}
		// groups[2] is already HTML-escaped; only the quote can still break the
		// attribute here (ampersands must not be escaped twice).
		links = append(links, `<a href="`+strings.ReplaceAll(groups[2], `"`, "&quot;")+`">`+title+`</a>`)
		return "\x01" + itoa(len(links)-1) + "\x02"
	})
	s = applyEmphasis(s)
	for i, tag := range links {
		s = strings.ReplaceAll(s, "\x01"+itoa(i)+"\x02", tag)
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

// applyEmphasis rewrites **, __, *, _ and ~~ delimiters into tags. It is a
// left-to-right scanner because RE2 lacks backreferences: the scanner pairs
// runs of identical markers and validates their content.
func applyEmphasis(s string) string {
	if !strings.ContainsAny(s, "*_~") {
		return s
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch c {
		case '~':
			if i+1 < len(s) && s[i+1] == '~' {
				if end := strings.Index(s[i+2:], "~~"); end >= 0 {
					inner := s[i+2 : i+2+end]
					if validEmphasis(inner) {
						b.WriteString("<s>")
						b.WriteString(applyEmphasis(inner))
						b.WriteString("</s>")
						i += 2 + end + 2
						continue
					}
				}
			}
			b.WriteByte(c)
			i++
		case '*', '_':
			run := 1
			for i+run < len(s) && s[i+run] == c {
				run++
			}
			if run > 2 {
				run = 2
			}
			if run == 2 {
				closer := strings.Repeat(string(c), 2)
				if end := strings.Index(s[i+2:], closer); end >= 0 {
					inner := s[i+2 : i+2+end]
					if validEmphasis(inner) {
						b.WriteString("<b>")
						b.WriteString(applyEmphasis(inner))
						b.WriteString("</b>")
						i += 2 + end + 2
						continue
					}
				}
			} else if c == '*' {
				if end := strings.IndexByte(s[i+1:], '*'); end >= 0 {
					inner := s[i+1 : i+1+end]
					if validEmphasis(inner) {
						b.WriteString("<i>")
						b.WriteString(applyEmphasis(inner))
						b.WriteString("</i>")
						i += 2 + end
						continue
					}
				}
			} else if !wordByteAt(s, i-1) {
				if end := strings.IndexByte(s[i+1:], '_'); end >= 0 && !wordByteAt(s, i+end+2) {
					inner := s[i+1 : i+1+end]
					if validEmphasis(inner) {
						b.WriteString("<i>")
						b.WriteString(applyEmphasis(inner))
						b.WriteString("</i>")
						i += 2 + end
						continue
					}
				}
			}
			b.WriteString(strings.Repeat(string(c), run))
			i += run
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

func validEmphasis(inner string) bool {
	return inner != "" && inner == strings.TrimSpace(inner)
}

// wordByteAt reports whether the byte at index i continues a word. Underscore
// emphasis must not fire inside identifiers like snake_case (Byte-level check:
// any UTF-8 continuation byte counts as part of a word.)
func wordByteAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80
}

// stripInlineMarks removes inline markup from table cells: links keep their
// label (every label, matched one by one), emphasis and code markers vanish.
func stripInlineMarks(s string) string {
	s = linkRe.ReplaceAllStringFunc(s, func(m string) string {
		return linkRe.FindStringSubmatch(m)[1]
	})
	return strings.NewReplacer("**", "", "__", "", "~~", "", "`", "").Replace(s)
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// renderedUTF16Len counts the UTF-16 units Telegram sees in HTML produced by
// ConvertMarkdown: Telegram applies the message limit to the text after
// parsing entities, so generated tags consume nothing and each escape counts
// as the single character it renders to. The scanner relies on
// ConvertMarkdown's invariant that escaped text never contains a raw '<':
// every '<' opens injected markup that ends at the next '>'.
func renderedUTF16Len(html string) int {
	n := 0
	for i := 0; i < len(html); {
		switch html[i] {
		case '<':
			if end := strings.IndexByte(html[i:], '>'); end >= 0 {
				i += end + 1
				continue
			}
			n++
			i++
		case '&':
			n++
			switch {
			case strings.HasPrefix(html[i:], "&amp;"):
				i += 5
			case strings.HasPrefix(html[i:], "&lt;"), strings.HasPrefix(html[i:], "&gt;"):
				i += 4
			case strings.HasPrefix(html[i:], "&quot;"):
				i += 6
			default:
				i++
			}
		default:
			r, size := utf8.DecodeRuneInString(html[i:])
			units := utf16.RuneLen(r)
			if units < 0 {
				units = 1
			}
			n += units
			i += size
		}
	}
	return n
}

// plainASCIIText identifies input whose rendered UTF-16 length equals its byte length.
func plainASCIIText(s string) bool {
	text, spaces := false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			text = true
		case c == ' ':
			spaces = true
		case c == '\n':
			if spaces && !text {
				return false
			}
			text, spaces = false, false
		default:
			return false
		}
	}
	return !spaces || text
}

// convertLen is the length model for every split and clip decision: the
// UTF-16 length of the text Telegram renders, not of the generated markup.
func convertLen(md string) int {
	if plainASCIIText(md) {
		return len(md)
	}
	return renderedUTF16Len(ConvertMarkdown(md))
}

// MaxMessageUTF16 is Telegram per-message text limit in UTF-16 code units.
const MaxMessageUTF16 = 4096

// maxTableWidth is the widest aligned <pre> table that survives a phone
// screen; wider tables degrade to one labelled line per row, which Telegram
// can wrap.
const maxTableWidth = 48

// compactTable renders rows as "- label: value" style lines instead of an
// aligned block, so no line depends on a monospace column that would be cut
// off horizontally.
func compactTable(rows [][]string, header []string, columns int) string {
	labels := make([]string, columns)
	for c := range labels {
		label := ""
		if c < len(header) {
			label = header[c]
		}
		if label == "" {
			label = "col" + strconv.Itoa(c+1)
		}
		labels[c] = label
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows[1:] {
		cells := make([]string, 0, columns)
		for c := 0; c < columns; c++ {
			value := ""
			if c < len(row) {
				value = row[c]
			}
			cells = append(cells, escapeHTML(labels[c])+": "+escapeHTML(value))
		}
		lines = append(lines, strings.Join(cells, " · "))
	}
	return strings.Join(lines, "\n")
}

// SplitForTelegram splits markdown into parts whose rendered text each fits
// within limit UTF-16 units. Blocks stay whole: paragraphs, lists, tables and
// code fences are never cut apart, and an oversized table repeats its header
// and delimiter rows in every part. Only a single line with no break point is
// hard-split, so a reply never degrades to raw markdown just because markup
// pushed it past the message cap.
func SplitForTelegram(md string, limit int) []string {
	if strings.TrimSpace(md) == "" {
		return nil
	}
	if limit <= 0 {
		limit = MaxMessageUTF16
	}
	var parts []string
	current := ""
	for _, block := range markdownBlocks(md) {
		for _, piece := range oversizeBlock(block, limit) {
			if current == "" {
				current = piece
				continue
			}
			candidate := current + "\n\n" + piece
			if convertLen(candidate) <= limit {
				current = candidate
				continue
			}
			parts = append(parts, current)
			current = piece
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

// markdownBlocks cuts markdown into blank-line separated blocks, keeping a
// code fence and a pipe table intact even when their own lines contain no
// blank line.
func markdownBlocks(md string) []string {
	lines := strings.Split(md, "\n")
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case trimmed == "":
			flush()
			i++
		case isFence(trimmed):
			body, next := collectFence(lines, i)
			flush()
			block := append([]string{lines[i]}, body...)
			if next-1 > i && strings.TrimSpace(lines[next-1]) == fenceMarker(trimmed) {
				block = append(block, lines[next-1])
			}
			blocks = append(blocks, strings.Join(block, "\n"))
			i = next
		case isTableStart(lines, i):
			body, next := collectTable(lines, i)
			flush()
			blocks = append(blocks, strings.Join(body, "\n"))
			i = next
		default:
			cur = append(cur, lines[i])
			i++
		}
	}
	flush()
	return blocks
}

// oversizeBlock breaks one block that cannot fit into limit units.
func oversizeBlock(block string, limit int) []string {
	if convertLen(block) <= limit {
		return []string{block}
	}
	lines := strings.Split(block, "\n")
	trimmed := strings.TrimSpace(lines[0])
	if len(lines) > 1 && isTableStart(lines, 0) {
		return splitTableBlock(lines, limit)
	}
	if isFence(trimmed) {
		body, _ := collectFence(lines, 0)
		return splitFenceBlock(body, fenceMarker(trimmed), limit)
	}
	return packLines(lines, limit)
}

// splitTableBlock packs table body rows into parts, repeating the header and
// delimiter row so every part renders as a table on its own.
func splitTableBlock(lines []string, limit int) []string {
	head := strings.Join(lines[:2], "\n")
	var out []string
	var chunk []string
	for _, row := range fitRows(lines[2:], limit) {
		candidate := append(append([]string{}, chunk...), row)
		if len(chunk) > 0 && convertLen(head+"\n"+strings.Join(candidate, "\n")) > limit {
			out = append(out, head+"\n"+strings.Join(chunk, "\n"))
			chunk = nil
		}
		chunk = append(chunk, row)
	}
	if len(chunk) > 0 {
		out = append(out, head+"\n"+strings.Join(chunk, "\n"))
	}
	if len(out) == 0 {
		out = []string{strings.Join(lines, "\n")}
	}
	return out
}

// splitFenceBlock packs code lines into several fences of the same marker.
func splitFenceBlock(body []string, marker string, limit int) []string {
	var out []string
	var chunk []string
	fence := func(lines []string) string {
		return marker + "\n" + strings.Join(lines, "\n") + "\n" + marker
	}
	for _, line := range fitCodeRows(body, limit) {
		candidate := append(append([]string{}, chunk...), line)
		if len(chunk) > 0 && convertLen(fence(candidate)) > limit {
			out = append(out, fence(chunk))
			chunk = nil
		}
		chunk = append(chunk, line)
	}
	if len(chunk) > 0 {
		out = append(out, fence(chunk))
	}
	if len(out) == 0 {
		out = []string{fence(nil)}
	}
	return out
}

// packLines groups lines so each group converts within limit units.
func packLines(lines []string, limit int) []string {
	var out []string
	var chunk []string
	for _, line := range lines {
		for _, piece := range fitPiece(line, limit) {
			candidate := append(append([]string{}, chunk...), piece)
			if len(chunk) > 0 && convertLen(strings.Join(candidate, "\n")) > limit {
				out = append(out, strings.Join(chunk, "\n"))
				chunk = nil
			}
			chunk = append(chunk, piece)
		}
	}
	if len(chunk) > 0 {
		out = append(out, strings.Join(chunk, "\n"))
	}
	return out
}

// fitRows normalises rows whose own markup would not fit, so the packers never
// have to measure a single overflowing entry.
func fitRows(rows []string, limit int) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fitPiece(row, limit)...)
	}
	return out
}

// fitCodeRows normalises fenced lines by their literal length. Inside a code
// block Telegram shows every character as-is, so a fence row must fit on its
// own before the packer assembles blocks; inline markdown measurement would
// undercount it (*a pairs render shorter than they occupy in a fence).
func fitCodeRows(rows []string, limit int) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if utf16Len(row) <= limit {
			out = append(out, row)
			continue
		}
		out = append(out, hardSplitUnits(row, limit)...)
	}
	return out
}

// fitPiece breaks a run of text until every piece renders within limit units.
// The rendered form can still exceed the piece budget (a --- rule widens to a
// dash run, compact tables add labels), so the budget is halved until
// measurement passes; at an 8-unit cut it always does.
func fitPiece(s string, limit int) []string {
	if limit < 64 {
		limit = 64
	}
	if convertLen(s) <= limit {
		return []string{s}
	}
	cut := utf16Len(s) / 2
	for {
		pieces := hardSplitUnits(s, cut)
		fits := true
		for _, piece := range pieces {
			if convertLen(piece) > limit {
				fits = false
				break
			}
		}
		if fits || cut <= 8 {
			return pieces
		}
		if cut /= 2; cut < 8 {
			cut = 8
		}
	}
}

// hardSplitUnits cuts text that offers no line boundary, by UTF-16 units.
func hardSplitUnits(s string, limit int) []string {
	var out []string
	start, n := 0, 0
	for i, r := range s {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		if n+size > limit {
			out = append(out, s[start:i])
			start = i
			n = 0
		}
		n += size
	}
	if start < len(s) || len(out) == 0 {
		out = append(out, s[start:])
	}
	return out
}

// ClipConvertible trims text so that its rendered form stays within limit
// UTF-16 units, marking the cut with an ellipsis. Live previews are edited in
// place and cannot be split across messages, so they clip instead; without
// this a long preview would fall back to raw markdown.
func ClipConvertible(s string, limit int) string {
	if limit <= 0 {
		limit = MaxMessageUTF16
	}
	if convertLen(s) <= limit {
		return s
	}
	const marker = "\n…"
	lo, hi := 0, utf16Len(s)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if convertLen(clipUnits(s, mid)+marker) <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return clipUnits(s, lo) + marker
}

// clipUnits returns the prefix of s occupying at most units UTF-16 units.
func clipUnits(s string, units int) string {
	if units <= 0 {
		return ""
	}
	used := 0
	for i, r := range s {
		size := utf16.RuneLen(r)
		if size < 0 {
			size = 1
		}
		if used+size > units {
			return s[:i]
		}
		used += size
	}
	return s
}
