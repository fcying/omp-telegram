package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
)

type modelSettings struct {
	Model              omp.Model `json:"model"`
	ThinkingLevel      string    `json:"thinkingLevel"`
	ConfiguredThinking string    `json:"configuredThinking"`
	IsStreaming        bool      `json:"isStreaming"`
	IsCompacting       bool      `json:"isCompacting"`
	FastModeEnabled    *bool     `json:"fastModeEnabled"`
	FastModeActive     *bool     `json:"fastModeActive"`
}

type modelOperationRequest struct {
	action    string
	user      int64
	messageID int64
	role      string
	provider  string
	modelID   string
	requested string
	enabled   bool
	current   omp.Model
}

type modelSelectionResult struct {
	omp.Model
	ConfiguredThinking  string `json:"configuredThinking"`
	ThinkingUnavailable bool   `json:"thinkingUnavailable"`
}

type thinkingSelectionResult struct {
	ThinkingLevel       string `json:"thinkingLevel"`
	ConfiguredThinking  string `json:"configuredThinking"`
	ThinkingUnavailable bool   `json:"thinkingUnavailable"`
}

type modelFastResult struct {
	Enabled *bool `json:"enabled"`
	Active  *bool `json:"active"`
}

func modelSelectionWithThinking(ctx context.Context, client *omp.Client, model omp.Model) (json.RawMessage, error) {
	queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	configured, err := client.ConfiguredThinking(queryCtx)
	// The mutation is already confirmed; this optional read cannot undo it.
	return json.Marshal(modelSelectionResult{Model: model, ConfiguredThinking: configured, ThinkingUnavailable: err != nil})
}

func (w *worker) beginModelState(action string, request modelOperationRequest) bool {
	allowBusy := action == "fast_status"
	if w.controlInProgress() {
		w.say(w.tr(i18n.Msg.SessionOperationStillLoading))
		w.finishMenu(request.messageID, w.tr(i18n.Msg.SessionOperationStillLoading))
		return false
	}
	if !allowBusy && (w.sessionControlBusy() || len(w.queue) != 0) {
		w.say(w.tr(i18n.Msg.WaitCurrentTaskQueueFinishBeforeChangingModelSettings))
		w.finishMenu(request.messageID, w.tr(i18n.Msg.WaitCurrentTaskQueueFinishBeforeChangingModelSettings))
		return false
	}
	client, err := w.ensureRuntime()
	if err != nil {
		text := w.errorText(err)
		w.say(text)
		w.finishMenu(request.messageID, text)
		return false
	}
	w.beginControlOperation(controlModel)
	w.modelMenuID = request.messageID
	w.startOperation("model_state", client, func(ctx context.Context) (json.RawMessage, error) {
		return client.Call(ctx, "get_state", nil)
	}, 0, "", request)
	return true
}

func (w *worker) interruptModelMenu() {
	messageID := w.modelMenuID
	w.modelMenuID = 0
	w.finishMenu(messageID, w.tr(i18n.Msg.SettingsChangeInterruptedResultCouldNotConfirmed))
}

func (w *worker) modelOperationFailure(action string) string {
	switch action {
	case "model_select", "model_switch":
		return w.tr(i18n.Msg.ModelSwitchCouldNotConfirmedUseStatusCheckActual)
	case "thinking_select", "thinking_picker":
		return w.tr(i18n.Msg.ThinkingLevelChangeCouldNotConfirmedOpenThinkingCheck)
	case "fast_select", "fast_picker":
		return w.tr(i18n.Msg.FastModeChangeCouldNotConfirmedMayUnsupportedModel)
	case "fast_status":
		return w.tr(i18n.Msg.FailedReadFastModeState)
	default:
		return w.tr(i18n.Msg.FailedReadModelSettings)
	}
}

