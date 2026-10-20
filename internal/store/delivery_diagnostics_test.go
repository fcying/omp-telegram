package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

const diagnosticsNow int64 = 1700000000

type diagnosticsOutput struct {
	id                                  int64
	chat, thread                        any
	state                               OutboxState
	createdAt, updatedAt, nextAttemptAt int64
	attemptCount                        int64
}

func insertDiagnosticsOutputs(t *testing.T, s *Store, outputs []diagnosticsOutput) {
	t.Helper()
	for _, output := range outputs {
		_, err := s.DB.Exec(`INSERT INTO outbox(id,chat,thread,text,state,created_at,updated_at,next_attempt_at,attempt_count)
VALUES(?,?,?,'private delivery body',?,?,?,?,?)`, output.id, output.chat, output.thread, output.state,
			output.createdAt, output.updatedAt, output.nextAttemptAt, output.attemptCount)
		requireStoreOK(t, err)
	}
}

func diagnosticsOutboxState(t *testing.T, s *Store) [][4]any {
	t.Helper()
	rows, err := s.DB.Query("SELECT id,state,attempt_count,next_attempt_at FROM outbox ORDER BY id")
	requireStoreOK(t, err)
	defer rows.Close()
	var snapshot [][4]any
	for rows.Next() {
		var id, attempts, next int64
		var state string
		requireStoreOK(t, rows.Scan(&id, &state, &attempts, &next))
		snapshot = append(snapshot, [4]any{id, state, attempts, next})
	}
	requireStoreOK(t, rows.Err())
	return snapshot
}

func requireDiagnosticsOutboxUnchanged(t *testing.T, s *Store, before [][4]any) {
	t.Helper()
	if after := diagnosticsOutboxState(t, s); !reflect.DeepEqual(after, before) {
		t.Fatalf("diagnostics changed outbox: before=%v after=%v", before, after)
	}
}

