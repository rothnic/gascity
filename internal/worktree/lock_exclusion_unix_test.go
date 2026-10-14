//go:build !windows

package worktree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// TestPathLockIsExclusive proves the lock actually excludes, using a
// non-blocking acquisition of the very file lockPath derives so the assertion
// is a fact about the lock rather than about goroutine scheduling. A test that
// raced a blocked goroutine against a held lock could pass without any locking
// at all, whenever the contender simply had not run yet.
func TestPathLockIsExclusive(t *testing.T) {
	repo, _ := initTestRepo(t)
	path := filepath.Join(t.TempDir(), "gc-locked")

	lockFile, err := lockFilePath(repo, path)
	if err != nil {
		t.Fatalf("lockFilePath: %v", err)
	}

	held, err := lockPath(repo, path)
	if err != nil {
		t.Fatalf("lockPath: %v", err)
	}
	if err := tryFlock(t, lockFile); err == nil {
		held.unlock()
		t.Fatal("a second acquisition succeeded while the lock was held")
	}

	held.unlock()
	if err := tryFlock(t, lockFile); err != nil {
		t.Fatalf("acquisition after release failed: %v", err)
	}
}

// tryFlock attempts a non-blocking exclusive lock on an already-derived lock
// file and releases it again on success.
func tryFlock(t *testing.T, lockFile string) error {
	t.Helper()
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("opening lock file %q: %v", lockFile, err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

func TestPathLockWaitBounds(t *testing.T) {
	repo, _ := initTestRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	held, err := lockPath(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()

	for _, tc := range []struct {
		name          string
		timeout       time.Duration
		parentWait    time.Duration
		minWait       time.Duration
		parentExpires bool
	}{
		{"override", 50 * time.Millisecond, 10 * time.Second, 50 * time.Millisecond, false},
		{"earlier caller deadline", time.Second, 50 * time.Millisecond, 50 * time.Millisecond, true},
		{"zero uses bounded default", 0, 40 * time.Second, 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			ctx, cancel := context.WithTimeout(t.Context(), tc.parentWait)
			defer cancel()
			lock, err := lockPathContext(ctx, repo, path, tc.timeout)
			elapsed := time.Since(started)
			if lock != nil {
				lock.unlock()
				t.Fatal("contender acquired a held lock")
			}
			if !errors.Is(err, ErrLockTimeout) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lock error = %v, want timeout retaining DeadlineExceeded", err)
			}
			if elapsed < tc.minWait || (tc.parentExpires && elapsed >= tc.timeout) ||
				(!tc.parentExpires && (elapsed >= tc.parentWait || ctx.Err() != nil)) {
				t.Fatalf("lock waited %s with parent error %v; no acquisition deadline credit", elapsed, ctx.Err())
			}
		})
	}
	held.unlock()
	later, err := lockPath(repo, path)
	if err != nil {
		t.Fatalf("uncontended legacy caller after release: %v", err)
	}
	later.unlock()
}

func TestPathLockAdmissionOutlivesCancellation(t *testing.T) {
	repo, _ := initTestRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	held, err := lockPathContext(ctx, repo, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	cancel()
	lockFile, err := lockFilePath(repo, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryFlock(t, lockFile); err == nil {
		t.Fatal("cancellation released an already-admitted transaction's lock")
	}
	held.unlock()
	if err := tryFlock(t, lockFile); err != nil {
		t.Fatalf("lock remained held after synchronous release: %v", err)
	}
}

func TestPathLockCancellationDuringWait(t *testing.T) {
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	repo, base := initTestRepo(t)
	root := t.TempDir()
	spec := managedSpec(repo, root, filepath.Join(root, "wt"), "work/canceled-wait", base)
	created, err := Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lockPath(repo, spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	lockFile := held.f.Name()
	lockInfo, err := os.Stat(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, repo)
	refs := runGit(t, repo, "show-ref")
	registration := runGit(t, repo, "worktree", "list", "--porcelain")

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var acquired *pathLock
		var acquireErr error
		go func() {
			acquired, acquireErr = lockPathContext(ctx, repo, spec.Path, time.Minute)
			close(done)
		}()
		// Git I/O and the nonblocking OS call cannot durably block. Wait
		// therefore returns only once the contender reaches its wait select.
		synctest.Wait()
		select {
		case <-done:
			if acquired != nil {
				acquired.unlock()
			}
			t.Fatalf("contender returned before cancellation: %v", acquireErr)
		default:
		}
		canceledAt := time.Now()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("contender did not finish after explicit cancellation")
		}
		if acquired != nil {
			acquired.unlock()
			t.Fatal("canceled contender acquired the held lock")
		}
		if !errors.Is(acquireErr, context.Canceled) || errors.Is(acquireErr, ErrLockTimeout) ||
			time.Since(canceledAt) >= testutil.GoroutineRaceTimeout {
			t.Fatalf("waiting cancellation = %v after %s, want bounded context.Canceled", acquireErr, time.Since(canceledAt))
		}
	})
	if err := tryFlock(t, lockFile); !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("cancellation changed the holder's exclusion: %v", err)
	}
	if current, err := os.Stat(lockFile); err != nil || !os.SameFile(lockInfo, current) {
		t.Fatal("cancellation removed or replaced the retained lock inode")
	}
	verified, err := Verify(spec)
	if err != nil || verified.Head != created.Head || !reflect.DeepEqual(verified.Provenance, created.Provenance) ||
		!reflect.DeepEqual(snapshotTree(t, repo), before) || runGit(t, repo, "show-ref") != refs ||
		runGit(t, repo, "worktree", "list", "--porcelain") != registration {
		t.Fatalf("cancellation changed the managed worktree, refs or provenance: %+v, %v", verified, err)
	}
	held.unlock()
	later, err := lockPathContext(t.Context(), repo, spec.Path, time.Second)
	if err != nil {
		t.Fatalf("acquisition after holder release: %v", err)
	}
	later.unlock()
}