func (w *worker) modelOperationFinished(result operationResult) {
	request, ok := result.meta.(modelOperationRequest)
	if !ok {
		w.endControlOperation()
		return
	}
	if result.err != nil || result.cancelled {
		w.endControlOperation()
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
		w.say(w.modelOperationFailure(request.action))
		w.finishMenu(request.messageID, w.modelOperationFailure(request.action))
		return
	}
	client, connected := w.runtimeClient()
	if !connected || client.ID() != result.clientID {
		w.endControlOperation()
		w.finishMenu(request.messageID, w.modelOperationFailure(request.action))
		return
	}
	switch result.kind {
	case "model_state":
		var state modelSettings
		if json.Unmarshal(result.data, &state) != nil {
			w.endControlOperation()
			w.say(w.modelOperationFailure(request.action))
			w.finishMenu(request.messageID, w.modelOperationFailure(request.action))
			return
		}
		if request.action != "fast_status" && (state.IsStreaming || state.IsCompacting) {
			w.endControlOperation()
			w.say(w.tr(i18n.Msg.WaitCurrentTaskFinishBeforeChangingModelSettings))
			w.finishMenu(request.messageID, w.tr(i18n.Msg.WaitCurrentTaskFinishBeforeChangingModelSettings))
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
				return modelSelectionWithThinking(ctx, client, model)
			}, 0, "", request)
		case "model_switch":
			w.startOperation("model_set_model", client, func(ctx context.Context) (json.RawMessage, error) {
				raw, err := client.Call(ctx, "set_model", map[string]any{"provider": request.provider, "modelId": request.modelID})
				if err != nil {
					return nil, err
				}
				var model omp.Model
				if err := json.Unmarshal(raw, &model); err != nil {
					return nil, err
				}
				return modelSelectionWithThinking(ctx, client, model)
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
			w.endControlOperation()
			w.say(w.fastModeText(state.FastModeEnabled, state.FastModeActive))
		}
	case "model_roles":
		var roles []omp.ModelRole
		if json.Unmarshal(result.data, &roles) != nil {
			w.endControlOperation()
			w.say(w.tr(i18n.Msg.CannotReadOMPSCycleRolesConfigurationUseModel))
			w.finishMenu(request.messageID, w.tr(i18n.Msg.CannotReadOMPSCycleRolesConfigurationUseModel))
			return
		}
		w.showModelPickerReady(request.user, request.current, roles)
	case "model_set_role", "model_set_model":
		var model modelSelectionResult
		if json.Unmarshal(result.data, &model) != nil || model.Provider == "" || model.ID == "" {
			w.endControlOperation()
			if result.kind == "model_set_model" {
				w.releaseRuntimeWithReason(true, "failure")
			}
			w.say(w.modelOperationFailure(request.action))
			w.finishMenu(request.messageID, w.modelOperationFailure(request.action))
			return
		}
		w.endControlOperation()
		w.touchBinding()
		w.configuredThinking = model.ConfiguredThinking
		text := w.tr(i18n.Msg.ModelSwitched, menuText(model.Provider+"/"+model.ID, 256))
		stopping := client.Stopping()
		if model.ThinkingUnavailable {
			w.configuredThinking = ""
			if stopping {
				text += w.tr(i18n.Msg.ModelSwitchedThinkingUnavailableRuntimeClosed)
			} else {
				text += w.tr(i18n.Msg.ModelSwitchedThinkingUnavailable)
			}
		}
		if stopping {
			// Retire before the actor can dispatch another call to this client.
			w.releaseRuntimeWithReason(true, "failure")
		}
		w.say(text)
		if result.kind == "model_set_role" {
			w.finishMenu(request.messageID, text+w.tr(i18n.Msg.Role, menuText(request.role, 120)))
		}
	case "model_set_thinking":
		w.startOperation("model_verify_thinking", client, func(ctx context.Context) (json.RawMessage, error) {
			raw, err := client.Call(ctx, "get_state", nil)
			if err != nil {
				return nil, err
			}
			var state modelSettings
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
			queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			configured, queryErr := client.ConfiguredThinking(queryCtx)
			// The effective level is verified; the optional query cannot undo it.
			return json.Marshal(thinkingSelectionResult{
				ThinkingLevel: state.ThinkingLevel, ConfiguredThinking: configured, ThinkingUnavailable: queryErr != nil,
			})
		}, 0, "", request)
	case "model_verify_thinking":
		var state thinkingSelectionResult
		if json.Unmarshal(result.data, &state) != nil || state.ThinkingLevel == "" {
			w.endControlOperation()
			w.releaseRuntimeWithReason(true, "failure")
			w.say(w.tr(i18n.Msg.OMPDidNotReportResultingThinkingLevelChangeCould))
			w.finishMenu(request.messageID, w.tr(i18n.Msg.OMPDidNotReportResultingThinkingLevelChangeCould))
			return
		}
		w.endControlOperation()
		w.touchBinding()
		w.configuredThinking = state.ConfiguredThinking
		text := w.tr(i18n.Msg.ThinkingLevel, menuText(state.ThinkingLevel, 32))
		if state.ThinkingLevel != request.requested {
			text = w.tr(i18n.Msg.ThinkingLevelOMPAdjustedRequestedLevel, menuText(state.ThinkingLevel, 32), request.requested)
		}
		stopping := client.Stopping()
		if state.ThinkingUnavailable {
			w.configuredThinking = ""
			if stopping {
				text += w.tr(i18n.Msg.ThinkingChangedQueryUnavailableRuntimeClosed)
			} else {
				text += w.tr(i18n.Msg.ThinkingChangedQueryUnavailable)
			}
		}
		if stopping {
			// Retire before the actor can dispatch another call to this client.
			w.releaseRuntimeWithReason(true, "failure")
		}
		w.say(text)
		w.finishMenu(request.messageID, text)
	case "model_set_fast":
		var fast modelFastResult
		if json.Unmarshal(result.data, &fast) != nil || fast.Enabled == nil || fast.Active == nil {
			w.endControlOperation()
			w.releaseRuntimeWithReason(true, "failure")
			w.say(w.modelOperationFailure("fast_select"))
			w.finishMenu(request.messageID, w.modelOperationFailure("fast_select"))
			return
		}
		w.endControlOperation()
		w.touchBinding()
		text := w.fastModeText(fast.Enabled, fast.Active)
		w.say(text)
		w.finishMenu(request.messageID, text)
	default:
		w.endControlOperation()
		w.finishMenu(request.messageID, w.modelOperationFailure(request.action))
	}
}