func TestDeliveryDiagnosticsQueueSnapshot(t *testing.T) {
	const now = diagnosticsNow
	tests := []struct {
		name    string
		outputs []diagnosticsOutput
		want    DeliveryDiagnostics
	}{
		{
			name: "future head blocks later due pending",
			outputs: []diagnosticsOutput{
				{1, -100, 7, OutboxDone, now - 1000, now - 900, 0, 8},
				{2, -100, 7, OutboxUncertain, now - 800, now - 700, now - 600, 5},
				{20, -100, 7, OutboxPending, now - 60, now - 20, now + 90, 3},
				{30, -100, 7, OutboxPending, now - 120, now - 10, now - 1, 0},
			},
			want: DeliveryDiagnostics{
				Topic: DeliveryCounts{Pending: 2, Uncertain: 1}, Global: DeliveryCounts{Pending: 2, Uncertain: 1},
				OldestPendingAt: now - 120,
				HeadState:       OutboxPending, HeadCreatedAt: now - 60, HeadUpdatedAt: now - 20, HeadNextAttemptAt: now + 90,
			},
		},
		{
			name: "ready head follows id rather than timestamps",
			outputs: []diagnosticsOutput{
				{20, -100, 7, OutboxPending, now - 10, now - 5, now - 1, 1},
				{30, -100, 7, OutboxPending, now - 100, now - 80, now + 100, 4},
			},
			want: DeliveryDiagnostics{
				Topic: DeliveryCounts{Pending: 2}, Global: DeliveryCounts{Pending: 2},
				OldestPendingAt: now - 100,
				HeadState:       OutboxPending, HeadCreatedAt: now - 10, HeadUpdatedAt: now - 5, HeadNextAttemptAt: now - 1,
			},
		},
		{
			name: "sending head precedes pending",
			outputs: []diagnosticsOutput{
				{1, -100, 7, OutboxUncertain, now - 900, now - 800, 0, 6},
				{30, -100, 7, OutboxSending, now - 300, now - 45, now - 80, 4},
				{40, -100, 7, OutboxPending, now - 500, now - 40, now - 20, 2},
				{50, -100, 7, OutboxSending, now - 600, now - 1, now - 50, 7},
			},
			want: DeliveryDiagnostics{
				Topic: DeliveryCounts{Pending: 1, Sending: 2, Uncertain: 1}, Global: DeliveryCounts{Pending: 1, Sending: 2, Uncertain: 1},
				OldestPendingAt: now - 500,
				HeadState:       OutboxSending, HeadCreatedAt: now - 300, HeadUpdatedAt: now - 45, HeadNextAttemptAt: now - 80,
			},
		},
		{
			name: "terminal deliveries are idle",
			outputs: []diagnosticsOutput{
				{1, -100, 7, OutboxDone, now - 40, now - 30, now - 20, 1},
				{2, -100, 7, OutboxSent, now - 50, now - 40, now - 30, 2},
				{3, -100, 7, OutboxFailed, now - 60, now - 50, now - 40, 3},
				{4, -100, 7, OutboxCancelled, now - 70, now - 60, now - 50, 4},
			},
			want: DeliveryDiagnostics{},
		},
		{
			name: "uncertain deliveries have no active head",
			outputs: []diagnosticsOutput{
				{1, -100, 7, OutboxUncertain, now - 50, now - 40, now + 100, 2},
				{2, -100, 7, OutboxUncertain, now - 80, now - 70, now - 60, 5},
			},
			want: DeliveryDiagnostics{
				Topic: DeliveryCounts{Uncertain: 2}, Global: DeliveryCounts{Uncertain: 2},
			},
		},
		{
			name: "empty queue",
			want: DeliveryDiagnostics{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t, t.TempDir())
			insertDiagnosticsOutputs(t, s, tt.outputs)
			_, err := s.DB.Exec("PRAGMA query_only=ON")
			requireStoreOK(t, err)
			before := diagnosticsOutboxState(t, s)
			got, err := s.DeliveryDiagnostics(context.Background(), -100, 7)
			requireDiagnosticsOutboxUnchanged(t, s, before)
			requireStoreOK(t, err)
			if got != tt.want {
				t.Fatalf("diagnostics = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDeliveryDiagnosticsTopicIsolation(t *testing.T) {
	const now = diagnosticsNow
	s := openTestStore(t, t.TempDir())
	insertDiagnosticsOutputs(t, s, []diagnosticsOutput{
		{1, -100, 0, OutboxPending, now - 10, now - 5, now + 10, 1},
		{2, -100, 7, OutboxSending, now - 20, now - 4, now - 15, 2},
		{3, -200, 0, OutboxUncertain, now - 30, now - 3, now - 25, 3},
		{4, -200, 7, OutboxPending, now - 40, now - 2, now - 35, 4},
		{5, -100, nil, OutboxPending, now - 50, now - 1, now - 45, 5},
		{6, nil, 0, OutboxSending, now - 60, now - 6, now - 55, 6},
		{7, nil, nil, OutboxUncertain, now - 70, now - 7, now - 65, 7},
		{8, -100, 0, OutboxUncertain, now - 80, now - 8, now - 75, 8},
		{9, -100, 70, OutboxPending, now - 90, now - 9, now - 85, 9},
	})
	tests := []struct {
		name         string
		chat, thread int64
		want         DeliveryDiagnostics
	}{
		{"zero thread", -100, 0, DeliveryDiagnostics{
			Topic: DeliveryCounts{Pending: 1, Uncertain: 1}, OldestPendingAt: now - 10,
			HeadState: OutboxPending, HeadCreatedAt: now - 10, HeadUpdatedAt: now - 5, HeadNextAttemptAt: now + 10,
		}},
		{"same chat different thread", -100, 7, DeliveryDiagnostics{
			Topic:     DeliveryCounts{Sending: 1},
			HeadState: OutboxSending, HeadCreatedAt: now - 20, HeadUpdatedAt: now - 4, HeadNextAttemptAt: now - 15,
		}},
		{"different chat zero thread", -200, 0, DeliveryDiagnostics{Topic: DeliveryCounts{Uncertain: 1}}},
		{"different chat same thread", -200, 7, DeliveryDiagnostics{
			Topic: DeliveryCounts{Pending: 1}, OldestPendingAt: now - 40,
			HeadState: OutboxPending, HeadCreatedAt: now - 40, HeadUpdatedAt: now - 2, HeadNextAttemptAt: now - 35,
		}},
		{"thread prefix does not match", -100, 70, DeliveryDiagnostics{
			Topic: DeliveryCounts{Pending: 1}, OldestPendingAt: now - 90,
			HeadState: OutboxPending, HeadCreatedAt: now - 90, HeadUpdatedAt: now - 9, HeadNextAttemptAt: now - 85,
		}},
		{"absent chat", -300, 0, DeliveryDiagnostics{}},
		{"absent thread", -100, 8, DeliveryDiagnostics{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := diagnosticsOutboxState(t, s)
			got, err := s.DeliveryDiagnostics(context.Background(), tt.chat, tt.thread)
			requireDiagnosticsOutboxUnchanged(t, s, before)
			requireStoreOK(t, err)
			tt.want.Global = DeliveryCounts{Pending: 4, Sending: 2, Uncertain: 3}
			if got != tt.want {
				t.Fatalf("diagnostics = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDeliveryDiagnosticsDeadlineWhileWaitingForConnection(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	insertDiagnosticsOutputs(t, s, []diagnosticsOutput{
		{1, -100, 0, OutboxPending, diagnosticsNow - 20, diagnosticsNow - 10, diagnosticsNow + 60, 3},
		{2, -200, 7, OutboxSending, diagnosticsNow - 30, diagnosticsNow - 5, diagnosticsNow - 10, 4},
	})
	before := diagnosticsOutboxState(t, s)
	conn, err := s.DB.Conn(context.Background())
	requireStoreOK(t, err)
	defer conn.Close()
	waitCount := s.DB.Stats().WaitCount
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	requireStoreOK(t, ctx.Err())
	_, err = s.DeliveryDiagnostics(ctx, -100, 0)
	requireStoreOK(t, conn.Close())
	requireDiagnosticsOutboxUnchanged(t, s, before)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("diagnostics error = %v, want deadline exceeded", err)
	}
	if s.DB.Stats().WaitCount <= waitCount {
		t.Fatal("diagnostic query did not wait for the occupied database connection")
	}
}

func TestDeliveryDiagnosticsDatabaseErrors(t *testing.T) {
	for _, name := range []string{"missing timestamp column", "closed database"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := openTestStore(t, dir)
			insertDiagnosticsOutputs(t, s, []diagnosticsOutput{
				{1, -100, 0, OutboxPending, diagnosticsNow - 20, diagnosticsNow - 10, diagnosticsNow + 60, 3},
				{2, -200, 7, OutboxUncertain, diagnosticsNow - 30, diagnosticsNow - 5, diagnosticsNow - 10, 4},
			})
			before := diagnosticsOutboxState(t, s)
			if name == "closed database" {
				requireStoreOK(t, s.Close())
			} else {
				_, err := s.DB.Exec("ALTER TABLE outbox RENAME COLUMN created_at TO unavailable_created_at")
				requireStoreOK(t, err)
			}
			_, err := s.DeliveryDiagnostics(context.Background(), -100, 0)
			if name == "closed database" {
				s = openTestStore(t, dir)
			}
			requireDiagnosticsOutboxUnchanged(t, s, before)
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("diagnostics error = %v, want database error", err)
			}
		})
	}
}
