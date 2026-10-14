//go:build !windows

package worktree

import (
	"errors"
	"os"
	"syscall"
)

// pathLock holds an exclusive advisory lock on one workspace path.
type pathLock struct{ f *os.File }

func (l *pathLock) tryLock() (bool, error) {
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
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
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
