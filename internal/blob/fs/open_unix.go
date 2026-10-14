//go:build unix

package fs

import (
	"os"
	"syscall"
)

// readFlags opens files non-blocking so that a FIFO or device node planted in
// the tree (rsync --specials) can never hang a reader; regular files ignore
// O_NONBLOCK. Non-regular files are rejected after the open.
const readFlags = os.O_RDONLY | syscall.O_NONBLOCK
