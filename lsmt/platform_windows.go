package lsmt

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(handle uintptr) error {
	return windows.LockFileEx(windows.Handle(handle), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 0xffffffff, 0xffffffff, &windows.Overlapped{})
}

func openDataFile(path string, engine IOEngine) (dataFile, error) {
	if engine != PSync {
		return nil, fmt.Errorf("lsmt: Windows supports only ioEngine 0 (buffered file I/O), got %d", engine)
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}
