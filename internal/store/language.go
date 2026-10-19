package store

import (
	"database/sql"
	"errors"

	"omp-telegram/internal/i18n"
)

func (s *Store) Language() (i18n.Language, error) {
	var language string
	err := s.DB.QueryRow("SELECT language FROM interface_language WHERE id=1").Scan(&language)
	if errors.Is(err, sql.ErrNoRows) {
		return i18n.English, nil
	}
	if err != nil {
		return "", err
	}
	parsed, ok := i18n.Parse(language)
	if !ok {
		return "", errors.New("invalid stored interface language")
	}
	return parsed, nil
}

func (s *Store) SetLanguage(language i18n.Language) error {
	if _, ok := i18n.Parse(string(language)); !ok {
		return errors.New("unsupported interface language")
	}
	_, err := s.DB.Exec(`INSERT INTO interface_language(id,language) VALUES(1,?)
ON CONFLICT(id) DO UPDATE SET language=excluded.language`, language)
	return err
}
