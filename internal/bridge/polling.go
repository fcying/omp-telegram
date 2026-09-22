package bridge

import (
	"context"
	"time"
)

const (
	pollBackoffInitial = time.Second
	pollBackoffMax     = 30 * time.Second
)

func nextPollBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return pollBackoffInitial
	}
	if current >= pollBackoffMax/2 {
		return pollBackoffMax
	}
	return current * 2
}

func nextPollDelay(current time.Duration, retryAfter int) time.Duration {
	delay := nextPollBackoff(current)
	if retryAfter <= 0 {
		return delay
	}
	serverDelay := time.Duration(retryAfter) * time.Second
	if serverDelay > delay {
		return serverDelay
	}
	return delay
}

func waitPollBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
