package omp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const maxThinkingOutput = 4096

// ConfiguredThinking queries the native local /effort command, preserving auto
// rather than reporting its effective thinking level. Model defaults and models
// without an adjustable thinking level return an empty selector without error.
// Callers must exclude prompts and model/thinking mutations until it completes.
// Invalid or missing output after a confirmed local acknowledgment leaves later
// metadata queries usable. Uncertain submission or a missing local acknowledgment
// disables metadata queries and stops the client; explicit RPC rejection does neither.
func (c *Client) ConfiguredThinking(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	if c.metadataBusy || c.metadataUnavailable {
		c.mu.Unlock()
		return "", errors.New("omp: configured thinking query unavailable")
	}
	query := &metadataQuery{thinking: true, output: make(chan string, 1)}
	c.metadataBusy = true
	c.metadata = query
	c.mu.Unlock()
	completed, local := false, false
	defer func() {
		c.mu.Lock()
		c.metadata = nil
		c.metadataBusy = false
		if !completed && !local {
			c.metadataUnavailable = true
		}
		c.mu.Unlock()
		if !completed && !local {
			c.fail(errors.New("omp: configured thinking command failed; execution state uncertain"))
		}
	}()
	raw, err := c.Call(ctx, "prompt", map[string]any{"message": "/effort"})
	if err != nil {
		// An explicit RPC rejection is not an uncertain submission.
		if ClassifyError(err) == "rejected" {
			completed = true
		}
		return "", err
	}
	var ack struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	if json.Unmarshal(raw, &ack) != nil || ack.AgentInvoked == nil || *ack.AgentInvoked {
		return "", errors.New("omp: configured thinking command was not local")
	}
	local = true
	// Native command_output precedes its acknowledgment. Waiting for a missing
	// output could consume an unrelated command's later response.
	var text string
	select {
	case text = <-query.output:
	default:
		return "", errors.New("omp: configured thinking output missing")
	}
	selector, ok := parseThinkingOutput(text)
	if !ok {
		return "", errors.New("omp: invalid configured thinking output")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	completed = true
	return selector, nil
}

func thinkingSelector(selector string) bool {
	switch selector {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max", "auto":
		return true
	default:
		return false
	}
}

func noThinkingOutput(text string) bool {
	identity, ok := strings.CutSuffix(text, " has no adjustable thinking level.")
	if !ok {
		return false
	}
	if identity == "The current model" {
		return true
	}
	provider, id, ok := strings.Cut(identity, "/")
	return ok && validModelText(provider) && validModelText(id)
}

func parseThinkingOutput(text string) (string, bool) {
	if len(text) > maxThinkingOutput || strings.ContainsAny(text, "\r\x00") {
		return "", false
	}
	if noThinkingOutput(text) {
		return "", true
	}
	first, available, ok := strings.Cut(text, "\nAvailable: ")
	if !ok || !strings.HasPrefix(first, "Thinking: ") {
		return "", false
	}
	selector := strings.TrimPrefix(first, "Thinking: ")
	if selector != "model default" && !thinkingSelector(selector) {
		return "", false
	}
	choices := strings.Split(available, ", ")
	if len(choices) > 8 {
		return "", false
	}
	for i, choice := range choices {
		if !thinkingSelector(choice) {
			return "", false
		}
		for _, previous := range choices[:i] {
			if choice == previous {
				return "", false
			}
		}
	}
	if selector == "model default" {
		return "", true
	}
	return selector, true
}
