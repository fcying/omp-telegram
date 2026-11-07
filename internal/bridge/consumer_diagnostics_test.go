package bridge

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"omp-telegram/internal/logging"
	"omp-telegram/internal/omp"
	"omp-telegram/internal/store"
	"omp-telegram/internal/telegram"
)

// The process emits only after Telegram (or a replacement boundary) has entered
// the phase under test, so this observes the real RPC overflow diagnostic.
func consumerDiagnosticClient(t *testing.T, capture *logCapture) *omp.Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logs, err := logging.New(capture, logging.Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	client, err := omp.Start(ctx, omp.Config{Binary: binary, NoSession: true}, logs.Logger(logging.RPC))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func releaseConsumerDiagnostic(t *testing.T, client *omp.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Send(ctx, map[string]any{"type": "fixture_consumer_overflow"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("blocked RPC consumer did not overflow")
	}
}

type consumerDiagnosticHTTP struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (f consumerDiagnosticHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	return f.roundTrip(r)
}

func TestBlockedCallbackReportsConsumerPhaseWithoutPayload(t *testing.T) {
	capture := &logCapture{}
	client := consumerDiagnosticClient(t, capture)
	logs, err := logging.New(capture, logging.Options{Level: "info", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	w := testWorker(t, &worker{
		client: client, ctx: context.Background(),
		binding: store.Binding{Generation: 71}, turn: 19,
		log: logs.Logger(logging.Bridge),
	})
	w.active = 83
	w.b.tg = telegram.New("PRIVATE_CONSUMER_TOKEN", logs.Logger(logging.Telegram))
	previous := http.DefaultTransport
	blocked := false
	http.DefaultTransport = consumerDiagnosticHTTP{roundTrip: func(r *http.Request) (*http.Response, error) {
		if blocked {
			releaseConsumerDiagnostic(t, client)
			// Exercise the default-info WARN without changing the threshold.
			time.Sleep(time.Second)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`))}, nil
	}}
	t.Cleanup(func() { http.DefaultTransport = previous })
	query := &telegram.CallbackQuery{ID: "PRIVATE_CONSUMER_CALLBACK_ID", Data: "PRIVATE_CONSUMER_CALLBACK_DATA:0"}
	if result := w.callback(query); result != callbackDone {
		t.Fatal("expired callback outcome changed")
	}
	if capturedEvent(t, capture, "rpc_consumer_slow") != nil {
		t.Fatal("fast callback produced a slow-consumer warning")
	}
	blocked = true
	if result := w.callback(query); result != callbackDone {
		t.Fatal("blocked callback outcome changed")
	}
	overflow := capturedEvent(t, capture, "rpc_queue_overflow")
	if overflow["consumer_phase"] != "callback" || overflow["client_id"] != float64(client.ID()) {
		t.Fatalf("callback overflow attribution = %+v", overflow)
	}
	slow := capturedEvent(t, capture, "rpc_consumer_slow")
	duration, _ := slow["duration_ms"].(float64)
	if slow["level"] != "WARN" || slow["phase"] != "callback" || slow["point"] != "callback" || duration < 1000 ||
		slow["client_id"] != float64(client.ID()) || slow["generation"] != float64(71) || slow["turn"] != float64(19) || slow["inbox_id"] != float64(83) {
		t.Fatalf("callback slow attribution = %+v", slow)
	}
	if strings.Contains(capture.String(), "PRIVATE_CONSUMER_") {
		t.Fatal("consumer diagnostics disclosed private callback, token, or RPC data")
	}
}

func TestConsumerSpanRestoresOriginalClientAcrossRuntimeReplacement(t *testing.T) {
	oldCapture, replacementCapture := &logCapture{}, &logCapture{}
	oldClient := consumerDiagnosticClient(t, oldCapture)
	replacement := consumerDiagnosticClient(t, replacementCapture)
	w := testWorker(t, &worker{client: oldClient, binding: store.Binding{Generation: 31}, turn: 5})
	oldClient.BeginConsumerPhase("operation")
	span := w.beginConsumerPhase("input", "handle")
	w.runtimeStarted(replacement, nil)
	w.binding.Generation++
	w.turn++
	replacement.BeginConsumerPhase("runtime")
	span.end()
	releaseConsumerDiagnostic(t, oldClient)
	releaseConsumerDiagnostic(t, replacement)
	oldOverflow := capturedEvent(t, oldCapture, "rpc_queue_overflow")
	newOverflow := capturedEvent(t, replacementCapture, "rpc_queue_overflow")
	if oldOverflow["consumer_phase"] != "operation" || newOverflow["consumer_phase"] != "runtime" {
		t.Fatalf("span crossed runtime replacement: old=%+v replacement=%+v", oldOverflow, newOverflow)
	}
	if oldOverflow["client_id"] != float64(oldClient.ID()) || newOverflow["client_id"] != float64(replacement.ID()) {
		t.Fatal("replacement overflow diagnostic borrowed the previous client identity")
	}
}

func TestBackgroundKeyboardCleanupDoesNotReplaceActorPhase(t *testing.T) {
	capture := &logCapture{}
	client := consumerDiagnosticClient(t, capture)
	ctx, cancel := context.WithCancel(context.Background())
	w := testWorker(t, &worker{client: client, ctx: ctx, cancel: cancel})
	w.b.tg = newTestTelegram(t)
	started, second := make(chan struct{}), make(chan struct{})
	release := make(chan struct{}, 1)
	defer close(release)
	oldTransport := http.DefaultTransport
	calls := 0
	http.DefaultTransport = consumerDiagnosticHTTP{roundTrip: func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		} else {
			close(second)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`))}, nil
	}}
	t.Cleanup(func() {
		cancel()
		w.background.Wait()
		http.DefaultTransport = oldTransport
	})
	client.BeginConsumerPhase("input")
	w.queueKeyboardCleanup(1)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("background keyboard cleanup did not start")
	}
	span := w.beginConsumerPhase("operation", "operation_returned")
	defer span.end()
	w.binding.Generation++
	w.turn++
	w.active = 99
	w.queueKeyboardCleanup(2)
	// The second request proves the first helper, including its defers, returned.
	release <- struct{}{}
	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("background keyboard cleanup did not finish the first request")
	}
	releaseConsumerDiagnostic(t, client)
	if overflow := capturedEvent(t, capture, "rpc_queue_overflow"); overflow["consumer_phase"] != "operation" {
		t.Fatalf("background cleanup replaced actor phase: %+v", overflow)
	}
}

func TestRuntimeInitializationOverflowReportsNewClientPhase(t *testing.T) {
	t.Setenv("OMP_TELEGRAM_FIXTURE_INIT_OVERFLOW", "1")
	capture := &logCapture{}
	w, command := setupLoggingWorker(t, capture, "json")
	command("/new " + t.TempDir())
	overflow := capturedEvent(t, capture, "rpc_queue_overflow")
	if overflow["consumer_phase"] != "runtime" || overflow["reason"] != "event_slots" || w.client != nil {
		t.Fatalf("new client initialization was misattributed or not retired: %+v", overflow)
	}
	if strings.Contains(capture.String(), "PRIVATE_CONSUMER_") {
		t.Fatal("initialization overflow disclosed frame content")
	}
}
