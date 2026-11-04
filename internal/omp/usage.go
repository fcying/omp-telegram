package omp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const maxUsageOutput = 64 << 10

// usageQuery is protected by Client.mu, including the reader-owned ACK boundary.
type usageQuery struct {
	id                       string
	sessionID                string
	epoch                    uint64
	started, open, seen      bool
	invalid, local, rejected bool
	uncertain                bool
	text                     string
}

// Usage privately queries the advertised builtin /usage in the current session.
// Callers must exclude other prompts and mutations until it completes. Native
// output must precede an explicit local ACK; uncertain execution retires the client.
func (c *Client) Usage(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	catalog := c.commandCatalog
	if c.metadataBusy || c.metadataUnavailable || catalog.SessionID == "" || !catalog.HasBuiltinCommand("usage") {
		c.mu.Unlock()
		return "", errors.New("omp: provider usage query unavailable")
	}
	query := &usageQuery{id: c.ReserveRequestID(), sessionID: catalog.SessionID, epoch: catalog.Epoch}
	c.metadataBusy = true
	c.mu.Unlock()
	interrupted := false
	defer func() {
		c.mu.Lock()
		uncertain := query.started && !query.rejected && (interrupted || !query.local || !query.seen)
		query.uncertain = uncertain
		query.open = false
		query.text = ""
		c.metadata = nil
		c.metadataBusy = false
		if uncertain {
			c.metadataUnavailable = true
		}
		c.mu.Unlock()
		if uncertain {
			c.fail(errors.New("omp: provider usage command failed; execution state uncertain"))
		}
	}()
	// Catalog updates and session changes are applied by the reader before this
	// response is released, so a stale scope cannot authorize the private prompt.
	raw, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		return "", err
	}
	var state struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(raw, &state) != nil || state.ID != catalog.SessionID {
		return "", errors.New("omp: provider usage session unavailable")
	}
	c.mu.Lock()
	current := c.commandCatalog
	if current.Epoch != catalog.Epoch || current.SessionID != catalog.SessionID || !current.HasBuiltinCommand("usage") {
		c.mu.Unlock()
		return "", errors.New("omp: provider usage catalog changed")
	}
	c.metadata = &metadataQuery{usage: query}
	c.mu.Unlock()
	_, err = c.CallWithID(ctx, query.id, "prompt", map[string]any{"message": "/usage"})
	if err != nil {
		interrupted = ctx.Err() != nil
		return "", err
	}
	c.mu.Lock()
	local, seen, invalid, text := query.local, query.seen, query.invalid, query.text
	current = c.commandCatalog
	c.mu.Unlock()
	if !local {
		return "", errors.New("omp: provider usage command was not local")
	}
	if current.Epoch != catalog.Epoch || current.SessionID != catalog.SessionID || !current.HasBuiltinCommand("usage") {
		return "", errors.New("omp: provider usage catalog changed")
	}
	if !seen {
		return "", errors.New("omp: provider usage output missing")
	}
	if invalid || !validUsageOutput(text) {
		return "", errors.New("omp: invalid provider usage output")
	}
	if err := ctx.Err(); err != nil {
		interrupted = true
		return "", err
	}
	return text, nil
}

// UsageNeedsRetirement reports submitted or uncertain private work. Callers must
// cancel the query context first; the send hook checks cancellation under mu.
func (c *Client) UsageNeedsRetirement() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.metadata != nil && c.metadata.usage != nil {
		query := c.metadata.usage
		return query.started && !query.rejected
	}
	return c.usageFence != nil && c.usageFence.uncertain
}

// A local prompt's ACK ends capture in wire order, not when its caller wakes.
// Suppress matching late output while idle. Without output request IDs, reuse
// relies on native local output-before-ACK ordering; no snapshot proves it.
func (c *Client) receiveUsageResponse(env envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	query := c.usageFence
	if query == nil || env.ID != query.id || env.Command != "prompt" {
		return
	}
	query.open = false
	query.rejected = !*env.Success
	if query.rejected {
		return
	}
	var ack struct {
		AgentInvoked *bool `json:"agentInvoked"`
	}
	query.local = json.Unmarshal(env.Data, &ack) == nil && ack.AgentInvoked != nil && !*ack.AgentInvoked
}

// A new prompt owns its own output, including an explicit user's /usage. Under
// the write gate, recheck private authorization before arming capture or sending.
func (c *Client) prepareUsagePrompt(ctx context.Context, id any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.metadata != nil && c.metadata.usage != nil && c.metadata.usage.id == id {
		query := c.metadata.usage
		catalog := c.commandCatalog
		if catalog.Epoch != query.epoch || catalog.SessionID != query.sessionID || !catalog.HasBuiltinCommand("usage") {
			return errors.New("omp: provider usage catalog changed")
		}
		query.started, query.open = true, true
		c.usageFence = query
	} else {
		c.usageFence = nil
	}
	return nil
}

func matchingUsageOutput(text string) bool {
	text = strings.TrimPrefix(text, "```\n")
	return text == "Usage" || strings.HasPrefix(text, "Usage\n") || strings.HasPrefix(text, "Usage (")
}

func validUsageOutput(text string) bool {
	if len(text) > maxUsageOutput || strings.ContainsAny(text, "\r\x00") {
		return false
	}
	body, ok := strings.CutPrefix(text, "```\n")
	if !ok {
		// Preserve the native fallback privately; the quota parser ignores it.
		return strings.HasPrefix(text, "Usage\nInput tokens: ") && strings.Contains(text, "\nCost: $")
	}
	body, ok = strings.CutSuffix(body, "\n```")
	if !ok || strings.Contains(body, "```") {
		return false
	}
	header, rest, ok := strings.Cut(body, "\n")
	return ok && (header == "Usage" || strings.HasPrefix(header, "Usage (") && strings.HasSuffix(header, " ago)")) && strings.TrimSpace(rest) != ""
}
