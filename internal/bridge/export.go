package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"omp-telegram/internal/media"
	"omp-telegram/internal/omp"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

var (
	errSessionExportTooLarge = errors.New("session export is too large to send through Telegram")
	errSessionHTMLTimeout    = errors.New("session HTML export timed out")
	exportDirectorySync      = syncExportSpoolDirectory
)

func syncExportSpoolDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

// SnapshotSession copies an absolute OMP session file into the private outbox spool.
// The source is opened read-only and is never returned to the outbox.
func SnapshotSession(ctx context.Context, spoolRoot, sessionFile string) (result media.File, err error) {
	if err = ctx.Err(); err != nil {
		return media.File{}, err
	}
	if !filepath.IsAbs(sessionFile) {
		return media.File{}, errors.New("session file must be absolute")
	}
	source, openErr := os.OpenFile(sessionFile, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if openErr != nil {
		return media.File{}, errors.New("cannot open session file")
	}
	defer source.Close()
	info, statErr := source.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return media.File{}, errors.New("session file must be a regular file")
	}
	if info.Size() > media.MaxDocumentBytes {
		return media.File{}, errSessionExportTooLarge
	}
	spoolRoot, err = filepath.Abs(spoolRoot)
	if err != nil {
		return media.File{}, errors.New("cannot resolve attachment spool")
	}
	if err = os.MkdirAll(spoolRoot, 0700); err != nil {
		return media.File{}, errors.New("cannot create attachment spool")
	}
	target, createErr := os.CreateTemp(spoolRoot, "attachment-*")
	if createErr != nil {
		return media.File{}, errors.New("cannot create session snapshot")
	}
	committed := false
	defer func() {
		_ = target.Close()
		if !committed {
			_ = os.Remove(target.Name())
		}
	}()
	copied, copyErr := io.Copy(target, io.LimitReader(sessionContextReader{ctx: ctx, reader: source}, media.MaxDocumentBytes+1))
	if copyErr != nil {
		if ctx.Err() != nil {
			return media.File{}, ctx.Err()
		}
		return media.File{}, errors.New("cannot copy session snapshot")
	}
	if copied > media.MaxDocumentBytes {
		return media.File{}, errSessionExportTooLarge
	}
	finalInfo, statErr := source.Stat()
	if statErr != nil || !finalInfo.Mode().IsRegular() || finalInfo.Size() != info.Size() || !finalInfo.ModTime().Equal(info.ModTime()) {
		return media.File{}, errors.New("session file changed during snapshot")
	}
	if err = target.Sync(); err != nil {
		return media.File{}, errors.New("cannot persist session snapshot")
	}
	if err = target.Chmod(0400); err != nil {
		return media.File{}, errors.New("cannot protect session snapshot")
	}
	if err = target.Sync(); err != nil {
		return media.File{}, errors.New("cannot persist session snapshot mode")
	}
	if err = target.Close(); err != nil {
		return media.File{}, errors.New("cannot finish session snapshot")
	}
	if err = exportDirectorySync(spoolRoot); err != nil {
		return media.File{}, fmt.Errorf("cannot persist session snapshot directory: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return media.File{}, err
	}
	committed = true
	return media.File{Path: target.Name(), Name: media.SafeFilename(filepath.Base(sessionFile)), Kind: "document"}, nil
}

// snapshotHTML writes native OMP HTML output from a stable bridge-owned source snapshot.
func snapshotHTML(ctx context.Context, binary, sessionFile, spoolRoot, displayName string) (media.File, error) {
	return snapshotHTMLWithLimit(ctx, binary, sessionFile, spoolRoot, displayName, media.MaxDocumentBytes)
}

func snapshotHTMLWithLimit(ctx context.Context, binary, sessionFile, spoolRoot, displayName string, maxBytes int64) (result media.File, err error) {
	if err = ctx.Err(); err != nil {
		return media.File{}, err
	}
	source, err := SnapshotSession(ctx, spoolRoot, sessionFile)
	if err != nil {
		return media.File{}, err
	}
	defer os.Remove(source.Path)

	spoolRoot, err = filepath.Abs(spoolRoot)
	if err != nil {
		return media.File{}, errors.New("cannot resolve attachment spool")
	}
	target, createErr := os.CreateTemp(spoolRoot, "attachment-*")
	if createErr != nil {
		return media.File{}, errors.New("cannot create HTML snapshot")
	}
	targetPath := target.Name()
	result = media.File{Path: targetPath, Name: media.SafeFilename(displayName), Kind: "document"}
	defer func() {
		_ = target.Close()
		if err != nil {
			_ = os.Remove(targetPath)
		}
	}()
	if err = target.Close(); err != nil {
		return media.File{}, errors.New("cannot prepare HTML snapshot")
	}
	if err = exportHTMLWithLimit(ctx, binary, source.Path, targetPath, maxBytes); err != nil {
		return media.File{}, err
	}
	info, statErr := os.Lstat(targetPath)
	if statErr != nil || !info.Mode().IsRegular() {
		return media.File{}, errors.New("native HTML export did not produce a regular file")
	}
	output, openErr := os.OpenFile(targetPath, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if openErr != nil {
		return media.File{}, errors.New("cannot open native HTML export")
	}
	defer output.Close()
	info, statErr = output.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		return media.File{}, errors.New("native HTML export did not produce a regular file")
	}
	if info.Size() == 0 {
		return media.File{}, errors.New("native HTML export produced an empty file")
	}
	if info.Size() > maxBytes {
		return media.File{}, errSessionExportTooLarge
	}
	if err = output.Sync(); err != nil {
		return media.File{}, errors.New("cannot persist HTML snapshot")
	}
	if err = output.Chmod(0400); err != nil {
		return media.File{}, errors.New("cannot protect HTML snapshot")
	}
	if err = output.Sync(); err != nil {
		return media.File{}, errors.New("cannot persist HTML snapshot mode")
	}
	if err = output.Close(); err != nil {
		return media.File{}, errors.New("cannot finish HTML snapshot")
	}
	if err = exportDirectorySync(spoolRoot); err != nil {
		return media.File{}, fmt.Errorf("cannot persist HTML snapshot directory: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return media.File{}, err
	}
	return result, nil
}

func exportHTMLWithLimit(ctx context.Context, binary, sessionFile, outputFile string, maxBytes int64) error {
	exportCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	exceeded := make(chan struct{}, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-exportCtx.Done():
				return
			case <-ticker.C:
				info, statErr := os.Stat(outputFile)
				if statErr == nil && info.Mode().IsRegular() && info.Size() > maxBytes {
					select {
					case exceeded <- struct{}{}:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	err := omp.ExportHTML(exportCtx, binary, sessionFile, outputFile)
	cancel()
	<-monitorDone
	select {
	case <-exceeded:
		return errSessionExportTooLarge
	default:
	}
	if info, statErr := os.Stat(outputFile); statErr == nil && info.Mode().IsRegular() && info.Size() > maxBytes {
		return errSessionExportTooLarge
	}
	return err
}

type sessionContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r sessionContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func exportFormatName(format string) string {
	return format
}

func shortSessionID(id string) string {
	id = media.SafeFilename(id)
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func sessionExportMessage(result exportResult) string {
	if errors.Is(result.err, omp.ErrCustomSessionDir) {
		return "Cannot export an inactive session when omp uses a custom session directory."
	}
	if errors.Is(result.err, errSessionExportTooLarge) {
		return "Session export is too large to send through Telegram."
	}
	if errors.Is(result.err, errSessionHTMLTimeout) {
		return "Session HTML export timed out."
	}
	if result.format == "html" {
		return "Session HTML export failed."
	}
	return "Session export failed."
}

func sessionExportReason(err error) string {
	switch {
	case errors.Is(err, omp.ErrCustomSessionDir):
		return "custom_session_directory"
	case errors.Is(err, errSessionExportTooLarge):
		return "too_large"
	case errors.Is(err, errSessionHTMLTimeout), errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "failed"
	}
}
