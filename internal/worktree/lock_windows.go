//go:build windows

package worktree

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// pathLock holds an exclusive lock on one workspace path. The overlapped value
// is retained because Windows requires the same one to release the range it was
// acquired with.
type pathLock struct {
	f  *os.File
	ov windows.Overlapped
}

func (l *pathLock) tryLock() (bool, error) {
	err := windows.LockFileEx(windows.Handle(l.f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.ov)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// unlock releases the lock. It is safe to call on a nil lock so callers can
// defer it unconditionally.
func (l *pathLock) unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	_ = l.f.Close()
	l.f = nil
}
