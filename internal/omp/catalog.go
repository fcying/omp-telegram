package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// CommandCatalogState distinguishes undiscovered, available, and explicitly unsupported catalogs.
type CommandCatalogState uint8

const (
	CatalogUnknown CommandCatalogState = iota
	CatalogReady
	CatalogUnsupported
)

type commandSource uint8

const (
	commandSourceUnknown commandSource = iota
	commandSourceBuiltin
	commandSourceExtension
	commandSourceOther
	commandSourceAutoresearch
)

// CommandCatalog is an immutable snapshot scoped to one logical native session.
// Revision increases on replacement; Epoch increases when the session scope is invalidated.
// Names and aliases may be deliberately published in chat-scoped menus, but must not be logged or persisted.
type CommandCatalog struct {
	State     CommandCatalogState
	Revision  uint64
	Epoch     uint64
	SessionID string
	commands  map[string]commandSource
}

// HasCommand accepts an advertised name or alias without a leading slash.
func (s CommandCatalog) HasCommand(name string) bool {
	_, found := s.commands[name]
	return s.State == CatalogReady && found
}

// HasExecutableCommand permits known sources and the explicitly supported autoresearch extension.
func (s CommandCatalog) HasExecutableCommand(name string) bool {
	source := s.commands[name]
	return s.State == CatalogReady && executableCommand(source)
}

// ExecutableCommands yields advertised names and aliases permitted by the bridge.
// Iteration order is unspecified, and unavailable catalogs yield no commands.
func (s CommandCatalog) ExecutableCommands(yield func(string) bool) {
	if s.State != CatalogReady {
		return
	}
	for name, source := range s.commands {
		if executableCommand(source) && !yield(name) {
			return
		}
	}
}

func executableCommand(source commandSource) bool {
	return source == commandSourceBuiltin || source == commandSourceOther || source == commandSourceAutoresearch
}

// HasBuiltinCommand limits colon/whitespace invocation syntax to advertised builtins.
func (s CommandCatalog) HasBuiltinCommand(name string) bool {
	return s.State == CatalogReady && s.commands[name] == commandSourceBuiltin
}

// HasExtensionCommand identifies advertised extensions that may change native session ownership.
func (s CommandCatalog) HasExtensionCommand(name string) bool {
	source := s.commands[name]
	return s.State == CatalogReady && (source == commandSourceExtension || source == commandSourceAutoresearch)
}

type commandCatalogQuery struct {
	id             string
	epoch          uint64
	updateRevision uint64
	done           chan struct{}
	err            error
}

// CommandCatalog returns a snapshot safe to retain while later updates arrive.
func (c *Client) CommandCatalog() CommandCatalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commandCatalog
}

// InvalidateCommandCatalog fences pending discovery responses without changing the client ID.
func (c *Client) InvalidateCommandCatalog() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidateCommandCatalogLocked()
}

func (c *Client) invalidateCommandCatalogLocked() {
	c.commandCatalog = CommandCatalog{
		Revision: c.commandCatalog.Revision + 1,
		Epoch:    c.commandCatalog.Epoch + 1,
	}
}

// The first identity establishes the startup scope without discarding its update.
// Later changes are fenced in wire order, before consumers see the notification.
func (c *Client) receiveCommandCatalogSession(data []byte) {
	var session struct {
		ID string `json:"sessionId"`
	}
	if json.Unmarshal(data, &session) != nil || session.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commandCatalog.SessionID != "" && c.commandCatalog.SessionID != session.ID {
		c.invalidateCommandCatalogLocked()
	}
	c.commandCatalog.SessionID = session.ID
}

