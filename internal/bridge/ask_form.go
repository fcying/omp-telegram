package bridge

import (
	"errors"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	xhtml "golang.org/x/net/html"
)

const (
	maxAskFormBytes   = 1 << 20
	maxAskFormFields  = 128
	maxAskFormOptions = 256
)

var errAskFormPattern = errors.New("form pattern is unsupported")

var askFormOtherLabel = regexp.MustCompile(`(?i)^other\b`)

type askForm struct {
	fields []askFormField
	data   map[string]any
}

type askFormField struct {
	name        string
	question    askQuestion
	selected    []bool
	optionCount int // Original option count before filtering for display.
	inputType   string
	placeholder string
	value       string
	required    bool
	other       bool
	hasOther    bool
	min         *float64
	max         *float64
	minLength   int
	maxLength   int
	pattern     string
}

func parseAskForm(source string) (*askForm, error) {
	if len(source) == 0 || len(source) > maxAskFormBytes {
		return nil, errors.New("form size is unsupported")
	}
	document, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return nil, errors.New("form markup is invalid")
	}
	root := firstFormElement(document)
	if root == nil {
		root = document
	}
	result := &askForm{data: make(map[string]any)}
	labels := make(map[string]*xhtml.Node)
	collectFormLabels(root, labels)
	groups := make(map[string]int)
	controlCount := 0
	var parseErr error
	walkFormControls(root, func(node *xhtml.Node) bool {
		controlCount++
		if controlCount > maxAskFormFields*4 {
			parseErr = errors.New("form has too many controls")
			return false
		}
		if node.Type != xhtml.ElementNode {
			return true
		}
		tag := strings.ToLower(node.Data)
		name, _ := formAttribute(node, "name")
		if name == "" {
			return true
		}
		if tag == "input" {
			inputType, _ := formAttribute(node, "type")
			inputType = strings.ToLower(inputType)
			if inputType == "" {
				inputType = "text"
			}
			if inputType == "password" {
				parseErr = errors.New("password fields cannot be collected securely")
				return false
			}
			if inputType == "hidden" {
				value, _ := formAttribute(node, "value")
				result.data[name] = value
				return true
			}
			if inputType == "button" || inputType == "file" || inputType == "image" || inputType == "reset" || inputType == "submit" {
				return true
			}
			if inputType == "radio" || inputType == "checkbox" {
				addAskFormChoice(result, groups, node, labels, name, inputType == "checkbox")
				return true
			}
			addAskFormText(result, node, labels, name, inputType, false)
			return true
		}
		if tag == "select" {
			addAskFormSelect(result, node, labels, name)
			return true
		}
		if tag == "textarea" {
			addAskFormText(result, node, labels, name, "textarea", true)
		}
		return true
	})
	if parseErr != nil {
		return nil, parseErr
	}
	if len(result.fields) > maxAskFormFields {
		return nil, errors.New("form has too many fields")
	}
	seenNames := make(map[string]struct{}, len(result.fields))
	for i := range result.fields {
		field := &result.fields[i]
		if _, exists := seenNames[field.name]; exists {
			return nil, errors.New("form contains incompatible controls with the same name")
		}
		seenNames[field.name] = struct{}{}
		if field.pattern != "" {
			if _, err := regexp.Compile("^(?:" + field.pattern + ")$"); err != nil {
				return nil, errAskFormPattern
			}
		}
		if field.question.ID == "" {
			field.question.ID = field.name
		}
		if len(field.question.Options) > maxAskFormOptions {
			return nil, errors.New("form choice has too many options")
		}
		if field.inputType == "radio" || field.inputType == "checkbox" || field.inputType == "select" {
			if len(field.question.Options) == 0 {
				return nil, errors.New("form choice has no enabled options")
			}
			if field.inputType == "radio" || field.inputType == "checkbox" {
				field.other = field.optionCount > 1 && !field.hasOther
			}
		}
	}
	return result, nil
}

func firstFormElement(node *xhtml.Node) *xhtml.Node {
	if node.Type == xhtml.ElementNode && strings.EqualFold(node.Data, "form") {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if form := firstFormElement(child); form != nil {
			return form
		}
	}
	return nil
}

