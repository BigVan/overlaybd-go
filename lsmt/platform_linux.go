//go:build linux && (amd64 || arm64)

package lsmt

import (
	"fmt"
	"os"

	"github.com/bigvan/overlaybd-go/internal/fileio"
	"golang.org/x/sys/unix"
)

func lockFile(handle uintptr) error {
	return unix.Flock(int(handle), unix.LOCK_EX|unix.LOCK_NB)
}

func openDataFile(path string, engine IOEngine) (dataFile, error) {
	switch engine {
	case PSync:
		return os.OpenFile(path, os.O_RDWR, 0)
	case IOUringBuffered, IOUringDirect:
		return fileio.Open(path, os.O_RDWR, engine == IOUringDirect)
	default:
		return nil, fmt.Errorf("lsmt: unsupported ioEngine %d (use 0, 3 or 4)", engine)
	}
}
