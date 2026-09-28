package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const incomingDirectoryPrefix = "incoming-"

const incomingOwnerFilename = ".owner.json"
const maxIncomingOwnerSize = 16 << 10
const incomingPreparingFilename = ".preparing"
const incomingPreparingContents = "omp-telegram incoming preparation\n"

// IncomingOwner identifies the native OMP session that received an attachment.
type IncomingOwner struct {
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace"`
}

// WriteIncomingOwner stores session identity beside a bridge-owned inbox directory.
func WriteIncomingOwner(dataDir, directory string, owner IncomingOwner) error {
	if owner.SessionID == "" || len(owner.SessionID) > 4096 || !filepath.IsAbs(owner.Workspace) {
		return errors.New("invalid incoming attachment owner")
	}
	root, inboxPath, err := openIncomingRoot(dataDir, false)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, ok := incomingRelativePath(inboxPath, directory)
	if !ok {
		return errors.New("invalid incoming attachment directory")
	}
	info, err := root.Lstat(rel)
	if err != nil || !info.IsDir() {
		return errors.New("invalid incoming attachment directory")
	}
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	if len(data)+1 > maxIncomingOwnerSize {
		return errors.New("invalid incoming attachment owner")
	}
	file, err := root.OpenFile(filepath.Join(rel, incomingOwnerFilename), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	directoryFile, err := root.Open(rel)
	if err != nil {
		return err
	}
	syncErr := directoryFile.Sync()
	closeErr = directoryFile.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	err = root.Remove(filepath.Join(rel, incomingPreparingFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readIncomingOwner(root *os.Root, directory string) (IncomingOwner, bool, error) {
	path := filepath.Join(directory, incomingOwnerFilename)
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return IncomingOwner{}, false, nil
	}
	if err != nil {
		return IncomingOwner{}, false, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxIncomingOwnerSize {
		return IncomingOwner{}, false, nil
	}
	file, err := root.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return IncomingOwner{}, false, nil
	}
	if err != nil {
		return IncomingOwner{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxIncomingOwnerSize+1))
	if err != nil {
		return IncomingOwner{}, false, err
	}
	if len(data) > maxIncomingOwnerSize {
		return IncomingOwner{}, false, nil
	}
	var owner IncomingOwner
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&owner) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) || owner.SessionID == "" || len(owner.SessionID) > 4096 || !filepath.IsAbs(owner.Workspace) {
		return IncomingOwner{}, false, nil
	}
	return owner, true, nil
}

func readIncomingPreparing(root *os.Root, directory string) (bool, error) {
	path := filepath.Join(directory, incomingPreparingFilename)
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(incomingPreparingContents)) {
		return false, nil
	}
	file, err := root.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(len(incomingPreparingContents)+1)))
	return err == nil && string(data) == incomingPreparingContents, err
}

func openIncomingRoot(dataDir string, create bool) (*os.Root, string, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, "", err
	}
	inboxPath := filepath.Join(dataDir, "attachments", "inbox")
	dataRoot, err := os.OpenRoot(dataDir)
	if err != nil {
		return nil, inboxPath, err
	}
	defer dataRoot.Close()

	attachmentsRoot, err := openChildDirectory(dataRoot, "attachments", create)
	if err != nil {
		return nil, inboxPath, err
	}
	defer attachmentsRoot.Close()

	inboxRoot, err := openChildDirectory(attachmentsRoot, "inbox", create)
	if err != nil {
		return nil, inboxPath, err
	}
	return inboxRoot, inboxPath, nil
}

func openChildDirectory(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("incoming attachment path is not a directory")
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	if err = directory.Chmod(0700); err != nil {
		directory.Close()
		root.Close()
		return nil, err
	}
	if err = directory.Close(); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}

// RemoveIncoming removes one bridge-owned directory directly under attachments/inbox.
func RemoveIncoming(dataDir, directory string) error {
	root, inboxPath, err := openIncomingRoot(dataDir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	rel, ok := incomingRelativePath(inboxPath, directory)
	if !ok {
		return nil
	}
	info, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return root.RemoveAll(rel)
}

// TouchIncoming resets retention age when the owning OMP task reaches a terminal state.
func TouchIncoming(dataDir, directory string, timestamp time.Time) error {
	root, inboxPath, err := openIncomingRoot(dataDir, false)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, ok := incomingRelativePath(inboxPath, directory)
	if !ok {
		return nil
	}
	info, err := root.Lstat(rel)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return root.Chtimes(rel, timestamp, timestamp)
}

// CleanupIncoming checks expired directories and delegates final eligibility and removal atomically.
func CleanupIncoming(ctx context.Context, dataDir string, cutoff time.Time, inUse map[string]struct{}, removeEligible func(IncomingOwner, func() error) (bool, error)) (int, error) {
	root, inboxPath, err := openIncomingRoot(dataDir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return 0, err
	}

	removed := 0
	var cleanupErrors []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(cleanupErrors, err)...)
		}
		if !strings.HasPrefix(entry.Name(), incomingDirectoryPrefix) {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, err)
			}
			continue
		}
		if !info.IsDir() || !info.ModTime().Before(cutoff) {
			continue
		}
		path := filepath.Join(inboxPath, entry.Name())
		if _, retained := inUse[path]; retained || removeEligible == nil {
			continue
		}
		owner, valid, err := readIncomingOwner(root, entry.Name())
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if !valid {
			marked, markerErr := readIncomingPreparing(root, entry.Name())
			if markerErr != nil {
				cleanupErrors = append(cleanupErrors, markerErr)
				continue
			}
			if marked {
				if err := ctx.Err(); err != nil {
					return removed, errors.Join(append(cleanupErrors, err)...)
				}
				if err := root.RemoveAll(entry.Name()); err != nil {
					cleanupErrors = append(cleanupErrors, err)
				} else {
					removed++
				}
			}
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(cleanupErrors, err)...)
		}
		deleted, err := removeEligible(owner, func() error { return root.RemoveAll(entry.Name()) })
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		if deleted {
			removed++
		}
	}
	return removed, errors.Join(cleanupErrors...)
}

func incomingRelativePath(inboxPath, directory string) (string, bool) {
	rel, err := filepath.Rel(inboxPath, directory)
	if err != nil || !filepath.IsLocal(rel) || filepath.Dir(rel) != "." || !strings.HasPrefix(filepath.Base(rel), incomingDirectoryPrefix) {
		return "", false
	}
	return rel, true
}