func walkFormControls(node *xhtml.Node, visit func(*xhtml.Node) bool) bool {
	if node.Type == xhtml.ElementNode {
		switch strings.ToLower(node.Data) {
		case "script", "style", "template":
			return true
		case "input", "select", "textarea":
			if !visit(node) {
				return false
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if !walkFormControls(child, visit) {
			return false
		}
	}
	return true
}

func collectFormLabels(node *xhtml.Node, labels map[string]*xhtml.Node) {
	if node.Type == xhtml.ElementNode && strings.EqualFold(node.Data, "label") {
		if id, ok := formAttribute(node, "for"); ok && id != "" {
			if _, exists := labels[id]; !exists {
				labels[id] = node
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectFormLabels(child, labels)
	}
}

func addAskFormChoice(form *askForm, groups map[string]int, node *xhtml.Node, labels map[string]*xhtml.Node, name string, multi bool) {
	inputType := "radio"
	if multi {
		inputType = "checkbox"
	}
	key := inputType + "\x00" + name
	index, exists := groups[key]
	if !exists {
		field := askFormField{
			name: name,
			question: askQuestion{
				ID: name, Question: formQuestion(node, name),
				Header: formHeader(node, formQuestion(node, name)), Multi: multi,
			},
			inputType: inputType,
			required:  formHasAttribute(node, "required"),
		}
		index = len(form.fields)
		groups[key] = index
		form.fields = append(form.fields, field)
	}
	field := &form.fields[index]
	field.required = field.required || formHasAttribute(node, "required")
	field.optionCount++
	value, ok := formAttribute(node, "value")
	if !ok {
		value = "on"
	}
	label := formLabel(node, labels)
	if label == "" {
		label = value
	}
	if askFormOtherLabel.MatchString(strings.TrimSpace(label)) {
		field.hasOther = true
	}
	if formHasAttribute(node, "disabled") {
		return
	}
	field.question.Options = append(field.question.Options, askOption{Label: label, Value: value})
	field.selected = append(field.selected, formHasAttribute(node, "checked"))
}

func addAskFormSelect(form *askForm, node *xhtml.Node, labels map[string]*xhtml.Node, name string) {
	multi := formHasAttribute(node, "multiple")
	field := askFormField{
		name: name,
		question: askQuestion{
			ID: name, Question: formQuestion(node, name),
			Header: formHeader(node, formQuestion(node, name)), Multi: multi,
		},
		inputType: "select",
		required:  formHasAttribute(node, "required"),
	}
	selected := false
	walkOptions(node, func(option *xhtml.Node) {
		field.optionCount++
		checked := formHasAttribute(option, "selected")
		selected = selected || checked
		if optionDisabled(option, node) {
			return
		}
		label := strings.TrimSpace(formVisibleText(option))
		if label == "" {
			return
		}
		value, ok := formAttribute(option, "value")
		if !ok {
			value = label
		}
		field.question.Options = append(field.question.Options, askOption{Label: label, Value: value})
		field.selected = append(field.selected, checked)
	})
	if !multi && !selected {
		for index := range field.selected {
			field.selected[index] = true
			break
		}
	}
	form.fields = append(form.fields, field)
}

func walkOptions(node *xhtml.Node, visit func(*xhtml.Node)) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == xhtml.ElementNode && strings.EqualFold(child.Data, "option") {
			visit(child)
			continue
		}
		walkOptions(child, visit)
	}
}

func optionDisabled(option, selectNode *xhtml.Node) bool {
	if formHasAttribute(option, "disabled") {
		return true
	}
	for parent := option.Parent; parent != nil && parent != selectNode; parent = parent.Parent {
		if parent.Type == xhtml.ElementNode && strings.EqualFold(parent.Data, "optgroup") && formHasAttribute(parent, "disabled") {
			return true
		}
	}
	return false
}

func addAskFormText(form *askForm, node *xhtml.Node, labels map[string]*xhtml.Node, name, inputType string, multiline bool) {
	value := ""
	if multiline {
		value = formTextContent(node)
		value = strings.TrimPrefix(strings.TrimPrefix(value, "\r\n"), "\n")
	} else {
		value, _ = formAttribute(node, "value")
	}
	placeholder, _ := formAttribute(node, "placeholder")
	field := askFormField{
		name: name,
		question: askQuestion{
			ID: name, Question: formTextQuestion(node, labels, name),
			Header: formHeader(node, formTextQuestion(node, labels, name)),
		},
		inputType:   inputType,
		placeholder: placeholder,
		value:       value,
		required:    formHasAttribute(node, "required"),
		min:         formNumberAttribute(node, "min"),
		max:         formNumberAttribute(node, "max"),
		minLength:   formIntegerAttribute(node, "minlength"),
		maxLength:   formIntegerAttribute(node, "maxlength"),
	}
	field.pattern, _ = formAttribute(node, "pattern")
	form.fields = append(form.fields, field)
}

func formQuestion(node *xhtml.Node, name string) string {
	if question := formSectionTitle(node); question != "" {
		return question
	}
	if question := formPreviousText(node); question != "" {
		return question
	}
	return strings.NewReplacer("_", " ", "-", " ").Replace(name)
}

func formHeader(node *xhtml.Node, question string) string {
	title := formSectionTitle(node)
	if title == question {
		return ""
	}
	return title
}

func formTextQuestion(node *xhtml.Node, labels map[string]*xhtml.Node, name string) string {
	if question := formLabel(node, labels); question != "" {
		return question
	}
	return formQuestion(node, name)
}

func formSectionTitle(node *xhtml.Node) string {
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type != xhtml.ElementNode || !strings.EqualFold(parent.Data, "fieldset") {
			continue
		}
		for child := parent.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == xhtml.ElementNode && strings.EqualFold(child.Data, "legend") {
				return formVisibleText(child)
			}
		}
	}
	return ""
}

