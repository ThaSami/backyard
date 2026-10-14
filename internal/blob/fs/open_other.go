//go:build !unix

package fs

import "os"

// readFlags is the flag set used to open objects for reading.
const readFlags = os.O_RDONLY
