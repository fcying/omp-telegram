package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"omp-telegram/internal/i18n"
)

func resolveWorkspace(root, input string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", newMessageError(i18n.Msg.WorkspaceRootMustAbsolutePath)
	}
	if strings.IndexFunc(input, unicode.IsControl) >= 0 {
		return "", newMessageError(i18n.Msg.WorkspacePathsCannotContainControlCharacters)
	}
	if !filepath.IsAbs(input) && !strings.HasPrefix(input, "~") {
		if strings.TrimSpace(input) == "" || input == "." || input == ".." || strings.ContainsAny(input, "/\\") {
			return "", newMessageError(i18n.Msg.UseSingleWorkspaceNameAbsoluteProjectPathPath)
		}
		input = root + string(filepath.Separator) + input
		if info, err := os.Lstat(input); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", newMessageError(i18n.Msg.NamedWorkspaceCannotSymbolicLink)
			}
		} else if !os.IsNotExist(err) {
			return "", newMessageError(i18n.Msg.CannotAccessNamedWorkspace)
		}
	} else if input == "~" || strings.HasPrefix(input, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", newMessageError(i18n.Msg.CannotDetermineAbsoluteUserHomeDirectory)
		}
		input = home + strings.TrimPrefix(input, "~")
	}
	if !filepath.IsAbs(input) {
		return "", newMessageError(i18n.Msg.UseWorkspaceNameAbsoluteProjectPathPath)
	}

	resolved := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(input, string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := os.Lstat(candidate)
		if os.IsNotExist(err) {
			return resolved + string(filepath.Separator) + strings.Join(parts[i:], string(filepath.Separator)), nil
		}
		if err != nil {
			return "", newMessageError(i18n.Msg.CannotAccessProjectPath)
		}
		if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return "", newMessageError(i18n.Msg.ProjectPathMustAccessibleDirectory)
		}
		resolved, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", newMessageError(i18n.Msg.CannotResolveProjectPath)
		}
		info, err = os.Stat(resolved)
		if err != nil || !info.IsDir() {
			return "", newMessageError(i18n.Msg.ProjectPathMustAccessibleDirectory)
		}
	}
	return resolved, nil
}
