package omp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSessionHeader(t *testing.T, path, id, cwd string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"type": "session", "id": id, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeRenderScript(t *testing.T, path string) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"[ \"$1\" = render ] || exit 11\n" +
		"[ \"$2\" = native-id ] || exit 12\n" +
		"[ \"$3\" = -q ] || exit 13\n" +
		"[ \"$4\" = -t ] || exit 14\n" +
		"printf '%s\\n' 'stdout is ignored'\n" +
		"printf 'session  %s\\nopen  fixture\\n' \"$PWD/session.jsonl\" >&2\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSessionPathUsesNativeRender(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, "session.jsonl")
	writeSessionHeader(t, file, "native-id", cwd)
	binary := filepath.Join(root, "omp-render")
	writeRenderScript(t, binary)

	got, err := ResolveSessionPath(context.Background(), Config{Binary: binary, CWD: cwd}, "native-id")
	if err != nil || got != file {
		t.Fatalf("resolved session path = %q, error = %v; want %q", got, err, file)
	}
}

func TestResolveSessionPathPlacesArgsBeforeRender(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, "session.jsonl")
	writeSessionHeader(t, file, "native-id", cwd)
	binary := filepath.Join(root, "omp-render")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = --model ] || exit 11\n" +
		"[ \"$2\" = fixture ] || exit 12\n" +
		"[ \"$3\" = render ] || exit 13\n" +
		"[ \"$4\" = native-id ] || exit 14\n" +
		"[ \"$5\" = -q ] || exit 15\n" +
		"[ \"$6\" = -t ] || exit 16\n" +
		"printf 'session  %s\\n' \"$PWD/session.jsonl\" >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveSessionPath(context.Background(), Config{Binary: binary, CWD: cwd, Args: []string{"--model", "fixture"}}, "native-id")
	if err != nil || got != file {
		t.Fatalf("resolved session path = %q, error = %v; want %q", got, err, file)
	}
}

func TestResolveSessionPathRequiresFirstDiagnosticLine(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(cwd, "session.jsonl")
	writeSessionHeader(t, file, "native-id", cwd)
	binary := filepath.Join(root, "omp-render")
	script := "#!/bin/sh\nprintf '%s\\n' 'unexpected diagnostic' >&2\nprintf 'session  %s\\n' \"$PWD/session.jsonl\" >&2\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSessionPath(context.Background(), Config{Binary: binary, CWD: cwd}, "native-id"); err == nil {
		t.Fatal("resolver accepted a non-session first diagnostic line")
	}
}

func TestResolveSessionPathVerifiesHeaderIdentity(t *testing.T) {
	for _, test := range []struct {
		name, id, cwd string
	}{
		{name: "id", id: "different-id"},
		{name: "cwd", id: "native-id", cwd: "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "workspace")
			if err := os.Mkdir(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			headerCWD := cwd
			if test.cwd == "other" {
				headerCWD = t.TempDir()
			}
			file := filepath.Join(cwd, "session.jsonl")
			writeSessionHeader(t, file, test.id, headerCWD)
			binary := filepath.Join(root, "omp-render")
			writeRenderScript(t, binary)
			if _, err := ResolveSessionPath(context.Background(), Config{Binary: binary, CWD: cwd}, "native-id"); err == nil {
				t.Fatal("resolver accepted a mismatched session header")
			}
		})
	}
}

func TestResolveSessionPathRejectsCLIOptionSessionID(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSessionPath(context.Background(), Config{CWD: filepath.Join(root, "workspace")}, "--print"); err == nil {
		t.Fatal("resolver accepted a session ID that starts with a CLI option")
	}
}

func TestResolveSessionPathRejectsCustomSessionDirectory(t *testing.T) {
	for _, test := range []struct {
		name, env string
		args      []string
	}{
		{name: "separate argument", args: []string{"--session-dir", "/tmp/native-store"}},
		{name: "equals argument", args: []string{"--session-dir=/tmp/native-store"}},
		{name: "environment", env: "/tmp/native-store"},
		{name: "whitespace environment", env: " "},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cwd := filepath.Join(root, "workspace")
			if err := os.Mkdir(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "executed")
			binary := filepath.Join(root, "omp-render")
			script := "#!/bin/sh\nprintf executed > \"$OMP_RENDER_MARKER\"\nexit 17\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("OMP_RENDER_MARKER", marker)
			t.Setenv("PI_CODING_AGENT_SESSION_DIR", test.env)

			_, err := ResolveSessionPath(context.Background(), Config{Binary: binary, CWD: cwd, Args: test.args}, "native-id")
			if !errors.Is(err, ErrCustomSessionDir) {
				t.Fatalf("custom session directory error = %v, want %v", err, ErrCustomSessionDir)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("render process ran despite custom session directory, stat error=%v", err)
			}
		})
	}
}

func TestExportHTMLDelegatesToNativeCLI(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(input, []byte(`{"native":true}
`), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output.html")
	binary := filepath.Join(root, "omp-export")
	script := "#!/bin/sh\n" +
		"[ \"$#\" -eq 3 ] || exit 11\n" +
		"[ \"$1\" = --export ] || exit 12\n" +
		"[ \"$2\" = " + input + " ] || exit 13\n" +
		"printf '%s' '<html>native</html>' > \"$3\"\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ExportHTML(context.Background(), binary, input, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "<html>native</html>" {
		t.Fatalf("native export output = %q, error = %v", data, err)
	}
}

func TestExportHTMLHonorsContextCancellation(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "session.jsonl")
	output := filepath.Join(root, "output.html")
	if err := os.WriteFile(input, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "omp-export")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nsleep 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := ExportHTML(ctx, binary, input, output); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled HTML export error = %v", err)
	}
}
