package bridge

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"omp-telegram/internal/i18n"
)

func cloneArgv(args []string) []string {
	return append([]string(nil), args...)
}

func cloneArgPresets(presets [][]string) [][]string {
	if len(presets) == 0 {
		return [][]string{{}}
	}
	out := make([][]string, len(presets))
	for i := range presets {
		out[i] = cloneArgv(presets[i])
	}
	return out
}

func validArgsLabels(labels []string, presetCount int) bool {
	if len(labels) != presetCount || presetCount == 0 {
		return false
	}
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return false
		}
	}
	return true
}

// initializeArgsSelection snapshots configured presets and loads their durable
// selection before workers can be restored.
func (b *Bridge) initializeArgsSelection() error {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.argsPresets = cloneArgPresets(b.cfg.OMPArgs)
	b.argsFingerprint = b.cfg.OMPArgsFingerprint
	index, err := b.db.LoadOrResetOMPArgsSelection(b.argsFingerprint, len(b.argsPresets))
	if err != nil {
		return err
	}
	b.argsIndex = index
	if !validArgsLabels(b.cfg.OMPArgsLabels, len(b.argsPresets)) && b.log != nil {
		b.log.Warn("OMP argument labels are unset, empty, or mismatched; using preset numbers", "event", "omp_args_labels_fallback", "preset_count", len(b.argsPresets), "label_count", len(b.cfg.OMPArgsLabels))
	}
	return nil
}

func (b *Bridge) ensureArgsPresetsLocked() {
	if len(b.argsPresets) == 0 {
		b.argsPresets = cloneArgPresets(b.cfg.OMPArgs)
		b.argsFingerprint = b.cfg.OMPArgsFingerprint
		b.argsIndex = 1
	}
}

func (b *Bridge) selectedArgsSnapshot() []string {
	_, _, args := b.selectedArgsPresetSnapshot()
	return args
}

func (b *Bridge) selectedArgsPresetSnapshot() (int, string, []string) {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	index := b.argsIndex
	if index < 1 || index > len(b.argsPresets) {
		index = 1
	}
	return index, b.argsFingerprint, cloneArgv(b.argsPresets[index-1])
}

func (b *Bridge) argsPresetSnapshotIfCurrent(index int, fingerprint string) ([]string, bool) {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	if index < 1 || index > len(b.argsPresets) || b.argsIndex != index || b.argsFingerprint != fingerprint {
		return nil, false
	}
	return cloneArgv(b.argsPresets[index-1]), true
}

func (b *Bridge) argsPresetsSnapshot() [][]string {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	return cloneArgPresets(b.argsPresets)
}

func (b *Bridge) argsMenuSnapshot() (int, int, []string) {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	index := b.argsIndex
	if index < 1 || index > len(b.argsPresets) {
		index = 1
	}
	var labels []string
	if validArgsLabels(b.cfg.OMPArgsLabels, len(b.argsPresets)) {
		labels = cloneArgv(b.cfg.OMPArgsLabels)
	}
	return index, len(b.argsPresets), labels
}

func (b *Bridge) argsPresetSelection() (int, int) {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	index := b.argsIndex
	if index < 1 || index > len(b.argsPresets) {
		index = 1
	}
	return index, len(b.argsPresets)
}

func (b *Bridge) selectArgsPreset(index int) error {
	b.argsMu.Lock()
	defer b.argsMu.Unlock()
	b.ensureArgsPresetsLocked()
	if index < 1 || index > len(b.argsPresets) {
		return fmt.Errorf("OMP argument preset index must be between 1 and %d", len(b.argsPresets))
	}
	if b.db == nil {
		return errors.New("OMP argument preset persistence is unavailable")
	}
	if err := b.db.SetOMPArgsSelection(index, b.argsFingerprint, len(b.argsPresets)); err != nil {
		return err
	}
	b.argsIndex = index
	return nil
}

func (b *Bridge) argsPresetSelectionMessage(language i18n.Language, index int) string {
	_, count, labels := b.argsMenuSnapshot()
	if index > 0 && index <= count && len(labels) > 0 {
		return i18n.Text(language, i18n.Msg.NamedArgumentPresetSelected, index, menuText(labels[index-1], 120))
	}
	return i18n.Text(language, i18n.Msg.ArgumentPresetSelected, index)
}

