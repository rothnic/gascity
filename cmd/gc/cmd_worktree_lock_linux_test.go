//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// The helper's ready pipe confirms an acquired OS lock, not a scheduling delay.
// This is Cobra/component coverage; native CLI proof belongs to test/acceptance.
func TestCmdWorktreeLockTimeoutHeld(t *testing.T) {
	for _, action := range []string{"ensure", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			clearCityEnv(t)
			t.Setenv("GIT_OPTIONAL_LOCKS", "0")
			repo, base := worktreeTestRepo(t)
			root := t.TempDir()
			opts := managedWorktreeCmdOpts(repo, root, filepath.Join(root, "wt"), base)
			opts.AttemptID = ensureAttemptID(t, &opts)
			if action == "ensure" {
				var stdout, stderr bytes.Buffer
				if code := runWorktreeCleanup(opts, &stdout, &stderr); code != 0 {
					t.Fatalf("cleanup setup exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if _, err := os.Stat(opts.Path); !os.IsNotExist(err) {
					t.Fatalf("ensure target must be absent after setup: %v", err)
				}
			}

			// Discover the persisted lock from this one-path fixture instead
			// of duplicating the product's canonical-path digest algorithm.
			common := testutil.RunGit(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
			locks := filepath.Join(common, "gc-worktree-locks")
			entries, err := os.ReadDir(locks)
			if err != nil || len(entries) != 1 {
				t.Fatalf("one-path fixture must have exactly one persisted lock: %v, %v", entries, err)
			}
			lockFile := filepath.Join(locks, entries[0].Name())
			lockInfo, err := os.Stat(lockFile)
			if err != nil {
				t.Fatal(err)
			}

			sibling := opts
			sibling.Path = filepath.Join(root, "sibling")
			sibling.Branch = "work/gc-sibling"
			sibling.BeadID = "gc-sibling"
			sibling.AttemptID = ""
			ensureAttemptID(t, &sibling)
			if err := os.WriteFile(filepath.Join(sibling.Path, "keep.txt"), []byte("keep sibling WIP\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			siblingGit := testutil.RunGit(t, sibling.Path, "rev-parse", "--absolute-git-dir")
			siblingBefore := worktreeLockFixtureState(t, sibling.Path, siblingGit)
			siblingRef := testutil.RunGit(t, repo, "rev-parse", "refs/heads/"+sibling.Branch)

			holder := startTestDoltFlockHelper(t, repo, lockFile)
			before := worktreeLockFixtureState(t, repo, root)
			args := append(worktreeLockArgs(action, opts), "--lock-timeout=100ms")
			var stdout, stderr bytes.Buffer
			cmd := newWorktreeCmd(&stdout, &stderr)
			cmd.SetArgs(args)
			ctx, cancel := context.WithTimeout(t.Context(), testutil.GoroutineRaceTimeout)
			defer cancel()
			done := make(chan error, 1)
			started := time.Now()
			go func() { done <- cmd.ExecuteContext(ctx) }()
			select {
			case err = <-done:
			case <-ctx.Done():
				// Release and join a broken contender before reporting failure.
				// A harness deadline never earns lock_timeout completion credit.
				_ = holder.stdin.Close()
				_ = holder.cmd.Wait()
				select {
				case <-done:
				case <-time.After(testutil.GoroutineRaceTimeout):
					t.Fatal("contender did not finish after holder release; no completion credit")
				}
				t.Fatal("worktree command exceeded the harness deadline; no lock_timeout credit")
			}
			elapsed := time.Since(started)
			if ctx.Err() != nil || elapsed >= testutil.GoroutineRaceTimeout {
				t.Fatal("completion at or after the harness deadline earns no lock_timeout credit")
			}
			if after := worktreeLockFixtureState(t, repo, root); !reflect.DeepEqual(after, before) {
				t.Error("held-lock refusal changed refs, registration, workspace, or provenance")
			}
			currentInfo, statErr := os.Stat(lockFile)
			if statErr != nil || !os.SameFile(lockInfo, currentInfo) {
				t.Error("held-lock refusal removed or replaced the persisted lock inode")
			}
			var refusal worktreeLockCLIResult
			if decodeErr := json.Unmarshal(stdout.Bytes(), &refusal); decodeErr != nil || err == nil ||
				refusal.SchemaVersion != "1" || refusal.OK || refusal.Command != "worktree "+action ||
				refusal.Error.Code != "lock_timeout" || refusal.Error.Message == "" || refusal.Error.ExitCode != 1 ||
				(action == "cleanup" && (!refusal.CleanupPending || refusal.Removed)) {
				t.Fatalf("refusal=%+v command error=%v JSON error=%v; stdout=%q stderr=%q", refusal, err, decodeErr, stdout.String(), stderr.String())
			}
			if elapsed < 100*time.Millisecond {
				t.Fatalf("lock_timeout returned in %s before the requested 100ms wait", elapsed)
			}

			if err := holder.stdin.Close(); err != nil {
				t.Fatal(err)
			}
			if err := holder.cmd.Wait(); err != nil {
				t.Fatalf("holder exit: %v; output=%s", err, holder.output)
			}
			stdout.Reset()
			stderr.Reset()
			cmd = newWorktreeCmd(&stdout, &stderr)
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("%s after release failed: %v; stdout=%q stderr=%q", action, err, stdout.String(), stderr.String())
			}
			var result worktreeLockCLIResult
			if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil || result.SchemaVersion != "1" || !result.OK ||
				(action == "ensure" && !result.Created) || (action == "cleanup" && !result.Removed) {
				t.Fatalf("%s after release result=%+v JSON error=%v", action, result, decodeErr)
			}
			if !reflect.DeepEqual(worktreeLockFixtureState(t, sibling.Path, siblingGit), siblingBefore) ||
				testutil.RunGit(t, repo, "rev-parse", "refs/heads/"+sibling.Branch) != siblingRef {
				t.Error("operation after release changed sibling work, ref, or provenance")
			}
		})
	}
}

func TestCmdWorktreeCanceledContextDoesNotOpenLock(t *testing.T) {
	for _, action := range []string{"ensure", "cleanup"} {
		t.Run(action, func(t *testing.T) {
			clearCityEnv(t)
			t.Setenv("GIT_OPTIONAL_LOCKS", "0")
			repo, base := worktreeTestRepo(t)
			root := t.TempDir()
			opts := managedWorktreeCmdOpts(repo, root, filepath.Join(root, "wt"), base)
			opts.AttemptID = "unused-attempt"
			before := worktreeLockFixtureState(t, repo, root)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			cmd := newWorktreeCmd(&stdout, &stderr)
			cmd.SetArgs(worktreeLockArgs(action, opts))
			err := cmd.ExecuteContext(ctx)
			if err == nil {
				t.Error("pre-canceled worktree command succeeded")
			}
			var refusal worktreeLockCLIResult
			if err := json.Unmarshal(stdout.Bytes(), &refusal); err != nil || refusal.SchemaVersion != "1" ||
				refusal.OK || refusal.Command != "worktree "+action || refusal.Error.Code != "canceled" ||
				refusal.Error.Message == "" || refusal.Error.ExitCode != 1 ||
				(action == "cleanup" && (!refusal.CleanupPending || refusal.Removed)) {
				t.Errorf("pre-canceled refusal=%+v JSON error=%v; stdout=%q stderr=%q", refusal, err, stdout.String(), stderr.String())
			}
			if after := worktreeLockFixtureState(t, repo, root); !reflect.DeepEqual(after, before) {
				t.Error("pre-canceled command opened a lock or mutated repository/workspace state")
			}
		})
	}
}

type worktreeLockCLIResult struct {
	SchemaVersion  string `json:"schema_version"`
	OK             bool   `json:"ok"`
	Command        string `json:"command"`
	Created        bool   `json:"created"`
	Removed        bool   `json:"removed"`
	CleanupPending bool   `json:"cleanup_pending"`
	Error          struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
}

func worktreeLockArgs(action string, o worktreeCmdOpts) []string {
	args := []string{
		action, "--repo", o.Repo, "--root", o.Root, "--path", o.Path,
		"--branch", o.Branch, "--base", o.Base, "--bead", o.BeadID, "--store-ref", o.StoreRef,
		"--creator", o.Creator, "--owner", o.Owner, "--generation", o.Generation, "--lifecycle", o.Lifecycle, "--json",
	}
	if action == "cleanup" {
		args = append(args, "--attempt-id", o.AttemptID)
	}
	return args
}

// Include bytes, modes, symlinks, refs, registration, and private provenance.
// A path list alone would miss in-place damage to sibling work.
func worktreeLockFixtureState(t *testing.T, roots ...string) map[string]string {
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
			t.Fatalf("snapshot owned fixture %q: %v", root, err)
		}
	}
	return state
}
