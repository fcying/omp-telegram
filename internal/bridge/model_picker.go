package bridge

import (
	"context"
	"encoding/json"

	"omp-telegram/internal/omp"
)

type modelSettings struct {
	Model           omp.Model `json:"model"`
	ThinkingLevel   string    `json:"thinkingLevel"`
	IsStreaming     bool      `json:"isStreaming"`
	IsCompacting    bool      `json:"isCompacting"`
	FastModeEnabled *bool     `json:"fastModeEnabled"`
	FastModeActive  *bool     `json:"fastModeActive"`
}

type modelOperationRequest struct {
	action    string
	user      int64
	role      string
	provider  string
	modelID   string
	requested string
	enabled   bool
	current   omp.Model
}

type modelFastResult struct {
	Enabled *bool `json:"enabled"`
	Active  *bool `json:"active"`
}

func (w *worker) beginModelState(action string, request modelOperationRequest) bool {
	allowBusy := action == "fast_status"
	if w.controlBusy {
		w.say("The session operation is still loading.")
		return false
	}
	if !allowBusy && (w.sessionControlBusy() || len(w.queue) != 0) {
		w.say("Wait for the current task and queue to finish before changing model settings.")
		return false
	}
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(err.Error())
		return false
	}
	w.controlBusy = true
	w.startOperation("model_state", client, func(ctx context.Context) (json.RawMessage, error) {
		return client.Call(ctx, "get_state", nil)
	}, 0, "", request)
	return true
}

func modelOperationFailure(action string) string {
	switch action {
	case "model_select", "model_switch":
		return "The model switch could not be confirmed. Use /status to check the actual model before retrying."
	case "thinking_select", "thinking_picker":
		return "The thinking level change could not be confirmed. Open /thinking to check before retrying."
	case "fast_select", "fast_picker":
		return "The fast mode change could not be confirmed. It may be unsupported by this model. Use /fast status to check before retrying."
	case "fast_status":
		return "Failed to read fast mode state."
	default:
		return "Failed to read model settings."
	}
}

