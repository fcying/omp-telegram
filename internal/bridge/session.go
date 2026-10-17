package bridge

import (
	"strings"
)

type sessionClaim struct {
	owner *worker
	id    string
}

func validSessionID(id string) bool {
	if len(id) < 8 || len(id) > 64 || strings.HasPrefix(id, "-") || strings.HasSuffix(id, "-") {
		return false
	}
	for _, r := range id {
		if r != '-' && !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func (b *Bridge) sessionInUse(id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.sessionMatchesLocked(nil, id) || b.exportMatchesLocked(nil, id) || b.deleteMatchesLocked(id)
}

func (b *Bridge) sessionInUseByOther(owner *worker, id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.sessionMatchesLocked(owner, id) || b.exportMatchesLocked(owner, id) || b.deleteMatchesLocked(id)
}

func sessionIDsMatch(left, right string) bool {
	left, right = strings.ToLower(left), strings.ToLower(right)
	return left == right || strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

func (b *Bridge) sessionMatchesLocked(except *worker, id string) bool {
	for _, claim := range b.sessionClaims {
		if claim.owner != except && sessionIDsMatch(claim.id, id) {
			return true
		}
	}
	return false
}

func (b *Bridge) exportMatchesLocked(except *worker, id string) bool {
	for owner, claimedID := range b.exportClaims {
		if owner != except && sessionIDsMatch(claimedID, id) {
			return true
		}
	}
	return false
}
func (b *Bridge) deleteMatchesLocked(id string) bool {
	for _, claimedID := range b.deleteClaims {
		if sessionIDsMatch(claimedID, id) {
			return true
		}
	}
	return false
}

// A released logical claim does not block deletion; live runtimes and in-flight operations do.
func (b *Bridge) sessionDeleteStatus(owner *worker, id string) (busy, elsewhere bool) {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.sessionDeleteStatusLocked(owner, id)
}

func (b *Bridge) sessionDeleteStatusLocked(owner *worker, id string) (busy, elsewhere bool) {
	for _, claim := range b.sessionClaims {
		if claim.owner == nil || !sessionIDsMatch(claim.id, id) {
			continue
		}
		if !claim.owner.sessionRuntimeActive.Load() && !claim.owner.sessionRuntimeStarting.Load() {
			continue
		}
		busy = true
		if claim.owner != owner {
			elsewhere = true
		}
	}
	for claimOwner, claimedID := range b.exportClaims {
		if sessionIDsMatch(claimedID, id) {
			busy = true
			if claimOwner != owner {
				elsewhere = true
			}
		}
	}
	for claimOwner, claimedID := range b.deleteClaims {
		if sessionIDsMatch(claimedID, id) {
			busy = true
			if claimOwner != owner {
				elsewhere = true
			}
		}
	}
	return busy, elsewhere
}
func (b *Bridge) reserveDelete(owner *worker, id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if busy, _ := b.sessionDeleteStatusLocked(nil, id); busy {
		return false
	}
	if b.deleteClaims == nil {
		b.deleteClaims = make(map[*worker]string)
	}
	if _, exists := b.deleteClaims[owner]; exists {
		return false
	}
	b.deleteClaims[owner] = id
	return true
}

func (b *Bridge) releaseDelete(owner *worker) {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	delete(b.deleteClaims, owner)
}

func (b *Bridge) reserveExport(owner *worker, id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if b.sessionMatchesLocked(owner, id) || b.exportMatchesLocked(owner, id) || b.deleteMatchesLocked(id) {
		return false
	}
	if b.exportClaims == nil {
		b.exportClaims = make(map[*worker]string)
	}
	if _, exists := b.exportClaims[owner]; exists {
		return false
	}
	b.exportClaims[owner] = id
	return true
}

func (b *Bridge) releaseExport(owner *worker) {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	delete(b.exportClaims, owner)
}

func (w *worker) claimPersistedSession() bool {
	if w.binding.Session == "" || !validSessionID(w.binding.SessionID) {
		return false
	}
	return w.claimSession(w.binding.Session, w.binding.SessionID)
}

func (w *worker) beginSessionRestore(id string) bool {
	if !validSessionID(id) {
		return false
	}
	b := w.b
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if b.deleteMatchesLocked(id) || b.sessionMatchesLocked(w, id) || b.exportMatchesLocked(w, id) {
		return false
	}
	current, err := b.db.Binding(b.bot.ID, w.key.chat, w.key.thread)
	if err != nil || current.Generation != w.binding.Generation || current.Workspace != w.binding.Workspace ||
		current.Session != w.binding.Session || current.Running != w.binding.Running || !strings.EqualFold(current.SessionID, id) {
		return false
	}
	if b.sessionClaims == nil {
		b.sessionClaims = make(map[string]sessionClaim)
	}
	claimPath := ""
	for path, claim := range b.sessionClaims {
		if claim.owner != w {
			continue
		}
		if !strings.EqualFold(claim.id, id) {
			return false
		}
		claimPath = path
		break
	}
	if claimPath == "" {
		claimPath = w.binding.Session
		if claimPath == "" {
			return false
		}
		if claim, exists := b.sessionClaims[claimPath]; exists && claim.owner != w {
			return false
		}
		b.sessionClaims[claimPath] = sessionClaim{owner: w, id: id}
	}
	w.claimedSession = claimPath
	w.sessionID = id
	w.sessionRuntimeStarting.Store(true)
	return true
}

func (w *worker) claimSession(file, id string) bool {
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	if w.b.sessionClaims == nil {
		w.b.sessionClaims = make(map[string]sessionClaim)
	}
	if w.b.exportMatchesLocked(w, id) || w.b.deleteMatchesLocked(id) {
		return false
	}
	if claim, exists := w.b.sessionClaims[file]; exists && claim.owner != w {
		return false
	}
	for _, claim := range w.b.sessionClaims {
		if claim.owner != w && strings.EqualFold(claim.id, id) {
			return false
		}
	}
	for path, claim := range w.b.sessionClaims {
		if claim.owner == w {
			delete(w.b.sessionClaims, path)
		}
	}
	w.b.sessionClaims[file] = sessionClaim{owner: w, id: id}
	w.claimedSession = file
	w.sessionID = id
	return true
}

func (w *worker) releaseSession() {
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	for path, claim := range w.b.sessionClaims {
		if claim.owner == w {
			delete(w.b.sessionClaims, path)
		}
	}
	w.claimedSession = ""
	w.sessionID = ""
	w.sessionRuntimeStarting.Store(false)
	w.sessionRuntimeActive.Store(false)
}
