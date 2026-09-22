package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	reasonMessageThreadNotFound = "message_thread_not_found"
	reasonReplyTargetNotFound   = "reply_target_not_found"
	reasonRateLimited           = "rate_limited"
	reasonTransportFailed       = "transport_failed"
	reasonTimeout               = "timeout"
	reasonAPIBadRequest         = "api_bad_request"
	reasonAPIError              = "api_error"
	reasonCancelled             = "cancelled"
	reasonUnknown               = "unknown"
)

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	IsBot    bool   `json:"is_bot"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type TextQuote struct {
	Text     string `json:"text"`
	Position int    `json:"position"`
	IsManual bool   `json:"is_manual"`
}

type Message struct {
	MessageID       int64       `json:"message_id"`
	MessageThreadID int64       `json:"message_thread_id"`
	From            *User       `json:"from"`
	Chat            Chat        `json:"chat"`
	Text            string      `json:"text"`
	Caption         string      `json:"caption"`
	Photo           []PhotoSize `json:"photo"`
	Document        *Document   `json:"document"`
	MediaGroupID    string      `json:"media_group_id"`
	ReplyToMessage  *Message    `json:"reply_to_message"`
	Quote           *TextQuote  `json:"quote"`
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type DisabledButton struct{}

type Button struct {
	Text         string          `json:"text"`
	CallbackData string          `json:"callback_data,omitempty"`
	Style        string          `json:"style,omitempty"`
	Disabled     *DisabledButton `json:"disabled,omitempty"`
}

type Keyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

type SendOptions struct {
	ReplyToMessageID int64
	Keyboard         *Keyboard
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// APIError contains only a sanitized server description, never a request URL.
type APIError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram API %d: %s", e.Code, e.Description)
}

// ErrorInfo is a bounded description of a Telegram operation failure.
type ErrorInfo struct {
	Code       int
	Reason     string
	RetryAfter int
	Uncertain  bool
}

type reasonError struct {
	reason string
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }
func (e *reasonError) Unwrap() error { return e.err }

func withReason(reason string, err error) error {
	if err == nil {
		return nil
	}
	return &reasonError{reason: reason, err: err}
}

func timeoutError(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// ClassifyError returns only stable metadata and never copies an error message.
func ClassifyError(err error) ErrorInfo {
	info := ErrorInfo{Reason: reasonUnknown}
	if err == nil {
		return info
	}
	if errors.Is(err, context.Canceled) {
		info.Reason = reasonCancelled
		var delivery *deliveryError
		if errors.As(err, &delivery) && delivery != nil {
			info.Uncertain = delivery.uncertain
		}
		return info
	}
	if errors.Is(err, context.DeadlineExceeded) {
		info.Reason = reasonTimeout
		var delivery *deliveryError
		if errors.As(err, &delivery) && delivery != nil {
			info.Uncertain = delivery.uncertain
		}
		return info
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		info.Code = apiErr.Code
		info.RetryAfter = apiErr.RetryAfter
		info.Uncertain = DeliveryUncertain(err)
		var delivery *deliveryError
		if !errors.As(err, &delivery) {
			info.Uncertain = apiErrorUncertain(apiErr)
		}
		switch {
		case apiErr.Code == http.StatusTooManyRequests:
			info.Reason = reasonRateLimited
		case apiErr.Code == http.StatusBadRequest && messageThreadNotFound(apiErr.Description):
			info.Reason = reasonMessageThreadNotFound
		case apiErr.Code == http.StatusBadRequest && replyTargetNotFound(apiErr.Description):
			info.Reason = reasonReplyTargetNotFound
		case apiErr.Code == http.StatusBadRequest:
			info.Reason = reasonAPIBadRequest
		default:
			info.Reason = reasonAPIError
		}
		return info
	}
	var classified *reasonError
	if errors.As(err, &classified) && classified != nil {
		info.Reason = classified.reason
		info.Uncertain = DeliveryUncertain(err)
		return info
	}
	if timeoutError(err) {
		info.Reason = reasonTimeout
		info.Uncertain = DeliveryUncertain(err)
		return info
	}
	info.Uncertain = DeliveryUncertain(err)
	return info
}

func apiErrorUncertain(err *APIError) bool {
	return err == nil || err.Code < 400 || err.Code > 599 || strings.TrimSpace(err.Description) == ""
}

func messageThreadNotFound(description string) bool {
	switch strings.ToLower(strings.TrimSpace(description)) {
	case "bad request: message thread not found", "message thread not found":
		return true
	default:
		return false
	}
}

func replyTargetNotFound(description string) bool {
	switch strings.ToLower(strings.TrimSpace(description)) {
	case "bad request: reply message not found", "reply message not found", "bad request: message to reply not found", "message to reply not found", "bad request: message to be replied not found", "message to be replied not found":
		return true
	default:
		return false
	}
}

type deliveryError struct {
	error
	uncertain bool
}

func (e *deliveryError) Unwrap() error { return e.error }

// DeliveryUncertain reports whether a failed operation may have been delivered.
// Unclassified errors are conservative; nil means there was no failure.
func DeliveryUncertain(err error) bool {
	if err == nil {
		return false
	}
	var delivery *deliveryError
	if errors.As(err, &delivery) {
		return delivery.uncertain
	}
	return true
}

func deliveryFailure(err error, uncertain bool) error {
	if err == nil {
		return nil
	}
	return &deliveryError{error: err, uncertain: uncertain}
}

type Client struct {
	token   string
	baseURL string
	http    *http.Client
	logger  *slog.Logger
}

func New(token string, logger *slog.Logger) *Client {
	return &Client{
		token:   token,
		baseURL: "https://api.telegram.org",
		http: &http.Client{
			Timeout: 40 * time.Second,
			// Never forward the token or request body to a redirect target.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		logger: logger,
	}
}

func (c *Client) GetMe(ctx context.Context) (User, error) {
	var user User
	err := c.call(ctx, "getMe", struct{}{}, &user, true)
	return user, err
}

func (c *Client) SetCommands(ctx context.Context, commands []BotCommand, languageCode string) error {
	// Replacing the same command list is idempotent, so transport retries are safe.
	return c.call(ctx, "setMyCommands", map[string]any{
		"commands":      commands,
		"scope":         map[string]string{"type": "default"},
		"language_code": languageCode,
	}, nil, true)
}

func (c *Client) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": 30,
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates, true)
	return updates, err
}

func (c *Client) Send(ctx context.Context, chatID, threadID int64, text string, options SendOptions) (Message, error) {
	return sendWithReplyFallback(c.logger, chatID, threadID, options, func(options SendOptions) (Message, error) {
		fields := map[string]any{"chat_id": chatID}
		applyHTML(fields, text)
		if threadID != 0 {
			fields["message_thread_id"] = threadID
		}
		if options.ReplyToMessageID != 0 {
			fields["reply_to_message_id"] = options.ReplyToMessageID
		}
		if options.Keyboard != nil {
			fields["reply_markup"] = options.Keyboard
		}
		var message Message
		err := c.call(ctx, "sendMessage", fields, &message, false)
		if parseEntitiesError(err) {
			applyPlain(fields, text)
			err = c.call(ctx, "sendMessage", fields, &message, false)
		}
		return message, err
	})
}

// applyHTML renders markdown as Telegram HTML. Conversions whose rendered
// text would exceed the message limit fall back to plain text instead of
// risking a rejected send.
func applyHTML(fields map[string]any, text string) {
	html := ConvertMarkdown(text)
	if renderedUTF16Len(html) > MaxMessageUTF16 {
		applyPlain(fields, text)
		return
	}
	fields["text"] = html
	fields["parse_mode"] = "HTML"
}

func applyPlain(fields map[string]any, text string) {
	fields["text"] = text
	delete(fields, "parse_mode")
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// parseEntitiesError reports Telegram's rejection of malformed entity markup.
// Such messages are retried as plain text, so a conversion quirk never
// swallows the reply.
func parseEntitiesError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != http.StatusBadRequest {
		return false
	}
	desc := strings.ToLower(apiErr.Description)
	return strings.Contains(desc, "can't parse entities") || strings.Contains(desc, "unexpected end of") || strings.Contains(desc, "unsupported start tag")
}

func sendWithReplyFallback(logger *slog.Logger, chatID, threadID int64, options SendOptions, send func(SendOptions) (Message, error)) (Message, error) {
	message, err := send(options)
	if err == nil || options.ReplyToMessageID == 0 || !replyRejected(err) {
		return message, err
	}
	logger.Warn("reply target unavailable; retrying without reply", "event", "reply_fallback", "chat_id", chatID, "thread_id", threadID, "reason", reasonReplyTargetNotFound, "api_code", http.StatusBadRequest)
	options.ReplyToMessageID = 0
	return send(options)
}

func replyRejected(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Code == http.StatusBadRequest && replyTargetNotFound(apiErr.Description)
}

func (c *Client) Edit(ctx context.Context, chatID, messageID int64, text string, keyboard *Keyboard) error {
	fields := map[string]any{"chat_id": chatID, "message_id": messageID}
	applyHTML(fields, text)
	if keyboard != nil {
		fields["reply_markup"] = keyboard
	}
	err := c.call(ctx, "editMessageText", fields, nil, false)
	if parseEntitiesError(err) {
		applyPlain(fields, text)
		err = c.call(ctx, "editMessageText", fields, nil, false)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "message is not modified") {
		return nil
	}
	return err
}

// EditForumTopic changes a forum topic's display name.
func (c *Client) EditForumTopic(ctx context.Context, chatID, threadID int64, name string) error {
	err := c.call(ctx, "editForumTopic", map[string]any{
		"chat_id": chatID, "message_thread_id": threadID, "name": name,
	}, nil, true)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == 400 {
		description := strings.ToLower(apiErr.Description)
		if strings.Contains(description, "topic") && strings.Contains(description, "not modified") {
			return nil
		}
	}
	return err
}

// Delete removes a Telegram message without changing durable task state.
func (c *Client) Delete(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil, false)
}

// ClearKeyboard removes an inline keyboard without changing the message text.
func (c *Client) ClearKeyboard(ctx context.Context, chatID, messageID int64) error {
	fields := map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": Keyboard{InlineKeyboard: [][]Button{}},
	}
	return c.call(ctx, "editMessageReplyMarkup", fields, nil, false)
}

func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil, false)
}

func (c *Client) Typing(ctx context.Context, chatID, threadID int64) error {
	fields := map[string]any{"chat_id": chatID, "action": "typing"}
	if threadID != 0 {
		fields["message_thread_id"] = threadID
	}
	return c.call(ctx, "sendChatAction", fields, nil, false)
}

func (c *Client) call(ctx context.Context, method string, fields any, result any, safe bool) error {
	body, err := json.Marshal(fields)
	if err != nil {
		return deliveryFailure(errors.New("telegram: cannot encode request"), false)
	}
	uncertain := false
	for attempt := range 3 {
		retry, delay, err := c.request(ctx, method, body, result, safe)
		uncertain = uncertain || DeliveryUncertain(err)
		if err == nil || !retry || attempt == 2 {
			return deliveryFailure(err, uncertain)
		}
		if delay < 0 {
			delay = time.Duration(attempt+1) * 200 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return deliveryFailure(ctx.Err(), uncertain)
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func (c *Client) request(ctx context.Context, method string, body []byte, result any, safe bool) (bool, time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, deliveryFailure(err, false)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return false, 0, deliveryFailure(errors.New("telegram: invalid API endpoint"), false)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		if timeoutError(err) {
			return safe, -1, withReason(reasonTimeout, errors.New("telegram: transport failed (delivery may be uncertain)"))
		}
		// net/http errors include the credential-bearing URL. Do not wrap them.
		return safe, -1, withReason(reasonTransportFailed, errors.New("telegram: transport failed (delivery may be uncertain)"))
	}
	return c.decodeResponse(ctx, resp, result, safe)
}

func (c *Client) decodeResponse(ctx context.Context, resp *http.Response, result any, safe bool) (bool, time.Duration, error) {
	defer resp.Body.Close()
	const maxResponse = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		if timeoutError(err) {
			return safe, -1, withReason(reasonTimeout, errors.New("telegram: response read failed (delivery may be uncertain)"))
		}
		return safe, -1, withReason(reasonTransportFailed, errors.New("telegram: response read failed (delivery may be uncertain)"))
	}
	if len(data) > maxResponse {
		return false, 0, withReason(reasonAPIError, errors.New("telegram: response exceeds size limit"))
	}
	var envelope struct {
		OK          *bool           `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter *int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return safe && resp.StatusCode >= 500, -1, withReason(reasonAPIError, fmt.Errorf("telegram: invalid API response (HTTP %d)", resp.StatusCode))
	}
	if envelope.OK == nil || !*envelope.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := envelope.Code
		if code == 0 {
			code = resp.StatusCode
		}
		apiErr := &APIError{Code: code, Description: c.sanitize(envelope.Description)}
		if apiErr.Description == "" {
			apiErr.Description = "request rejected"
		}
		// Only an explicit, complete Telegram rejection proves non-delivery.
		uncertain := envelope.OK == nil || *envelope.OK || envelope.Code < 400 || envelope.Code > 599 || strings.TrimSpace(envelope.Description) == ""
		rejection := deliveryFailure(apiErr, uncertain)
		if code == 429 && !uncertain {
			apiErr.RetryAfter = 1
			if envelope.Parameters.RetryAfter != nil {
				apiErr.RetryAfter = *envelope.Parameters.RetryAfter
			}
			// Respect the server's minimum delay; never clamp it and retry early.
			if apiErr.RetryAfter < 0 || apiErr.RetryAfter > 60 {
				return false, 0, rejection
			}
			return true, time.Duration(apiErr.RetryAfter) * time.Second, rejection
		}
		return safe && code >= 500 && code <= 599, -1, rejection
	}
	if result != nil {
		if len(envelope.Result) == 0 || string(envelope.Result) == "null" || json.Unmarshal(envelope.Result, result) != nil {
			return false, 0, withReason(reasonAPIError, errors.New("telegram: invalid API result"))
		}
	}
	return false, 0, nil
}

var responseURL = regexp.MustCompile(`(?i)https?://[^\s<>"']+`)

func (c *Client) sanitize(description string) string {
	if c.token != "" {
		for _, secret := range []string{c.token, url.QueryEscape(c.token), url.PathEscape(c.token)} {
			description = strings.ReplaceAll(description, secret, "[redacted]")
		}
	}
	description = responseURL.ReplaceAllString(description, "[redacted URL]")
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, description)
}
