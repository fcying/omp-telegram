package bridge

import (
	"context"
	"encoding/json"
	"time"

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

// modelSelectionState gates both opening a picker and consuming a selection.
func (w *worker) modelSelectionState() (modelSettings, bool) {
	if w.sessionControlBusy() || len(w.queue) != 0 {
		w.say("Wait for the current task and queue to finish before changing model settings.")
		return modelSettings{}, false
	}
	if _, err := w.ensureRuntime(); err != nil {
		w.say(err.Error())
		return modelSettings{}, false
	}
	raw, err := w.call("get_state", nil)
	var state modelSettings
	if err != nil || json.Unmarshal(raw, &state) != nil {
		w.say("Failed to read model settings.")
		return modelSettings{}, false
	}
	if state.IsStreaming || state.IsCompacting {
		w.say("Wait for the current task to finish before changing model settings.")
		return modelSettings{}, false
	}
	return state, true
}

func (w *worker) showModelPicker(user int64) {
	current, ok := w.modelSelectionState()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	models, err := omp.CycleRoles(ctx, omp.Config{Binary: w.b.cfg.OMP, CWD: w.binding.Workspace, Args: w.b.cfg.OMPArgs})
	if err != nil {
		w.say("Cannot read OMP's cycle roles for this configuration. Use /model provider/model.")
		return
	}
	if len(models) == 0 {
		w.say("No cycle roles are configured in OMP. Use /model provider/model.")
		return
	}
	// Telegram allows at most 100 inline buttons, including Cancel.
	if len(models) > 99 {
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
	c := confirmation{action: "model", method: "select", user: user, models: models, options: options}
	options = append(options, "Cancel")
	identity := "none selected"
	if current.Model.Provider != "" && current.Model.ID != "" {
		identity = menuText(current.Model.Provider+"/"+current.Model.ID, 256)
	}
	w.confirm(c, "Choose an OMP cycle role\nCurrent model: "+identity, options)
}

func (w *worker) selectModel(c confirmation, index int) {
	if index < 0 || index >= len(c.models) {
		return
	}
	if _, ok := w.modelSelectionState(); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	client, err := w.ensureRuntime()
	if err != nil {
		w.say(err.Error())
		return
	}
	model, err := client.SetModelRole(ctx, c.models[index].Role)
	w.touchActivity()
	if err != nil {
		w.say("The model switch could not be confirmed. Use /status to check the actual model before retrying.")
		return
	}
	w.touchBinding()
	w.say("Model switched to " + menuText(model.Provider+"/"+model.ID, 256) + ".")
}

func (w *worker) switchModel(provider, id string) {
	if _, ok := w.modelSelectionState(); !ok {
		return
	}
	raw, err := w.call("set_model", map[string]any{"provider": provider, "modelId": id})
	var model omp.Model
	if err != nil || json.Unmarshal(raw, &model) != nil || model.Provider == "" || model.ID == "" {
		w.say("The model switch could not be confirmed. Use /status to check the actual model before retrying.")
		return
	}
	w.touchBinding()
	w.say("Model switched to " + menuText(model.Provider+"/"+model.ID, 256) + ".")
}

var thinkingLevels = [...]string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

func (w *worker) showThinkingPicker(user int64) {
	state, ok := w.modelSelectionState()
	if !ok {
		return
	}
	if state.ThinkingLevel == "" {
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
	w.confirm(confirmation{action: "thinking", method: "select", user: user, options: thinkingLevels[:]},
		"Choose a thinking level\nCurrent effective level: "+menuText(state.ThinkingLevel, 32)+"\nOMP may adjust the requested level to match the model's capabilities.", labels)
}

func (w *worker) selectThinking(c confirmation, index int) {
	if index < 0 || index >= len(c.options) {
		return
	}
	if _, ok := w.modelSelectionState(); !ok {
		return
	}
	requested := c.options[index]
	if _, err := w.call("set_thinking_level", map[string]any{"level": requested}); err != nil {
		w.say("The thinking level change could not be confirmed. Open /thinking to check before retrying.")
		return
	}
	w.touchBinding()
	state, ok := w.modelSelectionState()
	if !ok {
		return
	}
	if state.ThinkingLevel == "" {
		w.say("OMP did not report the resulting thinking level. The change could not be confirmed.")
		return
	}
	text := "Thinking level: " + menuText(state.ThinkingLevel, 32)
	if state.ThinkingLevel != requested {
		text += " (OMP adjusted the requested " + requested + " level)"
	}
	w.say(text)
}

func fastModeText(enabled, active *bool) string {
	return "Fast setting: " + statusBool(enabled, "on", "off") + "\nFast active: " + statusBool(active, "on", "off")
}

func (w *worker) showFastPicker(user int64) {
	state, ok := w.modelSelectionState()
	if !ok {
		return
	}
	if state.FastModeEnabled == nil || state.FastModeActive == nil {
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
	w.confirm(confirmation{action: "fast", method: "select", user: user, options: options}, "Choose fast mode\n"+fastModeText(state.FastModeEnabled, state.FastModeActive), labels)
}

func (w *worker) switchFast(enabled bool) {
	if _, ok := w.modelSelectionState(); !ok {
		return
	}
	raw, err := w.call("set_fast_mode", map[string]any{"enabled": enabled})
	var result struct {
		Enabled *bool `json:"enabled"`
		Active  *bool `json:"active"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Enabled == nil || result.Active == nil {
		w.say("The fast mode change could not be confirmed. It may be unsupported by this model. Use /fast status to check before retrying.")
		return
	}
	w.touchBinding()
	w.say(fastModeText(result.Enabled, result.Active))
}
func (w *worker) showFastStatus() {
	if w.sessionControlBusy() {
		w.say("Wait for the current task and queue to finish before reading fast mode state.")
		return
	}
	if _, err := w.ensureRuntime(); err != nil {
		w.say(err.Error())
		return
	}
	raw, err := w.call("get_state", nil)
	var state modelSettings
	if err != nil || json.Unmarshal(raw, &state) != nil {
		w.say("Failed to read fast mode state.")
		return
	}
	w.say(fastModeText(state.FastModeEnabled, state.FastModeActive))
}
