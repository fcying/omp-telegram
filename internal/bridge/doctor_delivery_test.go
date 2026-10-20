package bridge

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/config"
	"omp-telegram/internal/i18n"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

const doctorDeliveryBodyCanary = "DOCTOR_BODY_CANARY_MUST_STAY_PRIVATE"
const doctorDeliveryPathCanary = "/private/DOCTOR_PATH_CANARY/attachment.txt"

func newDoctorDeliveryStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func insertDoctorDelivery(t *testing.T, db *store.Store, id int64, key target, state string, created, updated, retry int64) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO outbox(id,chat,thread,text,state,kind,path,name,created_at,updated_at,next_attempt_at,attempt_count)
VALUES(?,?,?,?,?,'document',?,'PRIVATE_ATTACHMENT_NAME',?,?,?,7)`, id, key.chat, key.thread, doctorDeliveryBodyCanary, state, doctorDeliveryPathCanary, created, updated, retry)
	if err != nil {
		t.Fatal(err)
	}
}

type doctorDeliveryRowSnapshot struct {
	State                                   string
	Attempts, NextAttempt, Created, Updated int64
}

func snapshotDoctorDelivery(t *testing.T, db *store.Store) map[int64]doctorDeliveryRowSnapshot {
	t.Helper()
	rows, err := db.DB.Query("SELECT id,state,attempt_count,next_attempt_at,created_at,updated_at FROM outbox ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snapshot := make(map[int64]doctorDeliveryRowSnapshot)
	for rows.Next() {
		var id int64
		var row doctorDeliveryRowSnapshot
		if err := rows.Scan(&id, &row.State, &row.Attempts, &row.NextAttempt, &row.Created, &row.Updated); err != nil {
			t.Fatal(err)
		}
		snapshot[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertDoctorDeliveryUnchanged(t *testing.T, db *store.Store, before map[int64]doctorDeliveryRowSnapshot, newRows int) {
	t.Helper()
	after := snapshotDoctorDelivery(t, db)
	if len(after) != len(before)+newRows {
		t.Fatalf("outbox row count changed by %d, want %d", len(after)-len(before), newRows)
	}
	for id, want := range before {
		if got, ok := after[id]; !ok || got != want {
			t.Fatalf("doctor changed existing delivery %d: before=%+v after=%+v present=%v", id, want, got, ok)
		}
	}
}

func assertDoctorDeliveryPrivate(t *testing.T, text string, ids ...int64) {
	t.Helper()
	for _, secret := range []string{doctorDeliveryBodyCanary, doctorDeliveryPathCanary, "PRIVATE_ATTACHMENT_NAME"} {
		if strings.Contains(text, secret) {
			t.Fatalf("doctor exposed private delivery data %q", secret)
		}
	}
	for _, id := range ids {
		if strings.Contains(text, strconv.FormatInt(id, 10)) {
			t.Fatalf("doctor exposed delivery identifier %d", id)
		}
	}
}

func TestDoctorDeliveryReportsQueueHeadWithoutChangingDelivery(t *testing.T) {
	const now int64 = 1700000000
	key := target{chat: -10, thread: 11}
	for _, tc := range []struct {
		name, state, status, duration string
		retry                         int64
	}{
		{name: "future head blocks later due output", state: "pending", retry: now + 91, status: "Waiting for retry", duration: "1m31s"},
		{name: "due head is ready", state: "pending", retry: now - 1, status: "Ready to send"},
		{name: "sending age ignores later retry", state: "sending", retry: now + 999, status: "Sending for", duration: "1m17s"},
		{name: "large retry does not overflow duration", state: "pending", retry: now + 1099511627776, status: "Waiting for retry", duration: "305419896h36m16s"},
		{name: "empty topic is idle", status: "Idle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newDoctorDeliveryStore(t)
			if tc.state != "" {
				insertDoctorDelivery(t, db, 881122334455, key, tc.state, now-7384, now-77, tc.retry)
				laterRetry := now - 5
				if tc.state == "sending" {
					laterRetry = now + 599
				}
				insertDoctorDelivery(t, db, 881122334456, key, "pending", now-120, now-30, laterRetry)
			}
			before := snapshotDoctorDelivery(t, db)
			outbox := checkDoctorDelivery(context.Background(), db, key, now)
			if outbox.Level != doctorOK || outbox.Delivery == nil {
				t.Fatalf("normal queue should be available and OK: outbox=%+v", outbox)
			}
			stats := outbox.Delivery.stats
			want := store.DeliveryCounts{}
			if tc.state != "" {
				want.Pending = 1
				if tc.state == "pending" {
					want.Pending++
				} else {
					want.Sending++
				}
			}
			if stats.Topic != want || stats.Global != want || string(stats.HeadState) != tc.state {
				t.Fatalf("queue diagnostics = %+v, want counts=%+v head=%q", stats, want, tc.state)
			}
			if tc.state != "" && (stats.HeadCreatedAt != now-7384 || stats.HeadUpdatedAt != now-77 || stats.HeadNextAttemptAt != tc.retry) {
				t.Fatalf("head did not follow lowest output id: %+v", stats)
			}
			report := formatDoctorReport(i18n.English, []doctorCheck{outbox})
			if !strings.Contains(report, tc.status) || (tc.duration != "" && !strings.Contains(report, tc.duration)) {
				t.Fatalf("report lost queue state or duration: %q", report)
			}
			if tc.state == "pending" && !strings.Contains(report, "2h3m4s") {
				t.Fatalf("report lost oldest pending age: %q", report)
			}
			if tc.state == "sending" && (strings.Contains(report, "Waiting for retry") || strings.Contains(report, "9m59s") || strings.Contains(report, "16m39s")) {
				t.Fatalf("sending head borrowed a retry deadline: %q", report)
			}
			assertDoctorDeliveryPrivate(t, report, 881122334455, 881122334456)
			assertDoctorDeliveryUnchanged(t, db, before, 0)
		})
	}
}

func TestDoctorReportsUncertainRecords(t *testing.T) {
	const now int64 = 1700000000
	db := newDoctorDeliveryStore(t)
	key := target{chat: -10, thread: 11}
	if _, err := db.DB.Exec("INSERT INTO inbox(id,raw,state,created_at,updated_at) VALUES(1,?,'uncertain',?,?)", []byte(doctorDeliveryBodyCanary), now-50, now-20); err != nil {
		t.Fatal(err)
	}
	insertDoctorDelivery(t, db, 881122334455, key, "pending", now-120, now-30, now+91)
	insertDoctorDelivery(t, db, 881122334456, key, "uncertain", now-200, now-40, now)
	insertDoctorDelivery(t, db, 881122334457, target{chat: key.chat, thread: 99}, "sending", now-123456, now-123456, now+99999)
	insertDoctorDelivery(t, db, 881122334458, target{chat: 99, thread: key.thread}, "pending", now-999999, now-999999, now+999999)
	insertDoctorDelivery(t, db, 881122334459, target{chat: 99, thread: 99}, "uncertain", now-99, now-99, now)
	before := snapshotDoctorDelivery(t, db)
	inbox := checkDoctorInbox(context.Background(), db)
	outbox := checkDoctorDelivery(context.Background(), db, key, now)
	if inbox.Level != doctorWarn || outbox.Level != doctorWarn || outbox.Delivery == nil {
		t.Fatalf("uncertain deliveries should warn: inbox=%+v outbox=%+v", inbox, outbox)
	}
	stats := outbox.Delivery.stats
	if stats.Topic != (store.DeliveryCounts{Pending: 1, Uncertain: 1}) || stats.Global != (store.DeliveryCounts{Pending: 2, Sending: 1, Uncertain: 2}) {
		t.Fatalf("topic and global counts mixed: %+v", stats)
	}
	if stats.HeadCreatedAt != now-120 || stats.HeadNextAttemptAt != now+91 || stats.OldestPendingAt != now-120 {
		t.Fatalf("foreign topic timing leaked into diagnostics: %+v", stats)
	}
	for _, language := range []i18n.Language{i18n.English, i18n.Chinese} {
		report := formatDoctorReport(language, []doctorCheck{inbox, outbox})
		assertDoctorDeliveryPrivate(t, report, 881122334455, 881122334456, 881122334457, 881122334458, 881122334459)
		for _, pattern := range []string{`(?m)^[^\d\n]*1[^\d\n]+0[^\d\n]*$`, `(?m)^[^\d\n]*2[^\d\n]+1[^\d\n]+2[^\d\n]*$`} {
			if matches := regexp.MustCompile(pattern).FindAllString(report, -1); len(matches) != 1 {
				t.Fatalf("report omitted or duplicated topic/global counts (%s): %q", pattern, report)
			}
		}
		if matches := regexp.MustCompile(`(?m)^[^\d\n]*1[^\d\n]*$`).FindAllString(report, -1); len(matches) != 2 {
			t.Fatalf("report must include separate inbox and topic uncertainty counts: %q", report)
		}
		outboxItems := 0
		for _, line := range strings.Split(report, "\n") {
			if strings.HasPrefix(line, i18n.Text(language, i18n.Msg.Outbox)) {
				outboxItems++
			}
		}
		if outboxItems != 1 {
			t.Fatalf("report contains %d outbox items, want one extended item: %q", outboxItems, report)
		}
		if strings.Contains(report, "Sending for") {
			t.Fatalf("report exposed foreign sending status: %q", report)
		}
		if strings.Contains(report, "%!") || strings.Contains(report, "34h17m36s") || strings.Contains(report, "277h46m39s") {
			t.Fatalf("report exposed another topic's timing or failed formatting: %q", report)
		}
	}
	// The current topic may be empty while a global uncertain count still warns.
	foreign := checkDoctorDelivery(context.Background(), db, target{chat: 123, thread: 456}, now)
	if foreign.Level != doctorWarn || foreign.Delivery == nil || foreign.Delivery.stats.Topic != (store.DeliveryCounts{}) || foreign.Delivery.stats.HeadState != "" {
		t.Fatalf("global uncertain count was lost for empty topic: %+v", foreign)
	}
	assertDoctorDeliveryUnchanged(t, db, before, 0)
	var state string
	var created, updated int64
	if err := db.DB.QueryRow("SELECT state,created_at,updated_at FROM inbox WHERE id=1").Scan(&state, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if state != "uncertain" || created != now-50 || updated != now-20 {
		t.Fatalf("doctor changed inbox state or timestamps: %q %d %d", state, created, updated)
	}
}

func newDoctorDeliveryWorker(t *testing.T, db *store.Store) (*worker, *logCapture) {
	t.Helper()
	oldGetMe, oldRunOMP, oldStatfs, oldTimeout := doctorGetMe, doctorRunOMP, doctorStatfs, doctorTimeout
	t.Cleanup(func() {
		doctorGetMe, doctorRunOMP, doctorStatfs, doctorTimeout = oldGetMe, oldRunOMP, oldStatfs, oldTimeout
	})
	doctorGetMe = func(context.Context, *telegram.Client) error { return nil }
	doctorRunOMP = func(context.Context, string, omp.Environment) error { return nil }
	doctorStatfs = func(string) (uint64, error) { return 2 << 30, nil }
	doctorTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	capture := &logCapture{}
	w := testWorker(t, &worker{
		b: testBridge(t, &Bridge{
			cfg: config.Config{OMP: "fixture", DataDir: t.TempDir(), MaxWorkers: 1, QueueCapacity: 1, ProgressMode: "summary"},
			db:  db, tg: newTestTelegram(t), bot: telegram.User{ID: 99},
		}),
		key: target{chat: -10, thread: 11}, ctx: ctx, cancel: cancel,
		runtimeLifecycle: runtimeLifecycle{runtime: runtimeReleased},
		log:              slog.New(slog.NewJSONHandler(capture, nil)).With("component", "bridge", "chat_id", int64(-10), "thread_id", int64(11)),
	})
	t.Cleanup(func() {
		w.teardownWorker(true)
		cancel()
		w.background.Wait()
	})
	return w, capture
}

func waitDoctorDeliveryResult(t *testing.T, w *worker) doctorResult {
	t.Helper()
	select {
	case result := <-w.doctorResults:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("doctor did not complete")
		return doctorResult{}
	}
}

func assertDoctorDeliveryLogFields(t *testing.T, record map[string]any, unavailable bool) {
	t.Helper()
	if record == nil {
		t.Fatal("doctor completion event is missing")
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "component": true, "event": true, "chat_id": true, "thread_id": true, "result": true, "head_state": true}
	if !unavailable {
		for _, field := range []string{"pending_count", "sending_count", "uncertain_count", "head_wait_seconds", "retry_in_seconds"} {
			allowed[field] = true
		}
	}
	for field := range record {
		if !allowed[field] {
			t.Errorf("doctor completion log contains unexpected field %q", field)
		}
	}
	for field := range allowed {
		if _, ok := record[field]; !ok {
			t.Errorf("doctor completion log omitted %q", field)
		}
	}
}

func TestDoctorDeliveryCompletionLogsOnlyBoundedTopicSummary(t *testing.T) {
	db := newDoctorDeliveryStore(t)
	w, capture := newDoctorDeliveryWorker(t, db)
	now := time.Now().Unix()
	insertDoctorDelivery(t, db, 881122334455, w.key, "pending", now-180, now-30, now+3600)
	insertDoctorDelivery(t, db, 881122334456, w.key, "pending", now-100, now-20, now-1)
	insertDoctorDelivery(t, db, 881122334457, w.key, "sending", now-90, now-10, now+9999)
	insertDoctorDelivery(t, db, 881122334458, w.key, "uncertain", now-80, now-5, now)
	insertDoctorDelivery(t, db, 881122334459, target{chat: 77, thread: 88}, "uncertain", now-9999, now-9999, now)
	before := snapshotDoctorDelivery(t, db)
	started := time.Now().Unix()
	w.runDoctor()
	result := waitDoctorDeliveryResult(t, w)
	finished := time.Now().Unix()
	if result.err != nil {
		t.Fatalf("doctor result error = %v", result.err)
	}
	w.doctorFinished(result)
	record := capturedEvent(t, capture, "doctor")
	assertDoctorDeliveryLogFields(t, record, false)
	for field, want := range map[string]float64{"pending_count": 2, "sending_count": 1, "uncertain_count": 1} {
		if record[field] != want {
			t.Errorf("doctor %s = %v, want %v", field, record[field], want)
		}
	}
	if record["result"] != "warn" || record["head_state"] != "pending" {
		t.Fatalf("doctor completion state = %+v", record)
	}
	for field, bounds := range map[string][2]float64{
		"head_wait_seconds": {float64(started - now + 180), float64(finished - now + 180)},
		"retry_in_seconds":  {float64(now + 3600 - finished), float64(now + 3600 - started)},
	} {
		got, ok := record[field].(float64)
		if !ok || got < bounds[0] || got > bounds[1] {
			t.Errorf("doctor %s = %v, want [%v,%v]", field, record[field], bounds[0], bounds[1])
		}
	}
	assertDoctorDeliveryPrivate(t, capture.String(), 881122334455, 881122334456, 881122334457, 881122334458, 881122334459)
	assertDoctorDeliveryUnchanged(t, db, before, 1)
	var report string
	if err := db.DB.QueryRow("SELECT text FROM outbox WHERE id NOT IN (881122334455,881122334456,881122334457,881122334458,881122334459)").Scan(&report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "omp-telegram diagnostics") || !strings.Contains(report, "Waiting for retry") {
		t.Fatalf("doctor did not enqueue its queue report: %q", report)
	}
	assertDoctorDeliveryPrivate(t, report, 881122334455, 881122334456, 881122334457, 881122334458, 881122334459)
}

func TestDoctorDeliveryQueryFailureStillProducesWarningReport(t *testing.T) {
	db := newDoctorDeliveryStore(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	outbox := checkDoctorDelivery(context.Background(), db, target{chat: 1, thread: 2}, 1700000000)
	if outbox.Level != doctorWarn || outbox.Delivery != nil {
		t.Fatalf("failed query should be unavailable, not successful zero counts: outbox=%+v", outbox)
	}
	report := formatDoctorReport(i18n.English, []doctorCheck{outbox})
	if !strings.Contains(report, "omp-telegram diagnostics") || !strings.Contains(report, "WARN") || strings.Contains(report, "Idle") {
		t.Fatalf("unavailable diagnostics did not remain reportable: %q", report)
	}
}

func TestDoctorDeliveryTimeoutReportsUnavailableInsteadOfGenericTimeout(t *testing.T) {
	db := newDoctorDeliveryStore(t)
	w, capture := newDoctorDeliveryWorker(t, db)
	now := time.Now().Unix()
	insertDoctorDelivery(t, db, 881122334455, w.key, "pending", now-180, now-30, now+3600)
	before := snapshotDoctorDelivery(t, db)
	doctorTimeout = time.Second
	locked := make(chan *sql.Conn, 1)
	doctorStatfs = func(string) (uint64, error) {
		conn, err := db.DB.Conn(context.Background())
		if err != nil {
			return 0, err
		}
		locked <- conn
		return 2 << 30, nil
	}
	w.runDoctor()
	var conn *sql.Conn
	select {
	case conn = <-locked:
	case <-time.After(time.Second):
		t.Fatal("doctor did not reach the last baseline probe")
	}
	t.Cleanup(func() { _ = conn.Close() })
	result := waitDoctorDeliveryResult(t, w)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if result.err != nil {
		t.Fatalf("delivery-only timeout failed the entire doctor: %v", result.err)
	}
	var outbox *doctorCheck
	var inbox *doctorCheck
	for i := range result.checks {
		if result.checks[i].Name == i18n.Msg.Outbox {
			outbox = &result.checks[i]
		}
		if result.checks[i].Name == i18n.Msg.Inbox {
			inbox = &result.checks[i]
		}
	}
	if outbox == nil || outbox.Level != doctorWarn || outbox.Delivery != nil {
		t.Fatalf("delivery timeout omitted unavailable warning: %+v", outbox)
	}
	if inbox == nil || inbox.Level != doctorOK {
		t.Fatalf("delivery timeout changed the successful inbox check: %+v", inbox)
	}
	w.doctorFinished(result)
	if w.doctorCancel != nil {
		t.Fatal("completed doctor kept its cancellation handle")
	}
	var report string
	if err := db.DB.QueryRow("SELECT text FROM outbox WHERE id<>881122334455").Scan(&report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "omp-telegram diagnostics") || !strings.Contains(report, "WARN") || strings.Contains(report, "Diagnostics timed out") || strings.Contains(report, "Idle") {
		t.Fatalf("delivery timeout did not produce a usable report: %q", report)
	}
	record := capturedEvent(t, capture, "doctor")
	assertDoctorDeliveryLogFields(t, record, true)
	if record["head_state"] != "unavailable" || record["result"] != "warn" {
		t.Fatalf("unavailable doctor completion = %+v", record)
	}
	assertDoctorDeliveryPrivate(t, report+capture.String(), 881122334455)
	assertDoctorDeliveryUnchanged(t, db, before, 1)
}
