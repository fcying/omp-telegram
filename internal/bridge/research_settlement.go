package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"omp-telegram/internal/omp"
)

// A settlement notification is only a reason to query native state. Neither a
// round boundary nor a cached mode value is terminal evidence for the root.
type researchSettlement struct {
	revision uint64
	probe    *researchSettlementProbe
}

type researchSettlementProbe struct {
	client                 *omp.Client
	generation, root       int64
	turn, revision         uint64
	admission              uint64
	epoch, catalogRevision uint64
	sessionID, rootRequest string
}

type researchSettlementState struct {
	Enabled bool `json:"enabled"`
	Idle    bool `json:"idle"`
	Settled bool `json:"settled"`
	Pending bool `json:"pending"`
}

func (w *worker) invalidateResearchSettlement() {
	w.research.settlement.revision++
	w.research.settlement.probe = nil
}

func (w *worker) probeResearchSettlement() {
	if !w.research.root || !w.taskActive() || w.research.roundOpen || w.research.disablePending || w.client == nil || w.research.settlement.probe != nil || len(w.steers) != 0 {
		return
	}
	client := w.client
	catalog := client.CommandCatalog()
	probe := &researchSettlementProbe{
		client: client, generation: w.binding.Generation, root: w.active,
		turn: w.turn, revision: w.research.settlement.revision,
		admission: w.steerFence.admission, epoch: catalog.Epoch, catalogRevision: catalog.Revision,
		sessionID: catalog.SessionID, rootRequest: w.rootRequestID,
	}
	w.research.settlement.probe = probe
	w.startOperation("research_settlement", client, func(ctx context.Context) (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		enabled, err := client.AutoresearchMode(ctx)
		if err != nil {
			return nil, err
		}
		state := researchSettlementState{Enabled: enabled}
		if !enabled {
			raw, err := client.Call(ctx, "get_state", nil)
			if err != nil {
				return nil, err
			}
			// Missing pending-work fields cannot prove that no native work is
			// outstanding. Unlike a general idle hint, completion fails closed.
			var native struct {
				Streaming  *bool `json:"isStreaming"`
				Compacting *bool `json:"isCompacting"`
				Settled    *bool `json:"isSettled"`
				Async      *bool `json:"hasPendingAsyncWork"`
				Queued     *int  `json:"queuedMessageCount"`
			}
			if json.Unmarshal(raw, &native) != nil || native.Streaming == nil || native.Compacting == nil || native.Settled == nil || native.Async == nil || native.Queued == nil || *native.Queued < 0 {
				return nil, errors.New("native research settlement state is incomplete")
			}
			state.Idle = !*native.Streaming && !*native.Compacting
			state.Settled = *native.Settled
			state.Pending = *native.Async || *native.Queued != 0
		}
		current := client.CommandCatalog()
		if current.Epoch != probe.epoch || current.Revision != probe.catalogRevision || current.SessionID != probe.sessionID {
			return nil, errors.New("autoresearch catalog changed during settlement lookup")
		}
		return json.Marshal(state)
	}, w.active, "", probe)
}

func (w *worker) researchSettlementFinished(result operationResult) {
	probe, ok := result.meta.(*researchSettlementProbe)
	if !ok || w.research.settlement.probe != probe {
		return
	}
	w.research.settlement.probe = nil
	if !w.research.root || !w.taskActive() || w.research.disablePending || w.client != probe.client || w.binding.Generation != probe.generation || w.active != probe.root || w.turn != probe.turn || w.rootRequestID != probe.rootRequest || w.research.settlement.revision != probe.revision || w.steerFence.admission != probe.admission || w.research.roundOpen || len(w.steers) != 0 {
		return
	}
	// Reader delivery and RPC results are separate actor inputs. Process queued
	// agent_start / steer results before accepting an off snapshot. Byte credit
	// also covers events the reader has not handed to the actor yet.
	for {
		select {
		case raw, open := <-probe.client.Events():
			if !open {
				w.failed()
				return
			}
			w.event(raw)
		default:
			goto drained
		}
	}

drained:
	if w.client != probe.client || !w.research.root || w.active != probe.root || w.turn != probe.turn || w.binding.Generation != probe.generation || w.rootRequestID != probe.rootRequest || w.research.settlement.revision != probe.revision || w.steerFence.admission != probe.admission || w.research.roundOpen || len(w.steers) != 0 {
		return
	}
	if probe.client.BufferedOutput() {
		w.probeResearchSettlement()
		return
	}
	catalog := probe.client.CommandCatalog()
	if catalog.Epoch != probe.epoch || catalog.Revision != probe.catalogRevision || catalog.SessionID != probe.sessionID {
		w.probeResearchSettlement()
		return
	}
	var state researchSettlementState
	if result.err != nil || json.Unmarshal(result.data, &state) != nil {
		w.retireResearch("Autoresearch settlement could not be confirmed. The task outcome is uncertain and will not be replayed automatically.")
		return
	}
	w.research.known, w.research.enabled = true, state.Enabled
	if state.Enabled {
		if err := w.ensureResearchWorkspace(); err != nil {
			w.retireResearch("Autoresearch workspace ownership could not be confirmed. The task outcome is uncertain and will not be replayed automatically.")
		}
		return
	}
	if !state.Idle || !state.Settled || state.Pending {
		return
	}
	// flushResearchRound already committed each round's replies. Finish only
	// the root's ownership; copying the final native messages would duplicate it.
	w.finish()
	if w.taskActive() || w.ctx.Err() != nil {
		return
	}
	w.invalidateResearchSettlement()
	w.clearSteerFence()
	w.releaseConfirmedResearchWorkspace()
}

func (w *worker) releaseConfirmedResearchWorkspace() bool {
	if err := w.releaseResearchWorkspace(); err != nil {
		w.resumeFailed = true
		w.say("Autoresearch workspace ownership could not be released. Queued work is paused: " + err.Error())
		return false
	}
	return true
}
