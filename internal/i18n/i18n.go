package i18n

import "fmt"

// Language selects Bridge-owned interface text, not model or native OMP output.
type Language string

const (
	English Language = "en"
	Chinese Language = "zh"
)

// Message keeps both interface languages together without a string lookup key.
type Message struct {
	en string
	zh string
}

func message(en, zh string) Message {
	if en == "" || zh == "" {
		panic("i18n message requires both English and Chinese text")
	}
	return Message{en: en, zh: zh}
}

// Parse accepts only the explicit language codes supported by /lang.
func Parse(value string) (Language, bool) {
	switch value {
	case "en":
		return English, true
	case "zh":
		return Chinese, true
	default:
		return English, false
	}
}

// Text translates a template before interpolating opaque caller-owned values.
// Empty language values use English.
func Text(language Language, msg Message, args ...any) string {
	source := msg.en
	if language == Chinese {
		source = msg.zh
	}
	if len(args) == 0 {
		return source
	}
	return fmt.Sprintf(source, args...)
}