func TestPathLockOpenFailures(t *testing.T) {
	repo, _ := initTestRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if lock, err := lockPathContext(ctx, filepath.Join(repo, "missing-repo"), path, time.Minute); lock != nil ||
		err == nil || errors.Is(err, ErrLockTimeout) {
		if lock != nil {
			lock.unlock()
		}
		t.Fatalf("bad repository identity = %v, want immediate identity failure", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "gc-worktree-locks")); !os.IsNotExist(err) {
		t.Fatalf("identity refusal prepared a lock directory: %v", err)
	}

	t.Run("permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission refusal requires an unprivileged user")
		}
		f, err := openLockFile(repo, path)
		if err != nil {
			t.Fatal(err)
		}
		lockFile := f.Name()
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockFile, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(lockFile, 0o644); err != nil {
				t.Errorf("restoring lock permissions: %v", err)
			}
		})
		lock, err := lockPathContext(ctx, repo, path, time.Minute)
		if lock != nil {
			lock.unlock()
		}
		if lock != nil || !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrLockTimeout) {
			t.Fatalf("unwritable lock = %v, want permission refusal without timeout", err)
		}
	})
}

func TestWorktreeMutationsRejectCanceledOrNegativeWait(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	spec := managedSpec(repo, root, filepath.Join(root, "wt"), "work/lock-refusal", base)
	spec.AttemptID = "unused"
	report := Report{Created: true, Provenance: &Provenance{AttemptID: "unused"}}
	before := snapshotTree(t, repo)
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if canceled {
			cancel()
			spec.LockTimeout = 0
		} else {
			spec.LockTimeout = -time.Second
		}
		checks := []struct {
			name string
			run  func() error
		}{
			{"ensure", func() error { _, err := EnsureContext(ctx, spec); return err }},
			{"cleanup", func() error { _, err := CleanupContext(ctx, spec); return err }},
			{"rollback", func() error { return RollbackAttemptContext(ctx, spec, report) }},
		}
		for _, check := range checks {
			err := check.run()
			if err == nil || (canceled && !errors.Is(err, context.Canceled)) ||
				(!canceled && !strings.Contains(err.Error(), "lock timeout must not be negative")) {
				t.Errorf("%s canceled=%v: %v", check.name, canceled, err)
			}
		}
		cancel()
	}
	if !reflect.DeepEqual(snapshotTree(t, repo), before) || len(snapshotDir(t, root)) != 0 {
		t.Fatal("refusal prepared a lock, worktree, provenance, or branch")
	}
}

