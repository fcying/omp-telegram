package bridge

import (
	"database/sql"
	"errors"
	"strings"

	"omp-telegram/internal/store"
)

// Query durable state instead of reading another worker's actor-owned migration.
func (b *Bridge) migrationBindingLocked(bot, chat, thread int64) bool {
	if b.db == nil {
		return false
	}
	_, err := b.db.Migration(bot, chat, thread)
	return !errors.Is(err, sql.ErrNoRows)
}

// Durable migration identities remain reserved even without a live worker. Callers
// hold sessionMu so native operations cannot acquire a competing reservation.
func (b *Bridge) migrationSessionStatusLocked(owner *worker, id string) (busy, elsewhere bool) {
	if id == "" {
		return false, false
	}
	for _, claim := range b.sessionClaims {
		if claim.migration && sessionIDsMatch(claim.id, id) {
			busy = true
			if claim.owner != owner {
				elsewhere = true
			}
		}
	}
	if b.db == nil {
		return busy, elsewhere
	}
	migrations, err := b.db.PendingMigrations(b.bot.ID)
	if err != nil {
		// A missing reservation must never turn a persistence failure into
		// permission to modify or delete a possibly migrating session.
		return true, true
	}
	for _, migration := range migrations {
		if (migration.Source.SessionID == "" || !sessionIDsMatch(migration.Source.SessionID, id)) &&
			(migration.CandidateID == "" || !sessionIDsMatch(migration.CandidateID, id)) {
			continue
		}
		busy = true
		if owner == nil || owner.key.chat != migration.Source.Chat || owner.key.thread != migration.Source.Thread {
			elsewhere = true
		}
	}
	return busy, elsewhere
}

