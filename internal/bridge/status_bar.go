package bridge

import "omp-telegram/internal/i18n"

func (w *worker) renderStatusBar(kind string, values compactStatusFields) string {
	switch kind {
	case "progress":
		values.status = w.tr(i18n.Msg.Processing)
	case "intermediate":
		values.status = w.tr(i18n.Msg.Assistant)
	case "final":
		values.status = assistantCompletedIcon
	}
	fields := w.b.cfg.StatusBar.Fields(kind)
	if kind == "progress" {
		return renderCompactFields(fields, values)
	}
	// Omit whole fields rather than truncating a dirty marker or literal fence.
	return renderCompactFieldsLimit(fields, values, assistantHeaderUnits)
}

func (w *worker) statusNeedsNative(kind string) bool {
	for _, field := range w.b.cfg.StatusBar.Fields(kind) {
		switch field {
		case "model", "thinking", "fast", "context", "speed", "quota":
			return true
		}
	}
	return false
}

func (w *worker) liveStatusNeedsNative() bool {
	return w.statusNeedsNative("intermediate") || w.b.cfg.ProgressMode != "off" && w.statusNeedsNative("progress")
}
