package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	defaults "omp-telegram"
)

// Content-addressed paths keep a new binary or --check from changing code used
// by an already-running OMP process. Generated files are owned by the bridge.
func prepareMigrationExtension(dataDir string) (string, error) {
	return prepareExtension(dataDir, "omp-telegram-migrate", defaults.MigrationExtension)
}

func prepareSub2apiExtension(dataDir string) (string, error) {
	return prepareExtension(dataDir, "omp-telegram-sub2api", defaults.Sub2apiExtension)
}

func prepareExtension(dataDir, name string, content []byte) (string, error) {
	dir := filepath.Join(dataDir, ".bridge-extensions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("invalid extension directory")
	}
	digest := sha256.Sum256(content)
	path := filepath.Join(dir, name+"-"+hex.EncodeToString(digest[:])+".ts")
	info, err = os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("invalid extension file")
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		cached, readErr := io.ReadAll(io.LimitReader(file, int64(len(content))+1))
		closeErr := file.Close()
		if readErr != nil {
			return "", readErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if bytes.Equal(cached, content) {
			return path, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	file, err := os.CreateTemp(dir, "."+name+"-*.ts")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(content); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}