func (w *worker) modelOperationFinished(result operationResult) {
	request, ok := result.meta.(modelOperationRequest)
	if !ok {
		w.controlBusy = false
		return
	}
	if result.err != nil || result.cancelled {
		switch result.kind {
		case "model_set_role":
			// SetModelRole fails closed when any native selection step is unconfirmed.
			w.releaseRuntimeWithReason(true, "failure")
		case "model_verify_thinking":
			w.releaseRuntimeWithReason(true, "failure")
		case "model_set_model", "model_set_thinking", "model_set_fast":
			if uncertainOperationOutcome(result.err, result.cancelled) {
				w.releaseRuntimeWithReason(true, "failure")
			}
		}
		w.controlBusy = false
		w.say(modelOperationFailure(request.action))
		return
	}
	client, connected := w.runtimeClient()
	if !connected || client.ID() != result.clientID {
		w.controlBusy = false
		return
	}
	switch result.kind {
	case "model_state":
		var state modelSettings
		if json.Unmarshal(result.data, &state) != nil {
			w.controlBusy = false
			w.say(modelOperationFailure(request.action))
			return
		}
		if request.action != "fast_status" && (state.IsStreaming || state.IsCompacting) {
			w.controlBusy = false
			w.say("Wait for the current task to finish before changing model settings.")
			return
		}
		switch request.action {
		case "model_picker":
			request.current = state.Model
			w.startModelRoles(client, request)
		case "model_select":
			w.startOperation("model_set_role", client, func(ctx context.Context) (json.RawMessage, error) {
				model, err := client.SetModelRole(ctx, request.role)
				if err != nil {
					return nil, err
				}
				return json.Marshal(model)
			}, 0, "", request)
		case "model_switch":
			w.startOperation("model_set_model", client, func(ctx context.Context) (json.RawMessage, error) {
				return client.Call(ctx, "set_model", map[string]any{"provider": request.provider, "modelId": request.modelID})
			}, 0, "", request)
		case "thinking_picker":
			w.showThinkingPickerReady(request.user, state)
		case "thinking_select":
			w.startOperation("model_set_thinking", client, func(ctx context.Context) (json.RawMessage, error) {
				return client.Call(ctx, "set_thinking_level", map[string]any{"level": request.requested})
			}, 0, "", request)
		case "fast_picker":
			w.showFastPickerReady(request.user, state)
		case "fast_select":
			w.startOperation("model_set_fast", client, func(ctx context.Context) (json.RawMessage, error) {
				return client.Call(ctx, "set_fast_mode", map[string]any{"enabled": request.enabled})
			}, 0, "", request)
		case "fast_status":
			w.controlBusy = false
			w.say(fastModeText(state.FastModeEnabled, state.FastModeActive))
		}
	case "model_roles":
		var roles []omp.ModelRole
		if json.Unmarshal(result.data, &roles) != nil {
			w.controlBusy = false
			w.say("Cannot read OMP's cycle roles for this configuration. Use /model provider/model.")
			return
		}
		w.showModelPickerReady(request.user, request.current, roles)
	case "model_set_role":
		var model omp.Model
		if json.Unmarshal(result.data, &model) != nil || model.Provider == "" || model.ID == "" {
			w.controlBusy = false
			w.say(modelOperationFailure("model_select"))
			return
		}
		w.controlBusy = false
		w.touchBinding()
		w.say("Model switched to " + menuText(model.Provider+"/"+model.ID, 256) + ".")
	case "model_set_model":
		var model omp.Model
		if json.Unmarshal(result.data, &model) != nil || model.Provider == "" || model.ID == "" {
			w.controlBusy = false
			w.releaseRuntimeWithReason(true, "failure")
			w.say(modelOperationFailure("model_switch"))
			return
		}
		w.controlBusy = false
		w.touchBinding()
		w.say("Model switched to " + menuText(model.Provider+"/"+model.ID, 256) + ".")
	case "model_set_thinking":
		w.startOperation("model_verify_thinking", client, func(ctx context.Context) (json.RawMessage, error) {
			return client.Call(ctx, "get_state", nil)
		}, 0, "", request)
	case "model_verify_thinking":
		var state modelSettings
		if json.Unmarshal(result.data, &state) != nil || state.ThinkingLevel == "" {
			w.controlBusy = false
			w.releaseRuntimeWithReason(true, "failure")
			w.say("OMP did not report the resulting thinking level. The change could not be confirmed.")
			return
		}
		w.controlBusy = false
		w.touchBinding()
		text := "Thinking level: " + menuText(state.ThinkingLevel, 32)
		if state.ThinkingLevel != request.requested {
			text += " (OMP adjusted the requested " + request.requested + " level)"
		}
		w.say(text)
	case "model_set_fast":
		var fast modelFastResult
		if json.Unmarshal(result.data, &fast) != nil || fast.Enabled == nil || fast.Active == nil {
			w.controlBusy = false
			w.releaseRuntimeWithReason(true, "failure")
			w.say(modelOperationFailure("fast_select"))
			return
		}
		w.controlBusy = false
		w.touchBinding()
		w.say(fastModeText(fast.Enabled, fast.Active))
	default:
		w.controlBusy = false
	}
}

func (w *worker) startModelRoles(client *omp.Client, request modelOperationRequest) {
	workspace := w.binding.Workspace
	args := append([]string(nil), w.b.cfg.OMPArgs...)
	binary := w.b.cfg.OMP
	w.startOperation("model_roles", client, func(ctx context.Context) (json.RawMessage, error) {
		roles, err := omp.CycleRoles(ctx, omp.Config{Binary: binary, CWD: workspace, Args: args})
		if err != nil {
			return nil, err
		}
		return json.Marshal(roles)
	}, 0, "", request)
}

func (w *worker) showModelPicker(user int64) {
	w.beginModelState("model_picker", modelOperationRequest{action: "model_picker", user: user})
}

