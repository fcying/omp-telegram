package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestValidateLoggingPolicy(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		for _, format := range []string{"text", "json"} {
			if err := Validate(Options{Level: level, Format: format}); err != nil {
				t.Fatalf("valid policy %s/%s rejected: %v", level, format, err)
			}
		}
	}
	for _, opts := range []Options{
		{Format: "text"},
		{Level: "info", Format: "text", ComponentLevels: map[string]string{string(Bridge): "trace"}},
		{Level: "trace", Format: "text"},
		{Level: "info", Format: "console"},
		{Level: "info", Format: "text", ComponentLevels: map[string]string{"unknown": "debug"}},
	} {
		if err := Validate(opts); err == nil {
			t.Fatalf("invalid policy accepted: %+v", opts)
		}
	}
}

func TestUnknownComponentValidationDoesNotExposeName(t *testing.T) {
	const unsafeName = "/private/omp-telegram/config.toml https://api.telegram.org/bot123456:FAKE_TOKEN\nUNKNOWN_COMPONENT_CANARY"
	opts := Options{
		Level:           "info",
		Format:          "text",
		ComponentLevels: map[string]string{unsafeName: "debug"},
	}
	if err := Validate(opts); err == nil {
		t.Fatal("unknown component accepted by Validate")
	} else {
		for _, canary := range []string{"/private/omp-telegram/config.toml", "api.telegram.org", "FAKE_TOKEN", "UNKNOWN_COMPONENT_CANARY"} {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("Validate error exposed component key canary %q: %v", canary, err)
			}
		}
	}
	if _, err := New(&bytes.Buffer{}, opts); err == nil {
		t.Fatal("unknown component accepted by New")
	} else {
		for _, canary := range []string{"/private/omp-telegram/config.toml", "api.telegram.org", "FAKE_TOKEN", "UNKNOWN_COMPONENT_CANARY"} {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("New error exposed component key canary %q: %v", canary, err)
			}
		}
	}
}

func TestComponentOverridesSurviveWith(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{
		Level:           "info",
		Format:          "json",
		ComponentLevels: map[string]string{string(RPC): "debug", string(Telegram): "warn"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry.Logger(RPC).With("operation", "test").Debug("rpc debug event")
	registry.Logger(Bridge).Debug("hidden bridge event")
	registry.Logger(Telegram).Info("hidden telegram info event")
	registry.Logger(Telegram).Warn("telegram warning event")

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("component thresholds emitted %d records, want 2: %s", len(lines), output.String())
	}
	var rpc, telegram map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rpc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &telegram); err != nil {
		t.Fatal(err)
	}
	if rpc["component"] != "rpc" || rpc["msg"] != "rpc debug event" || rpc["operation"] != "test" || rpc["level"] != "DEBUG" {
		t.Fatalf("derived RPC logger did not retain override or fields: %#v", rpc)
	}
	if telegram["component"] != "telegram" || telegram["msg"] != "telegram warning event" || telegram["level"] != "WARN" {
		t.Fatalf("telegram override did not retain warning: %#v", telegram)
	}
}

func TestJSONRecordsRemainIndependentlyParseableConcurrently(t *testing.T) {
	var output bytes.Buffer
	registry, err := New(&output, Options{Level: "debug", Format: "json"})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	const recordsPerWorker = 32
	var group sync.WaitGroup
	group.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer group.Done()
			component := components[worker%len(components)]
			logger := registry.Logger(component).With("worker", worker)
			for record := 0; record < recordsPerWorker; record++ {
				logger.Debug("component event", "record", record)
			}
		}(worker)
	}
	group.Wait()

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != workers*recordsPerWorker {
		t.Fatalf("record count = %d, want %d", len(lines), workers*recordsPerWorker)
	}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("record %d is not independently valid JSON: %v", i, err)
		}
		workerValue, ok := record["worker"].(float64)
		if !ok {
			t.Fatalf("record %d has no numeric worker identity: %#v", i, record)
		}
		worker := int(workerValue)
		wantComponent := string(components[worker%len(components)])
		if record["component"] != wantComponent || record["msg"] != "component event" {
			t.Fatalf("record %d metadata = %#v", i, record)
		}
	}
}

func TestUnknownRegistryComponentDoesNotFallback(t *testing.T) {
	registry, err := New(&bytes.Buffer{}, Options{Level: "info", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if logger := registry.Logger(Component("unknown")); logger != nil {
		t.Fatalf("unknown component returned logger: %v", fmt.Sprint(logger))
	}
}