func (w *worker) startModelRoles(client *omp.Client, request modelOperationRequest) {
	workspace := w.binding.Workspace
	args := cloneArgv(w.launchArgs)
	binary := w.b.cfg.OMP
	environment := w.b.cfg.OMPEnvironment
	w.startOperation("model_roles", client, func(ctx context.Context) (json.RawMessage, error) {
		roles, err := omp.CycleRoles(ctx, omp.Config{Binary: binary, CWD: workspace, Args: args, Environment: environment})
		if err != nil {
			return nil, err
		}
		return json.Marshal(roles)
	}, 0, "", request)
}

func (w *worker) showModelPicker(user int64) {
	w.beginModelState("model_picker", modelOperationRequest{action: "model_picker", user: user})
}

func currentModelRoleIndex(current omp.Model, models []omp.ModelRole) int {
	if current.Provider == "" || current.ID == "" {
		return -1
	}
	identity := current.Provider + "/" + current.ID
	match := -1
	for i, model := range models {
		selector, _, _ := strings.Cut(model.Selector, ":")
		if selector != identity {
			continue
		}
		if match != -1 {
			return -1
		}
		match = i
	}
	return match
}

func (w *worker) showModelPickerReady(user int64, current omp.Model, models []omp.ModelRole) {
	if len(models) == 0 {
		w.endControlOperation()
		w.say(w.tr(i18n.Msg.NoCycleRolesConfiguredOMPUseModelProviderModel))
		return
	}
	// Telegram allows at most 100 inline buttons, including Cancel.
	if len(models) > 99 {
		w.endControlOperation()
		w.say(w.tr(i18n.Msg.TooManyCycleRolesTelegramMenuUseModelProvider))
		return
	}
	options := make([]string, 0, len(models)+1)
	currentRole := currentModelRoleIndex(current, models)
	for i, model := range models {
		label := model.Role
		if model.Selector != "" {
			label += " - " + model.Selector
		}
		if i == currentRole {
			label = "* " + label
		}
		options = append(options, menuText(label, 120))
	}
	request := confirmation{action: "model", method: "select", user: user, models: models, options: options}
	options = append(options, w.tr(i18n.Msg.Cancel))
	identity := w.tr(i18n.Msg.NoneSelected)
	if current.Provider != "" && current.ID != "" {
		identity = menuText(current.Provider+"/"+current.ID, 256)
	}
	w.endControlOperation()
	w.confirm(request, w.tr(i18n.Msg.ChooseOMPCycleRoleCurrentModel, identity), options)
}

func (w *worker) selectModel(c confirmation, index int) {
	if index < 0 || index >= len(c.models) {
		w.finishMenu(c.messageID, w.tr(i18n.Msg.ModelSelectionUnavailableOpenModelChooseAgain))
		return
	}
	w.beginModelState("model_select", modelOperationRequest{action: "model_select", messageID: c.messageID, role: c.models[index].Role})
}