func (w *worker) showModelPickerReady(user int64, current omp.Model, models []omp.ModelRole) {
	if len(models) == 0 {
		w.controlBusy = false
		w.say("No cycle roles are configured in OMP. Use /model provider/model.")
		return
	}
	// Telegram allows at most 100 inline buttons, including Cancel.
	if len(models) > 99 {
		w.controlBusy = false
		w.say("Too many cycle roles for a Telegram menu. Use /model provider/model.")
		return
	}
	options := make([]string, 0, len(models)+1)
	for _, model := range models {
		label := model.Role
		if model.Selector != "" {
			label += " - " + model.Selector
		}
		options = append(options, menuText(label, 120))
	}
	request := confirmation{action: "model", method: "select", user: user, models: models, options: options}
	options = append(options, "Cancel")
	identity := "none selected"
	if current.Provider != "" && current.ID != "" {
		identity = menuText(current.Provider+"/"+current.ID, 256)
	}
	w.controlBusy = false
	w.confirm(request, "Choose an OMP cycle role\nCurrent model: "+identity, options)
}

func (w *worker) selectModel(c confirmation, index int) {
	if index < 0 || index >= len(c.models) {
		return
	}
	w.beginModelState("model_select", modelOperationRequest{action: "model_select", role: c.models[index].Role})
}

func (w *worker) switchModel(provider, id string) {
	w.beginModelState("model_switch", modelOperationRequest{action: "model_switch", provider: provider, modelID: id})
}

var thinkingLevels = [...]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (w *worker) showThinkingPicker(user int64) {
	w.beginModelState("thinking_picker", modelOperationRequest{action: "thinking_picker", user: user})
}

func (w *worker) showThinkingPickerReady(user int64, state modelSettings) {
	if state.ThinkingLevel == "" {
		w.controlBusy = false
		w.say("OMP did not report a thinking level for this session.")
		return
	}
	labels := make([]string, 0, len(thinkingLevels)+1)
	for _, level := range thinkingLevels {
		label := level
		if level == state.ThinkingLevel {
			label += " [effective]"
		}
		labels = append(labels, label)
	}
	labels = append(labels, "Cancel")
	w.controlBusy = false
	w.confirm(confirmation{action: "thinking", method: "select", user: user, options: thinkingLevels[:]},
		"Choose a thinking level\nCurrent effective level: "+menuText(state.ThinkingLevel, 32)+"\nOMP may adjust the requested level to match the model's capabilities.", labels)
}

func (w *worker) selectThinking(c confirmation, index int) {
	if index < 0 || index >= len(c.options) {
		return
	}
	requested := c.options[index]
	w.beginModelState("thinking_select", modelOperationRequest{action: "thinking_select", requested: requested})
}

func fastModeText(enabled, active *bool) string {
	return "Fast setting: " + statusBool(enabled, "on", "off") + "\nFast active: " + statusBool(active, "on", "off")
}

func (w *worker) showFastPicker(user int64) {
	w.beginModelState("fast_picker", modelOperationRequest{action: "fast_picker", user: user})
}

func (w *worker) showFastPickerReady(user int64, state modelSettings) {
	if state.FastModeEnabled == nil || state.FastModeActive == nil {
		w.controlBusy = false
		w.say("OMP did not report fast mode support for this session.")
		return
	}
	options := []string{"on", "off"}
	labels := []string{"on", "off", "Cancel"}
	if *state.FastModeEnabled {
		labels[0] += " [current]"
	} else {
		labels[1] += " [current]"
	}
	w.controlBusy = false
	w.confirm(confirmation{action: "fast", method: "select", user: user, options: options}, "Choose fast mode\n"+fastModeText(state.FastModeEnabled, state.FastModeActive), labels)
}

func (w *worker) switchFast(enabled bool) {
	w.beginModelState("fast_select", modelOperationRequest{action: "fast_select", enabled: enabled})
}

func (w *worker) showFastStatus() {
	w.beginModelState("fast_status", modelOperationRequest{action: "fast_status"})
}