// reserveMigrationCandidate preserves the source reservation while adding the
// candidate. It must not use claimSession, which replaces ordinary claims.
func (w *worker) reserveMigrationCandidate(file, id string) bool {
	if w.migration == nil || file == "" || !validSessionID(id) {
		return false
	}
	migration := *w.migration
	if migration.CandidateID != "" && (!strings.EqualFold(migration.CandidateID, id) || migration.CandidateFile != file) {
		return false
	}
	if sessionIDsMatch(migration.Source.SessionID, id) || migration.Source.Session == file {
		return false
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	migration.CandidateFile, migration.CandidateID = file, id
	return w.reserveMigrationClaimsLocked(migration)
}

func (w *worker) restoreMigrationClaims() bool {
	if w.migration == nil {
		return false
	}
	migration := *w.migration
	if (migration.CandidateFile == "") != (migration.CandidateID == "") {
		return false
	}
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	return w.reserveMigrationClaimsLocked(migration)
}

func (w *worker) reserveMigrationClaimsLocked(migration store.Migration) bool {
	if err := w.b.checkMigrationClaimsLocked(w, migration); err != nil {
		return false
	}
	w.b.installMigrationClaimsLocked(w, migration)
	w.claimedSession, w.sessionID = migration.Source.Session, migration.Source.SessionID
	return true
}

// observeMigrationCandidate survives cancellation and worker teardown: only
// immutable worker identity and sessionMu-protected reservations are accessed.
func (b *Bridge) observeMigrationCandidate(owner *worker, migrationID, file, id string) (store.Migration, error) {
	if owner == nil || owner.b != b || b.db == nil || file == "" || !validSessionID(id) {
		return store.Migration{}, errors.New("invalid migration candidate observation")
	}
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	migration, err := b.db.Migration(b.bot.ID, owner.key.chat, owner.key.thread)
	if err != nil {
		return store.Migration{}, err
	}
	if migration.ID != migrationID || migration.CandidateID != "" &&
		(migration.CandidateFile != file || !strings.EqualFold(migration.CandidateID, id)) {
		return store.Migration{}, store.ErrStaleMigration
	}
	if migration.CandidateID == "" {
		migration.CandidateFile, migration.CandidateID = file, id
	}
	// Reject ownership conflicts before persisting an otherwise irreversible
	// candidate identity; a rejected observation must not poison recovery.
	if err := b.checkMigrationClaimsLocked(owner, migration); err != nil {
		return store.Migration{}, err
	}
	if err := b.db.UpdateMigration(migration, migration.Phase); err != nil {
		return store.Migration{}, err
	}
	// Store may preserve a newer failure reason during the candidate write.
	migration, err = b.db.Migration(b.bot.ID, owner.key.chat, owner.key.thread)
	if err != nil {
		return store.Migration{}, err
	}
	if migration.ID != migrationID || migration.CandidateFile != file || !strings.EqualFold(migration.CandidateID, id) {
		return store.Migration{}, store.ErrStaleMigration
	}
	b.installMigrationClaimsLocked(owner, migration)
	return migration, nil
}

func (b *Bridge) checkMigrationClaimsLocked(owner *worker, migration store.Migration) error {
	sourceFile, sourceID := migration.Source.Session, migration.Source.SessionID
	candidateFile, candidateID := migration.CandidateFile, migration.CandidateID
	if sourceFile == "" || !validSessionID(sourceID) || (candidateFile == "") != (candidateID == "") ||
		candidateFile != "" && (!validSessionID(candidateID) || candidateFile == sourceFile || sessionIDsMatch(sourceID, candidateID)) {
		return errors.New("invalid migration session identities")
	}
	for path, claim := range b.sessionClaims {
		if claim.owner == owner && claim.migration && path != sourceFile &&
			(path != candidateFile || !strings.EqualFold(claim.id, candidateID)) {
			return store.ErrMigrationLocked
		}
	}
	available := func(file, id string) bool {
		if file == "" {
			return true
		}
		if b.exportMatchesLocked(nil, id) || b.deleteMatchesLocked(id) {
			return false
		}
		for path, claim := range b.sessionClaims {
			if claim.owner != owner && (path == file || sessionIDsMatch(claim.id, id)) {
				return false
			}
		}
		return true
	}
	if !available(sourceFile, sourceID) || !available(candidateFile, candidateID) {
		return store.ErrMigrationLocked
	}
	if b.db != nil {
		migrations, err := b.db.PendingMigrations(b.bot.ID)
		if err != nil {
			return err
		}
		for _, pending := range migrations {
			if pending.ID == migration.ID {
				continue
			}
			for _, identity := range []struct{ file, id string }{
				{pending.Source.Session, pending.Source.SessionID},
				{pending.CandidateFile, pending.CandidateID},
			} {
				if identity.id != "" && (sessionIDsMatch(identity.id, sourceID) || candidateID != "" && sessionIDsMatch(identity.id, candidateID)) ||
					identity.file != "" && (identity.file == sourceFile || identity.file == candidateFile) {
					return store.ErrMigrationLocked
				}
			}
		}
	}
	return nil
}

func (b *Bridge) installMigrationClaimsLocked(owner *worker, migration store.Migration) {
	if b.sessionClaims == nil {
		b.sessionClaims = make(map[string]sessionClaim)
	}
	b.sessionClaims[migration.Source.Session] = sessionClaim{owner: owner, id: migration.Source.SessionID, migration: true}
	if migration.CandidateFile != "" {
		b.sessionClaims[migration.CandidateFile] = sessionClaim{owner: owner, id: migration.CandidateID, migration: true}
	}
}

// finishMigrationClaims is called only after the durable migration transaction
// succeeds. The selected target must already be one of this worker's reservations.
func (w *worker) finishMigrationClaims(targetFile, targetID string) bool {
	b := w.b
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	claim, exists := b.sessionClaims[targetFile]
	if !exists || claim.owner != w || !claim.migration || !strings.EqualFold(claim.id, targetID) {
		return false
	}
	for path, owned := range b.sessionClaims {
		if owned.owner == w {
			delete(b.sessionClaims, path)
		}
	}
	b.sessionClaims[targetFile] = sessionClaim{owner: w, id: targetID}
	w.claimedSession, w.sessionID = targetFile, targetID
	return true
}
