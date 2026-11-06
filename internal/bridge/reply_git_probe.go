package bridge

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Every subprocess shares one deadline and one output budget. In particular,
// status never gets to launch its own recursive submodule status probes.
type replyGitProbe struct {
	ctx          context.Context
	environment  []string
	remaining    int
	guards       []string
	drivers      map[string]bool
	repositories []string
}

func (probe *replyGitProbe) run(workspace string, input []byte, arguments ...string) ([]byte, bool) {
	if probe.ctx.Err() != nil || probe.remaining <= 0 {
		return nil, false
	}
	args := []string{"--no-optional-locks", "-c", "core.fsmonitor=false"}
	args = append(args, probe.guards...)
	args = append(args, "-C", workspace)
	args = append(args, arguments...)
	cmd := exec.CommandContext(probe.ctx, "git", args...)
	cmd.Env = probe.environment
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	// Also bound cleanup when a descendant retains an output pipe.
	cmd.WaitDelay = 25 * time.Millisecond
	output := replyGitOutput{limit: probe.remaining}
	cmd.Stdout = &output
	err := cmd.Run()
	probe.remaining -= len(output.data)
	return output.data, err == nil && probe.ctx.Err() == nil && !output.overflow
}

func (probe *replyGitProbe) root(workspace string) (string, bool) {
	output, ok := probe.run(workspace, nil, "rev-parse", "--show-toplevel")
	if !ok || len(output) == 0 || output[len(output)-1] != '\n' {
		return "", false
	}
	root := string(output[:len(output)-1])
	return root, filepath.IsAbs(root)
}

func (probe *replyGitProbe) preflight(repository string) bool {
	for _, visited := range probe.repositories {
		if visited == repository {
			return false
		}
	}
	probe.repositories = append(probe.repositories, repository)
	// These plumbing commands inspect configuration, the index and attributes;
	// none hashes worktree contents or invokes clean/process filters.
	config, ok := probe.run(repository, nil, "config", "--null", "--list")
	if !ok {
		return false
	}
	for len(config) != 0 {
		record, rest, terminated := bytes.Cut(config, []byte{0})
		if !terminated {
			return false
		}
		config = rest
		key, _, _ := strings.Cut(string(record), "\n")
		if !strings.HasPrefix(key, "filter.") {
			continue
		}
		end := strings.LastIndexByte(key, '.')
		if end <= len("filter.") {
			return false
		}
		driver := key[len("filter."):end]
		if probe.drivers == nil {
			probe.drivers = make(map[string]bool)
		}
		if !probe.drivers[driver] {
			probe.drivers[driver] = true
			// Cover changes to attributes or an existing driver's configuration
			// between preflight and status, including inherited submodule config.
			probe.guards = append(probe.guards,
				"-c", "filter."+driver+".clean=", "-c", "filter."+driver+".process=",
				"-c", "filter."+driver+".required=true")
		}
	}
	submodules, ok := probe.attributes(repository)
	if !ok {
		return false
	}
	for _, module := range submodules {
		workspace := filepath.Join(repository, module)
		if _, err := os.Lstat(filepath.Join(workspace, ".git")); os.IsNotExist(err) {
			// An uninitialized submodule has no worktree state to inspect.
			continue
		} else if err != nil {
			return false
		}
		root, ok := probe.root(workspace)
		if !ok || !probe.preflight(root) {
			return false
		}
	}
	return true
}

func (probe *replyGitProbe) attributes(repository string) ([]string, bool) {
	index, ok := probe.run(repository, nil, "ls-files", "--stage", "-z")
	if !ok {
		return nil, false
	}
	var paths []byte
	var submodules []string
	for len(index) != 0 {
		record, rest, terminated := bytes.Cut(index, []byte{0})
		if !terminated {
			return nil, false
		}
		index = rest
		metadata, path, found := bytes.Cut(record, []byte{'\t'})
		if !found || len(path) == 0 {
			return nil, false
		}
		name := string(path)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return nil, false
		}
		if bytes.HasPrefix(metadata, []byte("160000 ")) {
			submodules = append(submodules, name)
			continue
		}
		paths = append(paths, path...)
		paths = append(paths, 0)
	}
	if len(paths) == 0 {
		return submodules, true
	}
	// --all distinguishes absence of an attribute from the literal driver name
	// "unspecified". The protocol cannot distinguish -filter from filter=unset;
	// that ambiguity is harmless unless a driver actually has the name "unset".
	attributes, ok := probe.run(repository, paths, "check-attr", "--all", "--stdin", "-z")
	if !ok {
		return nil, false
	}
	for len(attributes) != 0 {
		_, rest, pathOK := bytes.Cut(attributes, []byte{0})
		attribute, rest, attrOK := bytes.Cut(rest, []byte{0})
		value, remaining, valueOK := bytes.Cut(rest, []byte{0})
		if !pathOK || !attrOK || !valueOK {
			return nil, false
		}
		if string(attribute) == "filter" && (string(value) != "unset" || probe.drivers["unset"]) {
			// Running with conversions disabled could report false clean/dirty.
			// A configured but unused driver does not reach this branch.
			return nil, false
		}
		attributes = remaining
	}
	return submodules, true
}
