package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"omp-telegram/internal/i18n"
)

func TestResolveWorkspace(t *testing.T) {
	base := t.TempDir()
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	project := filepath.Join(home, "my project")
	root := filepath.Join(base, "workspaces")
	for _, dir := range []string{project, filepath.Join(root, "existing")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	projectLink := filepath.Join(base, "project-link")
	homeLink := filepath.Join(base, "home-link")
	rootLink := filepath.Join(base, "root-link")
	for link, target := range map[string]string{projectLink: project, homeLink: home, rootLink: root} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(project, "existing.txt")
	if err := os.WriteFile(marker, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeLink)
	for _, tt := range []struct {
		name  string
		root  string
		input string
		want  string
	}{
		{"existing name", root, "existing", filepath.Join(canonicalBase, "workspaces", "existing")},
		{"missing name", root, "test", filepath.Join(canonicalBase, "workspaces", "test")},
		{"name with spaces", root, "my project", filepath.Join(canonicalBase, "workspaces", "my project")},
		{"missing root", filepath.Join(base, "missing-root"), "test", filepath.Join(canonicalBase, "missing-root", "test")},
		{"symlink root", rootLink, "test", filepath.Join(canonicalBase, "workspaces", "test")},
		{"absolute project", root, project, filepath.Join(canonicalBase, "home", "my project")},
		{"missing absolute", root, filepath.Join(base, "missing", "child"), filepath.Join(canonicalBase, "missing", "child")},
		{"project symlink", root, projectLink, filepath.Join(canonicalBase, "home", "my project")},
		{"missing symlink descendant", root, filepath.Join(projectLink, "new", "child"), filepath.Join(canonicalBase, "home", "my project", "new", "child")},
		{"home project", root, "~/my project", filepath.Join(canonicalBase, "home", "my project")},
		{"missing home descendant", root, "~/new/child", filepath.Join(canonicalBase, "home", "new", "child")},
		{"home root", root, "~", filepath.Join(canonicalBase, "home")},
		{"clean absolute", root, project + "/../my project/.", filepath.Join(canonicalBase, "home", "my project")},
		{"filesystem root", root, string(filepath.Separator), string(filepath.Separator)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveWorkspace(tt.root, tt.input)
			if err != nil || got != tt.want {
				t.Fatalf("workspace = %q, error %v; want %q", got, err, tt.want)
			}
		})
	}
	for _, path := range []string{filepath.Join(root, "test"), filepath.Join(root, "my project"), filepath.Join(base, "missing-root"), filepath.Join(base, "missing"), filepath.Join(project, "new"), filepath.Join(home, "new")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("resolver modified missing path %q: %v", path, err)
		}
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("existing project file changed: %q, %v", data, err)
	}
}

func TestResolveWorkspaceTraversesParentAfterSymlinkResolution(t *testing.T) {
	base := t.TempDir()
	local := filepath.Join(base, "local")
	remote := filepath.Join(base, "remote")
	if err := os.MkdirAll(filepath.Join(remote, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(remote, "project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(local, "project"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(remote, "child"), filepath.Join(local, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := resolveWorkspace(base, local+string(filepath.Separator)+"link"+string(filepath.Separator)+".."+string(filepath.Separator)+"project")
	want := filepath.Join(remote, "project")
	if err != nil || got != want {
		t.Fatalf("workspace = %q, error %v; want %q", got, err, want)
	}
}

func TestResolveWorkspaceRejectsInvalidPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspaces")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", ".", "..", "../escape", "relative/path", "relative\\path", "bad\nname", filepath.Join(base, "bad\npath"), "~otheruser", "$HOME/project", "file", file, filepath.Join(file, "child"), filepath.Join(file, "child", "nested"), dangling, filepath.Join(dangling, "child")} {
		t.Run(input, func(t *testing.T) {
			if got, err := resolveWorkspace(root, input); err == nil || got != "" {
				t.Fatalf("invalid input returned workspace %q, error %v", got, err)
			}
		})
	}
	if got, err := resolveWorkspace("relative-root", base); err == nil || got != "" {
		t.Fatalf("relative root returned workspace %q, error %v", got, err)
	}
}

func TestNamedWorkspaceRejectsSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for name, target := range map[string]string{"outside": outside, "inside": root, "dangling": filepath.Join(outside, "missing")} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveWorkspace(root, name); err == nil || got != "" {
			t.Fatalf("named symlink %q returned workspace %q, error %v", name, got, err)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("outside directory was modified")
	}
}

func TestResolveWorkspaceIgnoresUnrelatedEnvironment(t *testing.T) {
	base := t.TempDir()
	canonicalBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", filepath.Join(base, "missing-environment")} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HOME", value)
			t.Setenv("TMPDIR", value)
			for _, tt := range []struct{ input, want string }{
				{"test", filepath.Join(canonicalBase, "test")},
				{base, canonicalBase},
				{filepath.Join(base, "missing"), filepath.Join(canonicalBase, "missing")},
			} {
				if got, err := resolveWorkspace(base, tt.input); err != nil || got != tt.want {
					t.Fatalf("workspace = %q, error %v; want %q", got, err, tt.want)
				}
			}
		})
	}
}

func TestResolveWorkspaceRejectsInvalidHome(t *testing.T) {
	root := t.TempDir()
	for _, home := range []string{"", "relative-home"} {
		t.Run(home, func(t *testing.T) {
			t.Setenv("HOME", home)
			if got, err := resolveWorkspace(root, "~/project"); err == nil || got != "" {
				t.Fatalf("invalid home returned workspace %q, error %v", got, err)
			}
		})
	}
}

func TestResolveWorkspaceRejectsPermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0700); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWorkspace(root, filepath.Join(locked, "child")); err == nil || got != "" {
		t.Fatalf("inaccessible path returned workspace %q, error %v", got, err)
	}
}

func TestWorkspaceErrorsAreTypedAndOpaqueErrorsStayUnchanged(t *testing.T) {
	w := &worker{b: &Bridge{}}
	w.b.setLanguage(i18n.Chinese)
	root := t.TempDir()
	for _, tc := range []struct {
		root    string
		input   string
		english string
		chinese string
	}{
		{"relative", "project", "The workspace root must be an absolute path.", "工作区根目录必须是绝对路径."},
		{root, "bad\nname", "Workspace paths cannot contain control characters.", "工作区路径不能包含控制字符."},
		{root, "../project", "Use a single workspace name, an absolute project path, or ~/path.", "请使用单个工作区名称, 项目绝对路径或 ~/path."},
	} {
		_, err := resolveWorkspace(tc.root, tc.input)
		if err == nil || err.Error() != tc.english {
			t.Fatalf("workspace error = %v, want stable English %q", err, tc.english)
		}
		if got := w.errorText(err); got != tc.chinese {
			t.Fatalf("localized workspace error = %q, want %q", got, tc.chinese)
		}
		if got := w.errorText(errors.New(tc.english)); got != tc.english {
			t.Fatalf("opaque error was translated: %q", got)
		}
	}
}
