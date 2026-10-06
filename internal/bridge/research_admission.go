package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"omp-telegram/internal/omp"
)

// Admission evidence belongs to one queue head, not to the session's next task.
// RPC metadata lookup runs off the actor so controls can cancel pending work.
type researchAdmission struct {
	client                *omp.Client
	generation, inbox     int64
	turn, epoch, revision uint64
	sessionID             string
	cancel                context.CancelFunc
	ready, enabled        bool
}

func (w *worker) invalidateResearchAdmission() {
	if probe := w.research.admission; probe != nil {
		probe.cancel()
		w.research.admission = nil
	}
}

func (w *worker) researchAdmissionReady(client *omp.Client) bool {
	probe := w.research.admission
	if probe != nil && (probe.client != client || probe.generation != w.binding.Generation || probe.turn != w.turn || probe.inbox != w.queue[0].id) {
		w.invalidateResearchAdmission()
		probe = nil
	}
	catalog := client.CommandCatalog()
	if probe != nil && probe.ready {
		if catalog.Epoch != probe.epoch || probe.sessionID != "" && catalog.SessionID != probe.sessionID {
			w.research.known = false
			w.cancelQueuedTask(probe.inbox)
			w.researchModeFailure()
			return false
		}
		if catalog.Revision != probe.revision {
			w.invalidateResearchAdmission()
			probe = nil
		}
	}
	if probe == nil {
		ctx, cancel := context.WithCancel(w.ctx)
		probe = &researchAdmission{
			client: client, generation: w.binding.Generation, inbox: w.queue[0].id,
			turn: w.turn, epoch: catalog.Epoch, revision: catalog.Revision,
			sessionID: catalog.SessionID, cancel: cancel,
		}
		w.research.admission = probe
		w.startOperation("research_admission", client, func(context.Context) (json.RawMessage, error) {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			enabled, err := client.AutoresearchMode(ctx)
			if err != nil {
				return nil, err
			}
			return json.Marshal(enabled)
		}, 0, "", probe)
		return false
	}
	if !probe.ready {
		return false
	}
	w.rememberResearchMode(client, probe.enabled)
	return true
}

func (w *worker) researchAdmissionFinished(result operationResult) {
	probe, ok := result.meta.(*researchAdmission)
	if !ok || w.research.admission != probe {
		return
	}
	probe.cancel()
	if w.client != probe.client || w.binding.Generation != probe.generation || w.turn != probe.turn || len(w.queue) == 0 || w.queue[0].id != probe.inbox {
		w.invalidateResearchAdmission()
		return
	}
	catalog := probe.client.CommandCatalog()
	if result.err == nil && (catalog.Epoch != probe.epoch || probe.sessionID != "" && catalog.SessionID != probe.sessionID) {
		result.err = errors.New("autoresearch session changed during admission lookup")
	}
	if result.err == nil && catalog.Revision != probe.revision {
		w.invalidateResearchAdmission()
		return
	}
	if result.err == nil {
		result.err = json.Unmarshal(result.data, &probe.enabled)
	}
	if result.err == nil && probe.enabled {
		result.err = w.ensureResearchWorkspace()
	}
	if result.err != nil {
		w.research.known = false
		w.cancelQueuedTask(probe.inbox)
		w.researchModeFailure()
		return
	}
	probe.ready = true
}
