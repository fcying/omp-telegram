package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// Download saves a Telegram file exclusively to destination. Partial files are removed.
func (c *Client) Download(ctx context.Context, fileID, destination string, maxBytes int64) (err error) {
	if maxBytes <= 0 || maxBytes > 20_000_000 {
		return errors.New("telegram: download limit must be between 1 and 20000000 bytes")
	}
	var metadata struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err = c.call(ctx, "getFile", map[string]string{"file_id": fileID}, &metadata, true); err != nil {
		return err
	}
	if metadata.Size > maxBytes {
		return errors.New("telegram: file exceeds download limit")
	}
	// Telegram paths are relative ASCII path segments, not URLs or escaped URLs.
	if !validFilePath(metadata.Path) {
		return errors.New("telegram: invalid file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/file/bot"+c.token+"/"+metadata.Path, nil)
	if err != nil {
		return errors.New("telegram: invalid file endpoint")
	}
	client := *c.http
	client.Timeout = 5 * time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if timeoutError(err) {
			return withReason(reasonTimeout, errors.New("telegram: download transport failed"))
		}
		return withReason(reasonTransportFailed, errors.New("telegram: download transport failed"))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: download rejected (HTTP %d)", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return errors.New("telegram: file exceeds download limit")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("telegram: cannot create download destination")
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = errors.New("telegram: cannot close download")
		}
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	n, copyErr := io.Copy(file, io.LimitReader(resp.Body, maxBytes+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if n > maxBytes {
		return errors.New("telegram: file exceeds download limit")
	}
	if copyErr != nil {
		return errors.New("telegram: download read or write failed")
	}
	return nil
}

func validFilePath(path string) bool {
	if path == "" {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.') {
				return false
			}
		}
	}
	return true
}

// SendFile streams a private local snapshot. Only explicit rate limits may retry.
func (c *Client) SendFile(ctx context.Context, chatID, threadID int64, kind, path, name, caption string, options SendOptions) (Message, error) {
	return sendWithReplyFallback(c.logger, chatID, threadID, options, func(options SendOptions) (Message, error) {
		return c.sendFile(ctx, chatID, threadID, kind, path, name, caption, options.ReplyToMessageID)
	})
}

func (c *Client) sendFile(ctx context.Context, chatID, threadID int64, kind, path, name, caption string, replyTo int64) (Message, error) {
	var message Message
	limit := int64(50_000_000)
	method := "sendDocument"
	if kind == "photo" {
		limit, method = 10_000_000, "sendPhoto"
	} else if kind != "document" {
		return message, deliveryFailure(errors.New("telegram: unsupported attachment kind"), false)
	}
	if !filepath.IsAbs(path) {
		return message, deliveryFailure(errors.New("telegram: attachment path must be absolute"), false)
	}
	if len(utf16.Encode([]rune(caption))) > 1024 {
		return message, deliveryFailure(errors.New("telegram: caption exceeds 1024 UTF-16 units"), false)
	}
	if name == "" {
		name = filepath.Base(path)
	}
	if strings.ContainsAny(name, "\r\n\x00") {
		return message, deliveryFailure(errors.New("telegram: invalid attachment name"), false)
	}
	uncertain := false
	for attempt := range 3 {
		file, err := openUpload(path, kind, limit)
		if err != nil {
			return message, deliveryFailure(err, uncertain)
		}
		retry, delay, err := c.upload(ctx, file, chatID, threadID, method, kind, name, caption, replyTo, limit, &message)
		_ = file.Close()
		uncertain = uncertain || DeliveryUncertain(err)
		if err == nil || !retry || attempt == 2 {
			return message, deliveryFailure(err, uncertain)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return message, deliveryFailure(ctx.Err(), uncertain)
		case <-timer.C:
		}
	}
	panic("unreachable")
}

func openUpload(path, kind string, limit int64) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("telegram: attachment must be a regular file")
	}
	// NONBLOCK prevents a substituted FIFO from blocking before the fstat check.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("telegram: cannot open attachment")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() > limit {
		file.Close()
		return nil, errors.New("telegram: attachment is unsafe or exceeds size limit")
	}
	if kind == "photo" {
		config, format, err := image.DecodeConfig(file)
		if err != nil || (format != "jpeg" && format != "png") || config.Width <= 0 || config.Height <= 0 || config.Width > 10000 || config.Height > 10000 || config.Width+config.Height > 10000 || int64(config.Width) > int64(config.Height)*20 || int64(config.Height) > int64(config.Width)*20 {
			file.Close()
			return nil, errors.New("telegram: photo must be JPEG/PNG with dimension sum <=10000 and ratio <=20")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			file.Close()
			return nil, errors.New("telegram: cannot rewind attachment")
		}
	}
	return file, nil
}

func (c *Client) upload(ctx context.Context, file *os.File, chatID, threadID int64, method, kind, name, caption string, replyTo, limit int64, result *Message) (bool, time.Duration, error) {
	if ctx.Err() != nil {
		return false, 0, deliveryFailure(ctx.Err(), false)
	}
	reader, writer := io.Pipe()
	multipartWriter := multipart.NewWriter(writer)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.token+"/"+method, reader)
	if err != nil {
		reader.Close()
		writer.Close()
		return false, 0, deliveryFailure(errors.New("telegram: invalid API endpoint"), false)
	}
	req.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	done := make(chan error, 1)
	go func() {
		writeErr := multipartWriter.WriteField("chat_id", strconv.FormatInt(chatID, 10))
		if writeErr == nil && threadID != 0 {
			writeErr = multipartWriter.WriteField("message_thread_id", strconv.FormatInt(threadID, 10))
		}
		if writeErr == nil && replyTo != 0 {
			var parameters []byte
			parameters, writeErr = json.Marshal(map[string]int64{"message_id": replyTo})
			if writeErr == nil {
				writeErr = multipartWriter.WriteField("reply_parameters", string(parameters))
			}
		}
		if writeErr == nil && caption != "" {
			writeErr = multipartWriter.WriteField("caption", caption)
		}
		if writeErr == nil {
			var part io.Writer
			part, writeErr = multipartWriter.CreateFormFile(kind, name)
			if writeErr == nil {
				var n int64
				n, writeErr = io.Copy(part, io.LimitReader(file, limit+1))
				if n > limit {
					writeErr = errors.New("telegram: attachment grew beyond size limit")
				}
			}
		}
		if writeErr == nil {
			writeErr = multipartWriter.Close()
		}
		_ = writer.CloseWithError(writeErr)
		done <- writeErr
	}()
	client := *c.http
	client.Timeout = 5 * time.Minute
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, requestErr := client.Do(req)
	// Closing the reader releases a writer even when the server rejects early.
	_ = reader.Close()
	writeErr := <-done
	if requestErr != nil {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		if timeoutError(requestErr) {
			return false, 0, withReason(reasonTimeout, errors.New("telegram: upload transport failed (delivery may be uncertain)"))
		}
		return false, 0, withReason(reasonTransportFailed, errors.New("telegram: upload transport failed (delivery may be uncertain)"))
	}
	retry, delay, responseErr := c.decodeResponse(ctx, resp, result, false)
	if responseErr != nil {
		return retry, delay, responseErr
	}
	if writeErr != nil {
		return false, 0, withReason(reasonTransportFailed, errors.New("telegram: upload interrupted (delivery may be uncertain)"))
	}
	return false, 0, nil
}