func (w *worker) switchModel(provider, id string) {
	w.beginModelState("model_switch", modelOperationRequest{action: "model_switch", provider: provider, modelID: id})
}

var thinkingLevels = [...]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (w *worker) thinkingLevelLabel(level string) string {
	switch level {
	case "off":
		return w.tr(i18n.Msg.Off)
	case "minimal":
		return w.tr(i18n.Msg.Minimal)
	case "low":
		return w.tr(i18n.Msg.Low)
	case "medium":
		return w.tr(i18n.Msg.Medium)
	case "high":
		return w.tr(i18n.Msg.High)
	case "xhigh":
		return w.tr(i18n.Msg.Xhigh)
	case "max":
		return w.tr(i18n.Msg.Max)
	default:
		return level
	}
}

func (w *worker) showThinkingPicker(user int64) {
	w.beginModelState("thinking_picker", modelOperationRequest{action: "thinking_picker", user: user})
}

func (w *worker) showThinkingPickerReady(user int64, state modelSettings) {
	if state.ThinkingLevel == "" {
		w.endControlOperation()
		w.say(w.tr(i18n.Msg.OMPDidNotReportThinkingLevelSession))
		return
	}
	labels := make([]string, 0, len(thinkingLevels)+1)
	for _, level := range thinkingLevels {
		label := w.thinkingLevelLabel(level)
		if level == state.ThinkingLevel {
			label = "* " + label
		}
		labels = append(labels, label)
	}
	labels = append(labels, w.tr(i18n.Msg.Cancel))
	w.endControlOperation()
	w.confirm(confirmation{action: "thinking", method: "select", user: user, options: thinkingLevels[:]},
		w.tr(i18n.Msg.ChooseThinkingLevelCurrentEffectiveLevelOMPMayAdjust, menuText(state.ThinkingLevel, 32)), labels)
}

func (w *worker) selectThinking(c confirmation, index int) {
	if index < 0 || index >= len(c.options) {
		w.finishMenu(c.messageID, w.tr(i18n.Msg.ThinkingSelectionUnavailableOpenThinkingChooseAgain))
		return
	}
	requested := c.options[index]
	w.beginModelState("thinking_select", modelOperationRequest{action: "thinking_select", messageID: c.messageID, requested: requested})
}

func (w *worker) fastModeText(enabled, active *bool) string {
	return w.tr(i18n.Msg.FastSettingFastActive, statusBoolText(w.b.language(), enabled, i18n.Msg.On, i18n.Msg.Off), statusBoolText(w.b.language(), active, i18n.Msg.On, i18n.Msg.Off))
}

func (w *worker) showFastPicker(user int64) {
	w.beginModelState("fast_picker", modelOperationRequest{action: "fast_picker", user: user})
}

func (w *worker) showFastPickerReady(user int64, state modelSettings) {
	if state.FastModeEnabled == nil || state.FastModeActive == nil {
		w.endControlOperation()
		w.say(w.tr(i18n.Msg.OMPDidNotReportFastModeSupportSession))
		return
	}
	options := []string{"on", "off"}
	labels := []string{w.tr(i18n.Msg.On), w.tr(i18n.Msg.Off), w.tr(i18n.Msg.Cancel)}
	if *state.FastModeEnabled {
		labels[0] = "* " + labels[0]
	} else {
		labels[1] = "* " + labels[1]
	}
	w.endControlOperation()
	w.confirm(confirmation{action: "fast", method: "select", user: user, options: options}, w.tr(i18n.Msg.ChooseFastMode, w.fastModeText(state.FastModeEnabled, state.FastModeActive)), labels)
}

func (w *worker) selectFast(c confirmation, index int) {
	if index < 0 || index >= len(c.options) {
		w.finishMenu(c.messageID, w.tr(i18n.Msg.FastModeSelectionUnavailableOpenFastChooseAgain))
		return
	}
	w.beginModelState("fast_select", modelOperationRequest{action: "fast_select", messageID: c.messageID, enabled: c.options[index] == "on"})
}

func (w *worker) switchFast(enabled bool) {
	w.beginModelState("fast_select", modelOperationRequest{action: "fast_select", enabled: enabled})
}

func (w *worker) showFastStatus() {
	w.beginModelState("fast_status", modelOperationRequest{action: "fast_status"})
}
