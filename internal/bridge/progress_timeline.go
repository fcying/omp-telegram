package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"omp-telegram/internal/telegram"
)

const (
	maxTimelineAttempts = 8
	maxTimelineLines    = 6
	timelineInterval    = 10 * time.Second
)

type timelineResult struct {
	generation  int64
	turn        uint64
	active      int64
	completedAt time.Time
}

type timelineLine struct {
	text    string
	startID string
	name    string
}

type progressTimeline struct {
	enabled   bool
	lines     []timelineLine
	sentNames map[string]string
	omitted   int
	attempts  int
	nextSend  time.Time
	assistant string
	inFlight  bool
	results   chan timelineResult
	ctx       context.Context
	cancel    context.CancelFunc
}

func (w *worker) startTimeline() {
	w.stopTimeline()
	if w.b.cfg.ProgressMode != "verbose" || w.b.tg == nil || w.ctx == nil {
		return
	}
	ctx, cancel := context.WithCancel(w.ctx)
	w.timeline = progressTimeline{enabled: true, results: make(chan timelineResult), ctx: ctx, cancel: cancel}
}

func (w *worker) sendTimeline(text string) {
	w.timeline.inFlight = true
	ctx, results := w.timeline.ctx, w.timeline.results
	chat, thread, replyTo := w.key.chat, w.key.thread, w.activeReplyTo
	generation, turn, active := w.binding.Generation, w.turn, w.active
	w.background.Add(1)
	go func() {
		defer w.background.Done()
		if ctx.Err() != nil {
			return
		}
		sendCtx, done := context.WithTimeout(ctx, 10*time.Second)
		_, err := w.b.tg.Send(sendCtx, chat, thread, text, telegram.SendOptions{ReplyToMessageID: replyTo})
		done()
		completedAt := time.Now()
		if err != nil && ctx.Err() == nil {
			logTelegramFailure(w.b.telegramLog, slog.LevelWarn, "progress_delivery_failed", "verbose progress delivery failed", err,
				slog.Int64("chat_id", chat))
		}
		select {
		case results <- timelineResult{generation: generation, turn: turn, active: active, completedAt: completedAt}:
		case <-ctx.Done():
		}
	}()
}

func (w *worker) timelineFinished(result timelineResult) {
	if !w.timeline.inFlight || result.generation != w.binding.Generation || result.turn != w.turn || result.active != w.active {
		return
	}
	w.timeline.inFlight = false
	w.timeline.nextSend = result.completedAt.Add(timelineInterval)
}

func (w *worker) stopTimeline() {
	if w.timeline.cancel != nil {
		w.timeline.cancel()
	}
	w.timeline = progressTimeline{}
}

func (w *worker) addTimelineLine(line string) {
	w.addTimelineEntry(timelineLine{text: line})
}

func (w *worker) addTimelineStart(id, name string) {
	w.addTimelineEntry(timelineLine{startID: id, name: menuText(name, maxToolNameUnits)})
}

func (w *worker) renameTimelineStart(id, name string) {
	if id == "" || name == "" {
		return
	}
	for i := range w.timeline.lines {
		if w.timeline.lines[i].startID == id {
			w.timeline.lines[i].name = menuText(name, maxToolNameUnits)
			return
		}
	}
}

func (w *worker) addTimelineEntry(entry timelineLine) {
	if !w.timeline.enabled || w.timeline.attempts == maxTimelineAttempts || (entry.text == "" && entry.startID == "") {
		return
	}
	if len(w.timeline.lines) == maxTimelineLines {
		copy(w.timeline.lines, w.timeline.lines[1:])
		w.timeline.lines[maxTimelineLines-1] = entry
		w.timeline.omitted++
		return
	}
	w.timeline.lines = append(w.timeline.lines, entry)
}

func (w *worker) promoteTimelineAssistant() {
	if w.timeline.assistant != "" {
		w.addTimelineLine("Update: " + w.timeline.assistant)
		w.timeline.assistant = ""
	}
}

func (w *worker) flushTimeline(now time.Time) {
	if !w.timeline.enabled || len(w.timeline.lines) == 0 || w.timeline.attempts == maxTimelineAttempts ||
		w.timeline.inFlight || now.Before(w.timeline.nextSend) ||
		!w.taskRunning() || now.Sub(w.progress.StartedAt) < progressInitialDelay {
		return
	}
	var text strings.Builder
	text.WriteString("Activity:\n")
	if w.timeline.omitted != 0 {
		fmt.Fprintf(&text, "%d earlier events omitted\n", w.timeline.omitted)
	}
	for i, line := range w.timeline.lines {
		if i != 0 {
			text.WriteByte('\n')
		}
		if line.startID != "" {
			text.WriteString("- " + line.name + ": started")
			if w.timeline.sentNames == nil {
				w.timeline.sentNames = make(map[string]string)
			}
			w.timeline.sentNames[line.startID] = line.name
		} else {
			text.WriteString(line.text)
		}
	}
	w.timeline.lines = nil
	w.timeline.omitted = 0
	w.timeline.attempts++
	w.sendTimeline(text.String())
}
