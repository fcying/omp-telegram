package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"omp-telegram/internal/i18n"
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
	workspace              string
	status                 statusState
	account                [32]byte
	git                    string
	cancel                 context.CancelFunc
}

type researchSettlementState struct {
	Enabled bool `json:"enabled"`
	Idle    bool `json:"idle"`
	Settled bool `json:"settled"`
	Pending bool `json:"pending"`
}

func (w *worker) invalidateResearchSettlement() {
	if probe := w.research.settlement.probe; probe != nil && probe.cancel != nil {
		probe.cancel()
	}
	w.research.settlement.revision++
	w.research.settlement.probe = nil
}

func (w *worker) probeResearchSettlement() {
	// Deferred guidance still belongs to this root before native submission, so
	// an older off snapshot cannot retire it or start another settlement probe.
	if !w.research.root || !w.taskActive() || w.research.roundOpen || w.research.disablePending || w.client == nil || w.research.settlement.probe != nil || len(w.steers) != 0 || len(w.deferredSteers) != 0 {
		return
	}
	client := w.client
	catalog := client.CommandCatalog()
	ctx, cancel := context.WithCancel(w.ctx)
	probe := &researchSettlementProbe{
		client: client, generation: w.binding.Generation, root: w.active,
		turn: w.turn, revision: w.research.settlement.revision,
		admission: w.steerFence.admission, epoch: catalog.Epoch, catalogRevision: catalog.Revision,
		sessionID: catalog.SessionID, rootRequest: w.rootRequestID,
		workspace: w.binding.Workspace,
		cancel:    cancel,
	}
	w.research.settlement.probe = probe
	git := w.b.cfg.StatusBar.Has("final", "git")
	quota := w.b.cfg.StatusBar.Has("final", "quota")
	var environment []string
	if git {
		environment = omp.ChildEnv(w.b.cfg.OMPEnvironment)
	}
	w.startOperation("research_settlement", client, func(_ context.Context) (json.RawMessage, error) {
		defer cancel()
		nativeCtx, nativeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer nativeCancel()
		enabled, err := client.AutoresearchMode(nativeCtx)
		if err != nil {
			return nil, err
		}
		state := researchSettlementState{Enabled: enabled}
		if !enabled {
			raw, err := client.Call(nativeCtx, "get_state", nil)
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
			if state.Idle && state.Settled && !state.Pending {
				// Reuse the authoritative final read before accepting settlement,
				// rather than opening another asynchronous completion window.
				var status statusState
				if json.Unmarshal(raw, &status) == nil && status.SessionID == probe.sessionID {
					probe.status = status
					if quota {
						probe.account = w.readProviderUsageScope(nativeCtx, client, status)
					}
				}
				if git {
					probe.git = readReplyGitField(ctx, probe.workspace, environment, w.b.cfg.StatusBar.GitCommand, w.b.cfg.StatusBar.GitTimeout)
				}
			}
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
	if !w.research.root || !w.taskActive() || w.research.disablePending || w.client != probe.client || w.binding.Generation != probe.generation || w.active != probe.root || w.turn != probe.turn || w.rootRequestID != probe.rootRequest || w.research.settlement.revision != probe.revision || w.steerFence.admission != probe.admission || w.research.roundOpen || len(w.steers) != 0 || len(w.deferredSteers) != 0 {
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
	if w.client != probe.client || !w.research.root || w.active != probe.root || w.turn != probe.turn || w.binding.Generation != probe.generation || w.rootRequestID != probe.rootRequest || w.research.settlement.revision != probe.revision || w.steerFence.admission != probe.admission || w.research.roundOpen || len(w.steers) != 0 || len(w.deferredSteers) != 0 {
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
		w.retireResearch(w.tr(i18n.Msg.AutoresearchSettlementCouldNotConfirmedTaskOutcomeUncertainWill))
		return
	}
	w.research.known, w.research.enabled = true, state.Enabled
	if state.Enabled {
		return
	}
	if !state.Idle || !state.Settled || state.Pending {
		return
	}
	// flushResearchRound already committed each round's replies. Finish only
	// the root's ownership; copying the final native messages would duplicate it.
	probe.status.ConfiguredThinking = w.configuredThinking
	fields := compactFields(w.b.language(), probe.status, &w.replyStatus.roundMetrics, false, true)
	if probe.git != "" && probe.workspace == w.binding.Workspace {
		fields.git = progressToolLiteral(probe.git)
	}
	w.setProviderUsageScope(probe.status.Model.Provider, probe.account)
	w.prepareReplyStatus(false, fields, true)
	w.commitReplyStatus(false)
	w.refreshProviderUsage(time.Now())
}
