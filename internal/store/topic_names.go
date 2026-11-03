package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TopicNameReservation identifies one rename request, independently of known titles.
type TopicNameReservation struct {
	Name     string
	Revision int64
}

// ReserveTopicName atomically selects a title not known or reserved by another topic.
func (s *Store) ReserveTopicName(bot, chat, thread int64, base string) (TopicNameReservation, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return TopicNameReservation{}, err
	}
	defer tx.Rollback()

	var r TopicNameReservation
	err = tx.QueryRow("SELECT revision FROM topic_names WHERE bot=? AND chat=? AND thread=?", bot, chat, thread).Scan(&r.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return TopicNameReservation{}, err
	}
	r.Name = truncateTopicName(base, 128)
	for attempt := 1; ; attempt++ {
		var occupied bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM topic_names WHERE bot=? AND chat=? AND thread<>? AND name=?)
OR EXISTS(SELECT 1 FROM topic_name_reservations WHERE bot=? AND chat=? AND thread<>? AND name=?)`, bot, chat, thread, r.Name, bot, chat, thread, r.Name).Scan(&occupied); err != nil {
			return TopicNameReservation{}, err
		}
		if !occupied {
			break
		}
		var suffix string
		if attempt == 1 {
			suffix = fmt.Sprintf(" [%d]", thread)
		} else {
			suffix = fmt.Sprintf(" [%d-%d]", thread, attempt)
		}
		r.Name = truncateTopicName(base, 128-len(suffix)) + suffix
	}
	r.Revision++
	if _, err := tx.Exec(`INSERT INTO topic_names(bot,chat,thread,name,revision) VALUES(?,?,?,'',?)
ON CONFLICT(bot,chat,thread) DO UPDATE SET revision=excluded.revision`, bot, chat, thread, r.Revision); err != nil {
		return TopicNameReservation{}, err
	}
	if _, err := tx.Exec("INSERT INTO topic_name_reservations(bot,chat,thread,name,revision,active) VALUES(?,?,?,?,?,1)", bot, chat, thread, r.Name, r.Revision); err != nil {
		return TopicNameReservation{}, err
	}
	if err := tx.Commit(); err != nil {
		return TopicNameReservation{}, err
	}
	return r, nil
}

// ObserveTopicName records a service event without freeing in-flight request names.
func (s *Store) ObserveTopicName(bot, chat, thread int64, name string) error {
	if name == "" {
		return nil
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO topic_names(bot,chat,thread,name,revision) VALUES(?,?,?,?,1)
ON CONFLICT(bot,chat,thread) DO UPDATE SET name=excluded.name,revision=topic_names.revision+1`, bot, chat, thread, name); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM topic_name_reservations WHERE bot=? AND chat=? AND thread=? AND active=0", bot, chat, thread); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfirmTopicName settles only the matching active request, preserving newer titles.
func (s *Store) ConfirmTopicName(bot, chat, thread int64, r TopicNameReservation) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var name string
	var revision int64
	err = tx.QueryRow(`SELECT n.name,n.revision FROM topic_names n
JOIN topic_name_reservations r USING(bot,chat,thread)
WHERE r.bot=? AND r.chat=? AND r.thread=? AND r.name=? AND r.revision=? AND r.active=1`, bot, chat, thread, r.Name, r.Revision).Scan(&name, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if revision == r.Revision {
		if _, err := tx.Exec("UPDATE topic_names SET name=?,revision=revision+1 WHERE bot=? AND chat=? AND thread=?", r.Name, bot, chat, thread); err != nil {
			return err
		}
	} else if name != r.Name {
		// A newer title cannot establish which request Telegram applied last.
		if _, err := tx.Exec("UPDATE topic_name_reservations SET active=0 WHERE bot=? AND chat=? AND thread=? AND name=? AND revision=? AND active=1", bot, chat, thread, r.Name, r.Revision); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.Exec("DELETE FROM topic_name_reservations WHERE bot=? AND chat=? AND thread=? AND name=? AND revision=? AND active=1", bot, chat, thread, r.Name, r.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseTopicName frees only a matching active request after explicit rejection.
func (s *Store) ReleaseTopicName(bot, chat, thread int64, r TopicNameReservation) error {
	// Keep the known row, including empty tombstones, so tokens cannot be reused.
	_, err := s.DB.Exec("DELETE FROM topic_name_reservations WHERE bot=? AND chat=? AND thread=? AND name=? AND revision=? AND active=1", bot, chat, thread, r.Name, r.Revision)
	return err
}

// RetainTopicName keeps an uncertain request occupied until a later observation.
func (s *Store) RetainTopicName(bot, chat, thread int64, r TopicNameReservation) error {
	_, err := s.DB.Exec("UPDATE topic_name_reservations SET active=0 WHERE bot=? AND chat=? AND thread=? AND name=? AND revision=? AND active=1", bot, chat, thread, r.Name, r.Revision)
	return err
}

func truncateTopicName(name string, units int) string {
	for i, r := range name {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units < width {
			return name[:i]
		}
		units -= width
	}
	return name
}