func TestWorktreeObservationsDoNotWaitForLock(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	spec := managedSpec(repo, root, filepath.Join(root, "wt"), "work/lock-read", base)
	created, err := Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lockPath(repo, spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	before := snapshotTree(t, repo)
	spec.LockTimeout = time.Nanosecond
	if _, err := Verify(spec); err != nil {
		t.Fatalf("read-only Verify waited for the lock: %v", err)
	}
	spec.DryRun = true
	if rep, err := Ensure(spec); err != nil || rep.Created || rep.Provenance == nil ||
		rep.Provenance.AttemptID != created.Provenance.AttemptID {
		t.Fatalf("dry-run reused worktree = %+v, %v", rep, err)
	}
	if !reflect.DeepEqual(snapshotTree(t, repo), before) {
		t.Fatal("read-only operation changed repository paths")
	}
}

func TestRollbackAttemptLockRefusals(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	spec := managedSpec(repo, root, filepath.Join(root, "wt"), "work/lock-rollback", base)
	report, err := Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	held, err := lockPath(repo, spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.unlock()
	spec.LockTimeout = 50 * time.Millisecond
	if err := RollbackAttemptContext(t.Context(), spec, report); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("rollback held-lock refusal = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RollbackAttemptContext(ctx, spec, report); !errors.Is(err, context.Canceled) {
		t.Fatalf("rollback canceled refusal = %v", err)
	}
	if verified, err := Verify(spec); err != nil || !reflect.DeepEqual(verified.Provenance, report.Provenance) {
		t.Fatalf("rollback refusal changed the workspace: %+v, %v", verified, err)
	}
	held.unlock()
	if err := RollbackAttemptContext(t.Context(), spec, report); err != nil {
		t.Fatalf("rollback after release = %v", err)
	}
}

func TestPathLockGitDiscoveryRespectsContext(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			readyPath, releasePath := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
			for _, path := range []string{readyPath, releasePath} {
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ready, err := os.OpenFile(readyPath, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := ready.Close(); err != nil {
					t.Error(err)
				}
			})
			release, err := os.OpenFile(releasePath, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := release.Close(); err != nil {
					t.Error(err)
				}
			})
			common := filepath.Join(dir, "common")
			script := fmt.Sprintf("#!/bin/sh\nprintf ready > %q\nIFS= read -r line < %q\nprintf '%%s\\n' %q\n", readyPath, releasePath, common)
			if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithCancel(t.Context())
			timeout := time.Second // This acquisition deadline is the subject under test.
			defer cancel()
			done, entered := make(chan struct{}), make(chan error, 1)
			var held *pathLock
			var acquireErr error
			go func() {
				held, acquireErr = lockPathContext(ctx, dir, filepath.Join(dir, "workspace"), timeout)
				close(done)
			}()
			go func() { var b [5]byte; _, err := io.ReadFull(ready, b[:]); entered <- err }()
			select {
			case err := <-entered:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				cancel()
				_, _ = release.WriteString("release\n")
				<-done
				t.Fatal("Git discovery did not enter the controlled boundary")
			}
			if mode == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(testutil.GoroutineRaceTimeout):
				_, _ = release.WriteString("release\n")
				<-done
				if held != nil {
					held.unlock()
				}
				t.Fatal("Git discovery outlived acquisition cancellation or deadline")
			}
			if held != nil {
				held.unlock()
				t.Fatal("canceled discovery acquired a lock")
			}
			want := context.DeadlineExceeded
			if mode == "cancel" {
				want = context.Canceled
			}
			if !errors.Is(acquireErr, want) {
				t.Fatalf("discovery error = %v, want %v", acquireErr, want)
			}
			if _, err := os.Stat(filepath.Join(common, "gc-worktree-locks")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("canceled discovery created lock state: %v", err)
			}
		})
	}
}
