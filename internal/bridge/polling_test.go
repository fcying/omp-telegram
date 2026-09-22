package bridge

import (
	"testing"
	"time"
)

func TestPollBackoffIsExponentialAndBounded(t *testing.T) {
	if got := nextPollBackoff(0); got != pollBackoffInitial {
		t.Fatalf("initial poll backoff = %v, want %v", got, pollBackoffInitial)
	}
	delay := time.Duration(0)
	for range 16 {
		delay = nextPollBackoff(delay)
	}
	if delay != pollBackoffMax {
		t.Fatalf("poll backoff = %v, want cap %v", delay, pollBackoffMax)
	}
	if got := nextPollBackoff(pollBackoffMax); got != pollBackoffMax {
		t.Fatalf("capped poll backoff grew to %v", got)
	}
}

func TestPollDelayHonorsServerRetryAfter(t *testing.T) {
	if got := nextPollDelay(0, 120); got != 120*time.Second {
		t.Fatalf("server retry delay = %v, want %v", got, 120*time.Second)
	}
	if got := nextPollDelay(20*time.Second, 5); got != pollBackoffMax {
		t.Fatalf("local backoff precedence = %v, want %v", got, pollBackoffMax)
	}
	if got := nextPollDelay(0, 0); got != pollBackoffInitial {
		t.Fatalf("missing retry delay = %v, want %v", got, pollBackoffInitial)
	}
}