func formPreviousText(node *xhtml.Node) string {
	current := node
	for current.Parent != nil {
		if current.Parent.Type == xhtml.ElementNode && strings.EqualFold(current.Parent.Data, "label") {
			current = current.Parent
			continue
		}
		for sibling := current.PrevSibling; sibling != nil; sibling = sibling.PrevSibling {
			if containsFormControl(sibling) {
				continue
			}
			if text := formVisibleText(sibling); text != "" {
				return text
			}
		}
		current = current.Parent
		if strings.EqualFold(current.Data, "form") {
			break
		}
	}
	return ""
}

func containsFormControl(node *xhtml.Node) bool {
	found := false
	walkFormControls(node, func(*xhtml.Node) bool { found = true; return false })
	return found
}

func formLabel(node *xhtml.Node, labels map[string]*xhtml.Node) string {
	if id, ok := formAttribute(node, "id"); ok {
		if label := labels[id]; label != nil {
			return formVisibleText(label)
		}
	}
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type == xhtml.ElementNode && strings.EqualFold(parent.Data, "label") {
			return formVisibleText(parent)
		}
	}
	for _, key := range []string{"aria-label", "title"} {
		if text, ok := formAttribute(node, key); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func formVisibleText(node *xhtml.Node) string {
	var builder strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current.Type == xhtml.TextNode {
			builder.WriteString(current.Data)
			builder.WriteByte(' ')
			return
		}
		if current.Type == xhtml.ElementNode {
			switch strings.ToLower(current.Data) {
			case "script", "style", "template", "input", "select", "textarea", "button":
				return
			case "img":
				if alt, ok := formAttribute(current, "alt"); ok {
					builder.WriteString(alt)
					builder.WriteByte(' ')
				}
				return
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}

func formTextContent(node *xhtml.Node) string {
	var builder strings.Builder
	var walk func(*xhtml.Node)
	walk = func(current *xhtml.Node) {
		if current.Type == xhtml.TextNode {
			builder.WriteString(current.Data)
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return builder.String()
}

func formAttribute(node *xhtml.Node, key string) (string, bool) {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val, true
		}
	}
	return "", false
}

func formHasAttribute(node *xhtml.Node, key string) bool {
	_, ok := formAttribute(node, key)
	return ok
}

func formNumberAttribute(node *xhtml.Node, key string) *float64 {
	text, ok := formAttribute(node, key)
	if !ok {
		return nil
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return &value
}

func formIntegerAttribute(node *xhtml.Node, key string) int {
	text, ok := formAttribute(node, key)
	if !ok {
		return 0
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func askFormValidateText(field askFormField, value string) string {
	var empty bool
	if field.inputType == "email" {
		empty = strings.TrimFunc(value, askFormEmailWhitespace) == ""
	} else {
		empty = strings.TrimSpace(value) == ""
	}
	if empty {
		if field.required {
			return "required"
		}
		return ""
	}
	if field.inputType == "number" || field.inputType == "range" {
		number, err := strconv.ParseFloat(strings.TrimFunc(value, askFormEmailWhitespace), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return "number"
		}
		if field.min != nil && number < *field.min || field.max != nil && number > *field.max {
			return "range"
		}
	}
	if field.inputType == "url" {
		parsed, err := url.ParseRequestURI(value)
		if err != nil || parsed.Scheme == "" || (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
			return "url"
		}
	}
	length := utf16Length(value)
	if field.minLength > 0 && length < field.minLength || field.maxLength > 0 && length > field.maxLength {
		return "length"
	}
	if field.inputType == "email" {
		local, domain, _ := strings.Cut(value, "@")
		if local == "" || domain == "" || strings.ContainsRune(domain, '@') || strings.ContainsFunc(value, askFormEmailWhitespace) {
			return "email"
		}
	}
	if field.pattern != "" {
		pattern, err := regexp.Compile("^(?:" + field.pattern + ")$")
		if err != nil || !pattern.MatchString(value) {
			return "pattern"
		}
	}
	return ""
}

// OMP's email check uses JavaScript's whitespace class for both trimming and matching.
func askFormEmailWhitespace(r rune) bool {
	return r >= '\t' && r <= '\r' || unicode.Is(unicode.Z, r) || r == '\uFEFF'
}

func askFormTextValue(field askFormField, value string) any {
	if field.inputType == "number" || field.inputType == "range" {
		if strings.TrimSpace(value) != "" {
			if number, err := strconv.ParseFloat(strings.TrimFunc(value, askFormEmailWhitespace), 64); err == nil && !math.IsInf(number, 0) && !math.IsNaN(number) {
				return number
			}
		}
	}
	return value
}

func hasAskSelection(selected map[int]bool) bool {
	for _, checked := range selected {
		if checked {
			return true
		}
	}
	return false
}
