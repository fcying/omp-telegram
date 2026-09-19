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
	return b.sessionMatchesLocked(nil, id)
}

func (b *Bridge) sessionInUseByOther(owner *worker, id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	return b.sessionMatchesLocked(owner, id)
}

func (b *Bridge) sessionMatchesLocked(except *worker, id string) bool {
	id = strings.ToLower(id)
	for _, claim := range b.sessionClaims {
		if claim.owner != except && strings.HasPrefix(strings.ToLower(claim.id), id) {
			return true
		}
	}
	return false
}

func (w *worker) claimPersistedSession() bool {
	if w.binding.Session == "" || !validSessionID(w.binding.SessionID) {
		return false
	}
	return w.claimSession(w.binding.Session, w.binding.SessionID)
}

func (w *worker) claimSession(file, id string) bool {
	w.b.sessionMu.Lock()
	defer w.b.sessionMu.Unlock()
	if w.b.sessionClaims == nil {
		w.b.sessionClaims = make(map[string]sessionClaim)
	}
	if claim, exists := w.b.sessionClaims[file]; exists && claim.owner != w {
		return false
	}
	for _, claim := range w.b.sessionClaims {
		if claim.owner != w && strings.EqualFold(claim.id, id) {
			return false
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
	if claim, exists := w.b.sessionClaims[w.claimedSession]; exists && claim.owner == w {
		delete(w.b.sessionClaims, w.claimedSession)
	}
	w.claimedSession = ""
	w.sessionID = ""
}
