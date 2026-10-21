package omp

import "golang.org/x/sys/unix"

// Pin directory identity without requiring permission to read its entries.
const deletionDirectoryOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
