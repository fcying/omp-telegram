package bridge

import (
	"errors"

	"omp-telegram/internal/i18n"
)

func (b *Bridge) language() i18n.Language {
	if b.interfaceLanguage.Load() == 1 {
		return i18n.Chinese
	}
	return i18n.English
}

func (b *Bridge) setLanguage(language i18n.Language) {
	if language == i18n.Chinese {
		b.interfaceLanguage.Store(1)
	} else {
		b.interfaceLanguage.Store(0)
	}
}

func (b *Bridge) tr(source i18n.Message, args ...any) string {
	return i18n.Text(b.language(), source, args...)
}

func (w *worker) tr(source i18n.Message, args ...any) string {
	return w.b.tr(source, args...)
}

type localizedMessageError struct {
	message i18n.Message
}

func (e *localizedMessageError) Error() string {
	return i18n.Text(i18n.English, e.message)
}

func newMessageError(message i18n.Message) error {
	return &localizedMessageError{message: message}
}

func (w *worker) errorText(err error) string {
	var localized *localizedMessageError
	if errors.As(err, &localized) {
		return w.tr(localized.message)
	}
	return err.Error()
}

func (b *Bridge) initializeLanguage() error {
	language, err := b.db.Language()
	if err != nil {
		return err
	}
	b.setLanguage(language)
	return nil
}

// Serialize persistence and publication so concurrent commands cannot invert them.
func (b *Bridge) changeLanguage(language i18n.Language) error {
	b.languageMu.Lock()
	defer b.languageMu.Unlock()
	if err := b.db.SetLanguage(language); err != nil {
		return err
	}
	b.setLanguage(language)
	b.refreshLanguageMenus()
	return nil
}

func (w *worker) handleLanguageCommand(arg string) {
	if arg == "" {
		name := "English"
		if w.b.language() == i18n.Chinese {
			name = "中文"
		}
		w.say(w.tr(i18n.Msg.Language.Current, name))
		return
	}
	language, ok := i18n.Parse(arg)
	if !ok {
		w.say(w.tr(i18n.Msg.Language.Usage))
		return
	}
	if err := w.b.changeLanguage(language); err != nil {
		w.b.storeLog.Error("interface language write failed", "event", "language_write_failed", "error_kind", "persistence")
		w.say(w.tr(i18n.Msg.Language.SaveFailed))
		return
	}
	w.say(i18n.Text(language, i18n.Msg.Language.Changed))
}

func (w *worker) commandHelp() string {
	return commandHelp(w.b.version, w.b.language())
}
