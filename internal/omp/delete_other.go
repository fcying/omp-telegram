//go:build !linux

package omp

import "golang.org/x/sys/unix"

const deletionDirectoryOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
