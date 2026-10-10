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

func (b *Bridge) reserveDelete(owner *worker, id string) bool {
	b.sessionMu.Lock()
	defer b.sessionMu.Unlock()
	if b.sessionMatchesLocked(nil, id) || b.exportMatchesLocked(nil, id) || b.deleteMatchesLocked(id) {
		return false
	}
	if b.db != nil {
		leased, err := b.db.ResearchSessionLeased(b.bot.ID, id)
		if err != nil || leased {
			return false
		}
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
	// Restoring durable identity does not authorize executing workspace tasks.
	// Conflicting or unavailable workspaces must not stop unrelated recovery.
	return w.claimSession(w.binding.Session, w.binding.SessionID)
}

func (w *worker) claimSession(file, id string) bool {
	return w.claimSessionWorkspace(file, id, "")
}

func (w *worker) claimSessionWorkspace(file, id, workspace string) bool {
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
	if workspace != "" {
		root, err := w.b.refreshWorkspaceRootLocked(w.ctx, workspace)
		if err != nil {
			return false
		}
		owner := w.workspaceOwner(id)
		if w.b.db.CheckWorkspaceSession(owner) != nil || w.b.db.AdmitWorkspace(root, owner) != nil {
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
