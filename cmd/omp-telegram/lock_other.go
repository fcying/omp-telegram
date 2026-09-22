//go:build !linux

package main

import (
	"errors"
	"os"
)

func openDaemonLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("daemon lock must not be a symlink")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}
