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
		block := collectMarkdownBlock(lines, i)
		switch block.kind {
		case markdownBlank:
			out = append(out, "")
		case markdownFence:
			out = append(out, "<pre>"+escapeHTML(strings.Join(block.body, "\n"))+"</pre>")
		case markdownTable:
			out = append(out, renderTable(block.body))
		case markdownHeading:
			out = append(out, convertHeadingInline(strings.TrimSpace(headingRe.FindStringSubmatch(lines[i])[2])))
		case markdownQuote:
			out = append(out, "<blockquote>"+ConvertMarkdown(strings.Join(block.body, "\n"))+"</blockquote>")
		case markdownRule:
			out = append(out, "──────────")
		case markdownList:
			out = append(out, convertList(lines[i:block.end]))
		case markdownParagraph:
			out = append(out, convertInline(strings.Join(lines[i:block.end], "\n")))
		}
		i = block.end
	}
	return strings.Join(out, "\n")
}

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+)$`)
	ruleRe     = regexp.MustCompile(`^\s*(?:-{3,}|\*{3,}|_{3,})\s*$`)
	listItemRe = regexp.MustCompile(`^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$`)
	tableDelim = regexp.MustCompile(`^\s*\|?[\s:|-]*-[\s:|-]*\|?\s*$`)
)

type markdownBlockKind uint8

const (
	markdownBlank markdownBlockKind = iota
	markdownFence
	markdownTable
	markdownHeading
	markdownQuote
	markdownRule
	markdownList
	markdownParagraph
)

type markdownBlock struct {
	kind markdownBlockKind
	end  int
	body []string
}

// collectMarkdownBlock is the converter's block dispatch, also used when a
// preview is projected or clipped. Collectors preserve recursive line boundaries.
func collectMarkdownBlock(lines []string, start int) markdownBlock {
	line := lines[start]
	trimmed := strings.TrimSpace(line)
	block := markdownBlock{end: start + 1}
	switch {
	case trimmed == "":
		block.kind = markdownBlank
	case isFence(trimmed):
		block.kind = markdownFence
		block.body, block.end = collectFence(lines, start)
	case isTableStart(lines, start):
		block.kind = markdownTable
		block.body, block.end = collectTable(lines, start)
	case headingRe.MatchString(line):
		block.kind = markdownHeading
	case strings.HasPrefix(trimmed, ">"):
		block.kind = markdownQuote
		block.body, block.end = collectQuote(lines, start)
	case ruleRe.MatchString(line):
		block.kind = markdownRule
	case listItemRe.MatchString(line):
		block.kind = markdownList
		for block.end < len(lines) {
			next := lines[block.end]
			if strings.TrimSpace(next) == "" || !(listItemRe.MatchString(next) || strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\t")) {
				break
			}
			block.end++
		}
	default:
		block.kind = markdownParagraph
		block.end = paragraphEnd(lines, start)
	}
	return block
}

// paragraphEnd shares the converter's block boundaries with inline splitting
// and preview projection. A soft newline belongs to the current paragraph.
func paragraphEnd(lines []string, start int) int {
	i := start + 1
	for i < len(lines) && !paragraphBoundary(lines, i) {
		i++
	}
	return i
}

func paragraphBoundary(lines []string, i int) bool {
	line := lines[i]
	trimmed := strings.TrimSpace(line)
	return trimmed == "" || isFence(trimmed) || isTableStart(lines, i) ||
		headingRe.MatchString(line) || strings.HasPrefix(trimmed, ">") ||
		ruleRe.MatchString(line) || listItemRe.MatchString(line)
}

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
func convertList(lines []string) string {
	var b strings.Builder
	openLen := 0
	for _, line := range lines {
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
		} else {
			continuation := convertInline(strings.TrimSpace(line))
			b.WriteString("\n" + strings.Repeat(" ", openLen) + continuation)
		}
	}
	return b.String()
}

var linkRe = regexp.MustCompile(`!?\[((?:\\.|[^\]\\])*)\]\(\s*([^\s)]+)(?:\s+("[^"]*"|'[^']*'))?\s*\)`)

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

// SplitForTelegram splits Markdown into independently convertible messages
// within limit UTF-16 units. Fitting blocks stay whole, oversized tables repeat
// their headers, and oversized ordinary paragraphs reopen matched inline styles
// around each payload cut instead of exposing their original delimiters.
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
		for index, piece := range oversizeBlock(block, limit) {
			if current == "" {
				current = piece
				continue
			}
			if index > 0 {
				parts = append(parts, current)
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

// splitTableBlock repeats the header for ordinary tables. If even one complete
// row cannot fit with its header, the entire table becomes labelled literal text
// so splitting never changes cell boundaries or interprets cell contents again.
func splitTableBlock(lines []string, limit int) []string {
	head := strings.Join(lines[:2], "\n")
	if convertLen(head) > limit {
		return splitLiteralBlock(literalTableText(lines), "```", limit)
	}

	var out []string
	chunk := head
	hasRows := false
	for _, row := range lines[2:] {
		single := head + "\n" + row
		if convertLen(single) > limit {
			return splitLiteralBlock(literalTableText(lines), "```", limit)
		}
		if !hasRows {
			chunk = single
			hasRows = true
			continue
		}
		candidate := chunk + "\n" + row
		if convertLen(candidate) > limit {
			out = append(out, chunk)
			chunk = single
		} else {
			chunk = candidate
		}
	}
	return append(out, chunk)
}

// literalTableText uses the same visible cell text and labels as compactTable,
// but leaves HTML escaping to the final fenced conversion.
func literalTableText(lines []string) string {
	header := tableCells(lines[0])
	rows := make([][]string, 0, len(lines)-2)
	columns := len(header)
	for _, line := range lines[2:] {
		row := tableCells(line)
		rows = append(rows, row)
		columns = max(columns, len(row))
	}
	labels := make([]string, columns)
	for c := range labels {
		if c < len(header) {
			labels[c] = header[c]
		}
		if labels[c] == "" {
			labels[c] = "col" + strconv.Itoa(c+1)
		}
	}
	if len(rows) == 0 {
		return strings.Join(labels, " · ")
	}
	var text strings.Builder
	for i, row := range rows {
		if i > 0 {
			text.WriteByte('\n')
		}
		for c, label := range labels {
			if c > 0 {
				text.WriteString(" · ")
			}
			text.WriteString(label)
			text.WriteString(": ")
			if c < len(row) {
				text.WriteString(row[c])
			}
		}
	}
	return text.String()
}

// splitFenceBlock preserves the literal body, including newline boundaries,
// while choosing a non-colliding marker for every newly wrapped chunk.
func splitFenceBlock(body []string, marker string, limit int) []string {
	return splitLiteralBlock(strings.Join(body, "\n"), marker, limit)
}

// splitLiteralBlock keeps fitting lines whole and only hard-splits oversized
// lines. Newlines remain part of the payload, not synthetic chunk separators.
func splitLiteralBlock(text, marker string, limit int) []string {
	var out []string
	var chunk strings.Builder
	units := 0
	flush := func() {
		if chunk.Len() > 0 {
			out = append(out, literalFence(chunk.String(), marker))
			chunk.Reset()
			units = 0
		}
	}
	appendPiece := func(piece string, size int) {
		if units+size > limit {
			flush()
		}
		chunk.WriteString(piece)
		units += size
	}
	for {
		line, rest, newline := strings.Cut(text, "\n")
		size := literalUTF16Len(line)
		if size <= limit {
			appendPiece(line, size)
		} else {
			flush()
			for _, piece := range hardSplitUnits(line, limit) {
				appendPiece(piece, literalUTF16Len(piece))
			}
		}
		if !newline {
			break
		}
		appendPiece("\n", 1)
		text = rest
	}
	flush()
	if len(out) == 0 {
		out = append(out, literalFence("", marker))
	}
	return out
}

// literalFence cannot close early, even when a hard cut exposes a marker-only
// line that did not exist in the original code block.
func literalFence(text, marker string) string {
	longest := len(marker) - 1
	for rest := text; ; {
		line, next, newline := strings.Cut(rest, "\n")
		line = strings.TrimSpace(line)
		if len(line) >= len(marker) && strings.Trim(line, marker[:1]) == "" {
			longest = max(longest, len(line))
		}
		if !newline {
			break
		}
		rest = next
	}
	if longest >= len(marker) {
		marker = strings.Repeat(marker[:1], longest+1)
	}
	return marker + "\n" + text + "\n" + marker
}

func literalUTF16Len(text string) int {
	units := 0
	for _, r := range text {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// packLines preserves ordinary paragraph scopes while leaving the converter's
// structured lines separate. A split paragraph already carries its newlines.
func packLines(lines []string, limit int) []string {
	var out []string
	current := ""
	flush := func() {
		if current != "" {
			out = append(out, current)
			current = ""
		}
	}
	for i := 0; i < len(lines); {
		next := i + 1
		ordinary := !paragraphBoundary(lines, i)
		if ordinary {
			next = paragraphEnd(lines, i)
		}
		text := strings.Join(lines[i:next], "\n")
		i = next
		if convertLen(text) > limit {
			flush()
			if ordinary {
				out = append(out, splitInlineParagraph(text, limit)...)
			} else {
				out = append(out, splitStructuredLine(text, limit)...)
			}
			continue
		}
		candidate := text
		if current != "" {
			candidate = current + "\n" + text
		}
		if current != "" && convertLen(candidate) > limit {
			flush()
			candidate = text
		}
		current = candidate
	}
	flush()
	return out
}

// splitStructuredLine retains the existing source-cut fallback for oversized
// headings, list items and quote lines; paragraph splitting must not reinterpret
// those blocks as ordinary inline text.
func splitStructuredLine(s string, limit int) []string {
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
		if fits || cut <= 1 {
			return pieces
		}
		cut = max(1, cut/2)
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

// TailForTelegram preserves recent lines and their real block context, then
// stabilizes only the final inline scope. Stored source remains untouched.
func TailForTelegram(md string, limit int) string {
	if limit <= 0 {
		return ""
	}
	lines := strings.Split(md, "\n")
	// Extract only a bounded original payload, before any synthetic opener or
	// closer exists. In particular, never expand a whole compact table to HTML.
	literal := func() string {
		return literalPreview(previewLiteralTail(lines, limit), limit)
	}
	start := previewTailStart(md, limit)
	line := strings.Count(md[:start], "\n")
	if start > 0 && md[start-1] != '\n' {
		next := strings.IndexByte(md[start:], '\n')
		if next < 0 || strings.TrimSpace(md[start+next+1:]) == "" {
			return literal()
		}
		start += next + 1
		line++
	}
	for {
		prefix, skipTo := previewBlockPrefix(lines, line, limit)
		if skipTo > 0 {
			if skipTo >= len(lines) {
				return literal()
			}
			for line < skipTo {
				start += len(lines[line]) + 1
				line++
			}
			if strings.TrimSpace(md[start:]) == "" {
				return literal()
			}
			continue
		}
		if prefix != "" && previewTailStart(prefix, limit-1) != 0 {
			return literal()
		}
		tail := md[start:]
		if prefix != "" {
			tail = prefix + tail
		}
		if previewTailStart(tail, limit) == 0 {
			projected := strings.Split(tail, "\n")
			if projectPreviewLines(projected) {
				if !previewScopesMatch(strings.Split(tail, "\n"), projected) {
					return literal()
				}
				tail = strings.Join(projected, "\n")
			}
			// Only this source-bounded candidate may be rendered. Restored
			// table context can expand even when its Markdown source fits.
			if previewTailStart(tail, limit) != 0 || convertLen(tail) > limit {
				return literal()
			}
			return tail
		}
		next := strings.IndexByte(md[start:], '\n')
		if next < 0 || strings.TrimSpace(md[start+next+1:]) == "" {
			return literal()
		}
		start += next + 1
		line++
	}
}

// previewBlockPrefix restores only the enclosing block syntax removed by a
// source cut. It follows the original converter scopes, never reparsing a
// quoted code payload as nested quotes or treating a closing fence as an opener.
// A nonzero second result jumps directly to the next safe source line.
func previewBlockPrefix(lines []string, line, limit int) (string, int) {
	for start := 0; start < len(lines); {
		block := collectMarkdownBlock(lines, start)
		if line >= block.end {
			start = block.end
			continue
		}
		switch block.kind {
		case markdownFence:
			if line > start {
				if line == start+len(block.body)+1 {
					return "", line + 1
				}
				return fenceMarker(strings.TrimSpace(lines[start])) + "\n", 0
			}
		case markdownTable:
			if line > start && previewTailStart(block.body[0], limit-1) != 0 {
				return block.body[0], 0
			}
			if line == start+1 {
				return block.body[0] + "\n", 0
			}
			if line > start+1 {
				return block.body[0] + "\n" + block.body[1] + "\n", 0
			}
		case markdownList:
			if line > start && !listItemRe.MatchString(lines[line]) {
				// Scan the discarded continuation once, stopping at the next
				// complete item rather than discarding the rest of the list.
				next := line + 1
				for next < block.end && !listItemRe.MatchString(lines[next]) {
					next++
				}
				return "", next
			}
		case markdownQuote:
			prefix, next := previewBlockPrefix(block.body, line-start, limit)
			if next > 0 {
				return "", start + next
			}
			if prefix != "" && previewTailStart(prefix, limit-1) != 0 {
				return prefix, 0
			}
			if prefix != "" {
				quote := lines[line][:len(lines[line])-len(block.body[line-start])]
				prefix = quote + strings.ReplaceAll(strings.TrimSuffix(prefix, "\n"), "\n", "\n"+quote) + "\n"
			}
			return prefix, 0
		}
		return "", 0
	}
	return "", 0
}

// previewScopesMatch rejects projections that expose new block syntax. Inline
// closers are harmless only while the converter sees the same recursive scopes.
func previewScopesMatch(before, after []string) bool {
	if len(before) != len(after) {
		return false
	}
	for i := 0; i < len(before); {
		original := collectMarkdownBlock(before, i)
		projected := collectMarkdownBlock(after, i)
		if original.kind != projected.kind || original.end != projected.end {
			return false
		}
		if original.kind == markdownQuote && !previewScopesMatch(original.body, projected.body) {
			return false
		}
		i = original.end
	}
	return true
}

// previewLiteralTail follows the original block scopes without materializing
// rendered HTML. Tables deliberately keep bounded raw rows: compact rendering
// could repeat an arbitrarily long header for every discarded row.
func previewLiteralTail(lines []string, limit int) string {
	if limit <= 0 {
		return ""
	}
	start := -1
	var last markdownBlock
	for i := 0; i < len(lines); {
		block := collectMarkdownBlock(lines, i)
		if block.kind != markdownBlank {
			start, last = i, block
		}
		i = block.end
	}
	if start < 0 {
		return strings.Repeat("\n", min(len(lines)-1, limit))
	}
	trailing := len(lines) - last.end
	if trailing >= limit {
		return strings.Repeat("\n", limit)
	}
	available := limit - trailing
	var payload string
	switch last.kind {
	case markdownQuote:
		payload = previewLiteralTail(last.body, available)
	case markdownFence, markdownTable:
		payload = previewSourceTail(last.body, available)
	default:
		payload = previewSourceTail(lines[start:last.end], available)
	}
	return payload + strings.Repeat("\n", trailing)
}

// previewSourceTail joins no more than limit UTF-16 units of original lines.
func previewSourceTail(lines []string, limit int) string {
	if len(lines) == 0 || limit <= 0 {
		return ""
	}
	start, cut, remaining := len(lines)-1, 0, limit
	for i := len(lines) - 1; i >= 0; i-- {
		if i < len(lines)-1 {
			remaining--
			if remaining < 0 {
				break
			}
		}
		start, cut = i, previewTailStart(lines[i], remaining)
		if cut > 0 {
			break
		}
		remaining -= utf16Len(lines[i])
	}
	tail := lines[start][cut:]
	if start+1 < len(lines) {
		tail += "\n" + strings.Join(lines[start+1:], "\n")
	}
	return tail
}

// projectPreviewLines uses complete paragraphs at every quote depth. Tables,
// fences and list continuations keep the same scopes as final conversion.
func projectPreviewLines(lines []string) bool {
	start := 0
	block := collectMarkdownBlock(lines, start)
	for block.end < len(lines) {
		start = block.end
		block = collectMarkdownBlock(lines, start)
	}
	switch block.kind {
	case markdownQuote:
		if !projectPreviewLines(block.body) {
			return false
		}
		for i, body := range block.body {
			line := lines[start+i]
			trimmed := strings.TrimLeft(line, " \t")
			content := strings.TrimLeft(trimmed[1:], " \t")
			if content != body {
				lines[start+i] = line[:len(line)-len(content)] + body
			}
		}
		return true
	case markdownParagraph:
		paragraph := strings.Join(lines[start:block.end], "\n")
		stable := previewInline(paragraph)
		if stable != paragraph {
			copy(lines[start:block.end], strings.Split(stable, "\n"))
			return true
		}
	case markdownHeading, markdownList:
		last := block.end - 1
		stable := previewInline(lines[last])
		if stable != lines[last] {
			lines[last] = stable
			return true
		}
	}
	return false
}

func previewTailStart(text string, limit int) int {
	start, units := len(text), 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(text[:start])
		if units+utf16.RuneLen(r) > limit {
			break
		}
		units += utf16.RuneLen(r)
		start -= size
	}
	return start
}

func literalPreview(text string, limit int) string {
	if limit < 8 {
		return text[previewTailStart(text, min(limit, 1)):]
	}
	for {
		backticks, tildes, run := 0, 0, 0
		var previous rune
		for _, r := range text {
			if r != previous {
				run = 0
			}
			run++
			if r == '`' {
				backticks = max(backticks, run)
			} else if r == '~' {
				tildes = max(tildes, run)
			}
			previous = r
		}
		character, longest := "`", backticks
		if tildes < backticks {
			character, longest = "~", tildes
		}
		length := max(3, longest+1)
		available := limit - 2*length - 2
		if available < 0 {
			text = text[previewTailStart(text, limit/3):]
			continue
		}
		marker := strings.Repeat(character, length)
		return marker + "\n" + text[previewTailStart(text, available):] + "\n" + marker
	}
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