const argsMenuPageSize = 10

func argsMenuButtonLabel(language i18n.Language, preset, current int, labels []string) string {
	labelName := i18n.Text(language, i18n.Msg.Preset, preset)
	if len(labels) > 0 {
		labelName = labels[preset-1]
	}
	currentMarker := ""
	if preset == current {
		currentMarker = "* "
	}
	if len(labels) > 0 {
		return menuText(fmt.Sprintf("%d: %s%s", preset, currentMarker, labelName), 61)
	}
	return menuText(currentMarker+labelName, 61)
}

func (w *worker) handleArgsCommand(arg string, user int64) {
	if arg == "" {
		w.showArgsMenuPage(confirmation{action: "args", user: user}, 0)
		return
	}
	validNumber := true
	for i := range arg {
		if arg[i] < '0' || arg[i] > '9' {
			validNumber = false
			break
		}
	}
	index, err := strconv.Atoi(arg)
	_, count := w.b.argsPresetSelection()
	if !validNumber || err != nil || index < 1 || index > count {
		w.say(w.tr(i18n.Msg.UsageArgsPresetNumberInvalidPresetNumber))
		return
	}
	if err := w.b.selectArgsPreset(index); err != nil {
		w.say(w.tr(i18n.Msg.FailedSaveOMPArgumentPresetSelection))
		return
	}
	w.say(w.b.argsPresetSelectionMessage(w.b.language(), index))
}

func (w *worker) showArgsMenuPage(c confirmation, page int) {
	current, count, labels := w.b.argsMenuSnapshot()
	pages := (count + argsMenuPageSize - 1) / argsMenuPageSize
	if page < 0 || page >= pages {
		return
	}
	start := page * argsMenuPageSize
	end := min(start+argsMenuPageSize, count)
	choices := end - start
	c.action, c.method = "args", "select"
	c.argsPage, c.argsChoiceCount = page, choices
	c.options = make([]string, 0, choices+2)
	options := make([]string, 0, choices+3)
	for offset := range choices {
		preset := start + offset + 1
		label := argsMenuButtonLabel(w.b.language(), preset, current, labels)
		c.options = append(c.options, label)
		options = append(options, label)
	}
	if page > 0 {
		c.options = append(c.options, "Previous")
		options = append(options, w.tr(i18n.Msg.Previous))
	}
	if page+1 < pages {
		c.options = append(c.options, "Next")
		options = append(options, w.tr(i18n.Msg.Next))
	}
	options = append(options, w.tr(i18n.Msg.Cancel))
	title := w.tr(i18n.Msg.CurrentOMPArgumentPresetAsteriskMarksCurrentPresetChoose, current)
	if pages > 1 {
		title += w.tr(i18n.Msg.Page, page+1, pages)
	}
	w.confirm(c, title, options)
}

func (w *worker) selectArgsFromMenu(c confirmation, index int) {
	if index == len(c.options) {
		w.finishConfirmation(c, index)
		return
	}
	if index < 0 || index >= len(c.options) {
		return
	}
	if index >= c.argsChoiceCount {
		nextPage := c.argsPage
		switch c.options[index] {
		case "Previous":
			nextPage--
		case "Next":
			nextPage++
		default:
			return
		}
		w.finishMenu(c.messageID, w.tr(i18n.Msg.PageChanged))
		w.showArgsMenuPage(c, nextPage)
		return
	}
	preset := c.argsPage*argsMenuPageSize + index + 1
	if err := w.b.selectArgsPreset(preset); err != nil {
		w.finishMenu(c.messageID, w.tr(i18n.Msg.SelectionNotChanged))
		w.say(w.tr(i18n.Msg.FailedSaveOMPArgumentPresetSelection))
		return
	}
	message := w.b.argsPresetSelectionMessage(w.b.language(), preset)
	if c.messageID == 0 {
		w.say(message)
		return
	}
	w.finishMenu(c.messageID, message)
}