// RefreshCommandCatalog joins any discovery already in flight. Cancellation only
// stops the caller's wait: a sent request remains in flight until its response or
// transport shutdown, so a subsequent refresh cannot issue an overlapping query.
// The returned epoch belongs to the joined or created query, even if the session
// changes before completion; errors before query selection return epoch zero.
func (c *Client) RefreshCommandCatalog(ctx context.Context) (epoch uint64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-c.stop:
		return 0, c.failure()
	case <-c.done:
		return 0, c.failure()
	default:
	}
	c.mu.Lock()
	query := c.catalogQuery
	leader := query == nil
	if leader {
		query = &commandCatalogQuery{
			id:             c.ReserveRequestID(),
			epoch:          c.commandCatalog.Epoch,
			updateRevision: c.catalogUpdateRevision,
			done:           make(chan struct{}),
		}
		c.catalogQuery = query
		c.pending[query.id] = request{command: "get_available_commands", catalog: query}
	}
	c.mu.Unlock()
	if leader {
		if err := c.Send(ctx, map[string]any{"type": "get_available_commands", "id": query.id}); err != nil {
			c.finishCommandCatalogQuery(query, err)
		}
	}
	select {
	case <-query.done:
		return query.epoch, query.err
	case <-ctx.Done():
		return query.epoch, ctx.Err()
	case <-c.stop:
		return query.epoch, c.failure()
	case <-c.done:
		return query.epoch, c.failure()
	}
}

func (c *Client) finishCommandCatalogQuery(query *commandCatalogQuery, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalogQuery != query {
		return
	}
	delete(c.pending, query.id)
	query.err = err
	c.catalogQuery = nil
	close(query.done)
}

func validCommandName(name string) bool {
	return name != "" && !strings.HasPrefix(name, "/")
}

// Retain only recognition fields and compact source classification. Other metadata
// must not escape into diagnostic errors.
func parseCommandCatalog(data []byte) (map[string]commandSource, error) {
	var payload struct {
		Commands []struct {
			Name    string          `json:"name"`
			Aliases json.RawMessage `json:"aliases"`
			Source  json.RawMessage `json:"source"`
		} `json:"commands"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Commands == nil {
		return nil, errProtocol
	}
	commands := make(map[string]commandSource, len(payload.Commands))
	for _, command := range payload.Commands {
		if !validCommandName(command.Name) {
			return nil, errProtocol
		}
		classification := commandSourceUnknown
		var source string
		if len(command.Source) != 0 && json.Unmarshal(command.Source, &source) == nil {
			switch source {
			case "builtin":
				classification = commandSourceBuiltin
			case "extension":
				classification = commandSourceExtension
			case "skill", "custom", "mcp_prompt", "file":
				classification = commandSourceOther
			}
		}
		commands[command.Name] = classification
		if command.Name == "autoresearch" && classification == commandSourceExtension {
			commands[command.Name] = commandSourceAutoresearch
		}
		if command.Aliases == nil {
			continue
		}
		var aliases []string
		if json.Unmarshal(command.Aliases, &aliases) != nil || bytes.Equal(bytes.TrimSpace(command.Aliases), []byte("null")) {
			return nil, errProtocol
		}
		for _, alias := range aliases {
			if !validCommandName(alias) {
				return nil, errProtocol
			}
			if alias != command.Name {
				commands[alias] = classification
			}
		}
	}
	return commands, nil
}

func (c *Client) receiveCommandCatalogUpdate(frame []byte) error {
	commands, err := parseCommandCatalog(frame)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.catalogUpdateRevision++
	c.commandCatalog = CommandCatalog{
		State: CatalogReady, Revision: c.commandCatalog.Revision + 1,
		Epoch: c.commandCatalog.Epoch, SessionID: c.commandCatalog.SessionID, commands: commands,
	}
	return nil
}

// The reader applies the result before notifying the caller. An update received
// since request registration, or a changed session scope, always wins.
func (c *Client) receiveCommandCatalogResponse(query *commandCatalogQuery, env envelope) error {
	state := CatalogReady
	var commands map[string]commandSource
	if !*env.Success {
		// Only the native unknown-command rejection proves lack of discovery support.
		var rejection string
		if json.Unmarshal(env.Error, &rejection) != nil || rejection != "Unknown command: get_available_commands" {
			return &classifiedError{kind: "rejected", err: errors.New("omp: RPC command rejected")}
		}
		state = CatalogUnsupported
	} else {
		var err error
		commands, err = parseCommandCatalog(env.Data)
		if err != nil {
			return err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.commandCatalog.Epoch == query.epoch && c.catalogUpdateRevision == query.updateRevision {
		c.commandCatalog = CommandCatalog{
			State: state, Revision: c.commandCatalog.Revision + 1,
			Epoch: query.epoch, SessionID: c.commandCatalog.SessionID, commands: commands,
		}
	}
	return nil
}
