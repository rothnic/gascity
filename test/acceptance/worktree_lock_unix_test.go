//go:build acceptance_a && !windows

package acceptance_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// A separate gc process must refuse a held path lock without changing managed
// work, then complete the same operation after that lock is released.
func TestWorktreeLockTimeoutNative(t *testing.T) {
	for _, action := range []string{"ensure", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("GIT_OPTIONAL_LOCKS", "0")
			env := testEnv.Clone().With("GIT_OPTIONAL_LOCKS", "0").Without("GC_CITY")
			repo := createGitRig(t)
			base := git(t, repo, "rev-parse", "HEAD")
			root := helpers.TempDir(t)
			path := filepath.Join(root, "target")
			branch := "work/gc-lock-test"
			args := func(action, path, branch, attempt string) []string {
				args := []string{
					"worktree", action, "--repo", repo, "--root", root,
					"--path", path, "--branch", branch, "--base", base,
					"--bead", filepath.Base(branch), "--store-ref", "gascity",
					"--creator", "test", "--owner", "gc-sling", "--generation", "1", "--json",
				}
				if action == "cleanup" {
					args = append(args, "--attempt-id", attempt)
				}
				return args
			}
			runSuccess := func(args []string) nativeWorktreeLockResult {
				stdout, stderr, err := helpers.RunGCStreams(env, repo, args...)
				var result nativeWorktreeLockResult
				decodeErr := json.Unmarshal([]byte(stdout), &result)
				if err != nil || decodeErr != nil || result.SchemaVersion != "1" || !result.OK || result.Error != nil {
					t.Fatalf("gc %v: error=%v JSON error=%v result=%+v; stdout=%q stderr=%q", args, err, decodeErr, result, stdout, stderr)
				}
				return result
			}
			created := runSuccess(args("ensure", path, branch, ""))
			if !created.Created || created.Provenance.AttemptID == "" {
				t.Fatalf("ensure setup did not create a managed attempt: %+v", created)
			}
			attempt := created.Provenance.AttemptID
			if action == "ensure" {
				if removed := runSuccess(args("cleanup", path, branch, attempt)); !removed.Removed {
					t.Fatalf("cleanup setup did not remove the target: %+v", removed)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("ensure target must be absent after setup: %v", err)
				}
			}

			// Discover the retained lock instead of copying its digest algorithm.
			common := git(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
			locks := filepath.Join(common, "gc-worktree-locks")
			entries, err := os.ReadDir(locks)
			if err != nil || len(entries) != 1 {
				t.Fatalf("one-path setup must have one retained lock: entries=%v error=%v", entries, err)
			}
			lockPath := filepath.Join(locks, entries[0].Name())
			lockInfo, err := os.Stat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			sibling := filepath.Join(root, "sibling")
			siblingBranch := "work/gc-sibling"
			runSuccess(args("ensure", sibling, siblingBranch, ""))
			if err := os.WriteFile(filepath.Join(sibling, "keep.txt"), []byte("keep sibling WIP\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			siblingGit := git(t, sibling, "rev-parse", "--absolute-git-dir")
			siblingBefore := nativeWorktreeLockState(t, sibling, siblingGit)
			siblingRef := git(t, repo, "rev-parse", "refs/heads/"+siblingBranch)

			lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					if err := lock.Close(); err != nil {
						t.Errorf("release held lock: %v", err)
					}
				}
			}
			t.Cleanup(release)
			// Successful synchronous acquisition is the readiness barrier. The
			// gc child opens a separate descriptor; no scheduling sleep is needed.
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("hold actual path lock: %v", err)
			}
			before := nativeWorktreeLockState(t, repo, root)
			const wait = 100 * time.Millisecond
			contenderArgs := append(args(action, path, branch, attempt), "--lock-timeout="+wait.String())
			type invocation struct {
				stdout, stderr string
				err            error
			}
			done := make(chan invocation, 1)
			started := time.Now()
			guard, cancel := context.WithTimeout(t.Context(), testutil.ExecRaceTimeout)
			defer cancel()
			go func() {
				stdout, stderr, err := helpers.RunGCStreams(env, repo, contenderArgs...)
				done <- invocation{stdout, stderr, err}
			}()
			var outcome invocation
			select {
			case outcome = <-done:
			case <-guard.Done():
				// Join a broken contender after release. A harness timeout never
				// earns credit for the requested acquisition deadline.
				release()
				select {
				case <-done:
				case <-time.After(testutil.ExecRaceTimeout):
					t.Fatal("gc did not exit after holder release; no lock_timeout credit")
				}
				t.Fatal("gc exceeded the harness deadline; no lock_timeout credit")
			}
			elapsed := time.Since(started)
			if guard.Err() != nil || elapsed >= testutil.ExecRaceTimeout {
				t.Fatal("completion at the harness deadline earns no lock_timeout credit")
			}
			var refusal nativeWorktreeLockResult
			decodeErr := json.Unmarshal([]byte(outcome.stdout), &refusal)
			var exitErr *exec.ExitError
			if !errors.As(outcome.err, &exitErr) || exitErr.ExitCode() != 1 || decodeErr != nil ||
				refusal.SchemaVersion != "1" || refusal.OK || refusal.Command != "worktree "+action ||
				refusal.Created || refusal.Removed || refusal.Error == nil || refusal.Error.Code != "lock_timeout" ||
				refusal.Error.Message == "" || refusal.Error.ExitCode != 1 ||
				(action == "cleanup" && !refusal.CleanupPending) {
				t.Fatalf("held-lock refusal: error=%v JSON error=%v result=%+v; stdout=%q stderr=%q", outcome.err, decodeErr, refusal, outcome.stdout, outcome.stderr)
			}
			if elapsed < wait {
				t.Fatalf("lock_timeout returned in %s before the requested %s wait", elapsed, wait)
			}
			t.Logf("native %s refused the held lock after %s", action, elapsed)
			if !maps.Equal(nativeWorktreeLockState(t, repo, root), before) {
				t.Error("refusal changed refs, registration, workspace bytes, or provenance")
			}
			if current, err := os.Stat(lockPath); err != nil || !os.SameFile(lockInfo, current) {
				t.Error("refusal removed or replaced the retained lock inode")
			}

			release()
			result := runSuccess(contenderArgs)
			if result.Command != "worktree "+action || (action == "ensure" && !result.Created) ||
				(action == "cleanup" && (!result.Removed || result.CleanupPending)) {
				t.Fatalf("operation after release did not complete: %+v", result)
			}
			if !maps.Equal(nativeWorktreeLockState(t, sibling, siblingGit), siblingBefore) ||
				git(t, repo, "rev-parse", "refs/heads/"+siblingBranch) != siblingRef {
				t.Error("operation after release changed sibling work, ref, or provenance")
			}
			if current, err := os.Stat(lockPath); err != nil || !os.SameFile(lockInfo, current) {
				t.Error("operation after release removed or replaced the retained lock inode")
			}
		})
	}
}

type nativeWorktreeLockResult struct {
	SchemaVersion  string `json:"schema_version"`
	OK             bool   `json:"ok"`
	Command        string `json:"command"`
	Created        bool   `json:"created"`
	Removed        bool   `json:"removed"`
	CleanupPending bool   `json:"cleanup_pending"`
	Provenance     struct {
		AttemptID string `json:"attempt_id"`
	} `json:"provenance"`
	Error *struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
}

// Capture bytes, modes and symlink targets, including private Git provenance.
func nativeWorktreeLockState(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for i, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			var data []byte
			if info.Mode().IsRegular() {
				data, err = os.ReadFile(path)
			} else if info.Mode()&os.ModeSymlink != 0 {
				var target string
				target, err = os.Readlink(path)
				data = []byte(target)
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err == nil {
				state[fmt.Sprintf("%d/%s", i, rel)] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(data))
			}
			return err
		})
		if err != nil {
			t.Fatalf("snapshot native worktree fixture %q: %v", root, err)
		}
	}
	return state
}
