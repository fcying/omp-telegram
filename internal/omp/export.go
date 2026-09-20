package omp

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
)

// ExportHTML delegates HTML rendering to the native omp CLI.
// It intentionally does not pass bridge RPC, cwd, resume, or omp.args options.
func ExportHTML(ctx context.Context, binary, sessionFile, outputFile string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if binary == "" {
		binary = "omp"
	}
	if !filepath.IsAbs(sessionFile) || !filepath.IsAbs(outputFile) {
		return errors.New("omp: HTML export requires absolute paths")
	}
	cmd := exec.Command(binary, "--export", sessionFile, outputFile)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := runProcess(ctx, cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("omp: native HTML export failed")
	}
	return nil
}

func runProcess(ctx context.Context, cmd *exec.Cmd) error {
	waited, err := startProcess(cmd)
	if err != nil {
		return err
	}
	select {
	case err := <-waited:
		killProcessGroup(cmd.Process)
		return err
	case <-ctx.Done():
		killProcessGroup(cmd.Process)
		<-waited
		return ctx.Err()
	}
}
