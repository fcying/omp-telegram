package omp

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"time"
)

const resumeModelErrorPrefix = "SessionModelRestoreError: Could not restore model "

// defaultModelArgs replaces model selectors without modifying the caller's argv.
func defaultModelArgs(args []string) []string {
	// A trailing valueless option must not consume the model override.
	result := make([]string, 2, len(args)+2)
	result[0], result[1] = "--model", "@default"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--model" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--model=") {
			continue
		}
		result = append(result, arg)
		// OMP built-in string flags consume even flag-looking values. Keep this
		// list aligned with coding-agent/src/cli/flag-tables.ts and profile selectors.
		switch arg {
		case "--config", "--add-dir", "--fork", "--provider", "--smol", "--slow",
			"--goal", "--plan", "--prewalk-into", "--plan-yolo-into", "--max-time",
			"--warn-cost", "--max-cost", "--service-tier", "--api-key", "--system-prompt",
			"--system-prompt-template", "--append-system-prompt", "--provider-session-id",
			"--prompt-cache-key", "--session-dir", "--models", "--tools", "--thinking",
			"--hook", "--extension", "-e", "--trusted-extension", "--plugin-dir",
			"--skills", "--approval-mode", "--profile", "--alias":
			if i+1 < len(args) {
				i++
				result = append(result, args[i])
			}
		}
	}
	return result
}

// resumeModelDiagnostics retains only a classification bit, never native text.
// The child receives a descriptor, not an os/exec-managed copying writer.
type resumeModelDiagnostics struct {
	reader  *os.File
	done    chan struct{}
	matched bool // Read only after done closes.
}

func openResumeModelDiagnostics(enabled bool) (*os.File, *resumeModelDiagnostics, error) {
	if !enabled {
		file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		return file, nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	d := &resumeModelDiagnostics{reader: reader, done: make(chan struct{})}
	go d.drain()
	return writer, d, nil
}

func (d *resumeModelDiagnostics) drain() {
	defer close(d.done)
	reader := bufio.NewReaderSize(d.reader, 4096)
	first := true
	for {
		fragment, err := reader.ReadSlice('\n')
		if first && bytes.HasPrefix(fragment, []byte(resumeModelErrorPrefix)) {
			d.matched = true
		}
		// Even enormous lines are drained without allocating or inspecting tails.
		first = len(fragment) != 0 && fragment[len(fragment)-1] == '\n'
		if err != nil && err != bufio.ErrBufferFull {
			return
		}
	}
}

// finish is called once, after reaping and killing the original process group.
// A finite drain catches buffered fatal diagnostics without waiting on escaped
// descendants that may still hold (or actively write to) the inherited pipe.
func (d *resumeModelDiagnostics) finish() {
	timer := time.NewTimer(100 * time.Millisecond)
	select {
	case <-d.done:
	case <-timer.C:
		d.reader.Close()
		<-d.done
	}
	timer.Stop()
	d.reader.Close()
}
