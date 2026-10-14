package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/git"
)

// TestCleanupRemovesSquashMergedWorktreeAfterRemoteBranchDeleted is the
// regression test for the gate this package previously got wrong.
//
// The normal end state of a merged bead is: the branch is squash-merged into
// the base and then deleted from the remote. From that moment no
// remote-tracking ref reaches the worktree's HEAD ever again, so a gate that
// asks "is this pushed?" latches on permanently and cleanup refuses forever,
// leaking a tree per merged bead. Asking "would removing this orphan commits?"
// is the question that actually protects work, and it answers no here because
// `git worktree remove` deletes the checkout rather than refs/heads (#4816).
func TestCleanupRemovesSquashMergedWorktreeAfterRemoteBranchDeleted(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-merged")
	spec := managedSpec(repo, root, wt, "work/gc-merged", base)
	rep, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	spec.AttemptID = rep.Provenance.AttemptID

	// The end state after a squash-merge with the remote branch deleted: the
	// worktree HEAD is contained in the base, and NO remote-tracking ref
	// reaches it. The repo deliberately has no remote refs, which is that
	// condition in its simplest form.
	if refs := runGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/remotes"); refs != "" {
		t.Fatalf("precondition: expected no remote-tracking refs, got %q", refs)
	}

	// A push-state gate refuses here forever, because nothing on a remote
	// reaches HEAD. A reachability gate allows it, because the local base
	// branch still reaches HEAD and removing the checkout orphans nothing.
	report, cleanupErr := Cleanup(spec)
	if cleanupErr != nil {
		t.Fatalf("Cleanup refused a merged worktree whose remote branch was deleted: %+v (%v)", report, cleanupErr)
	}
	if !report.Removed || report.CleanupPending {
		t.Fatalf("Cleanup report = %+v, want removed with no pending action", report)
	}
	if _, statErr := os.Stat(wt); !os.IsNotExist(statErr) {
		t.Fatalf("worktree still present after cleanup: %v", statErr)
	}
}

// TestRollbackAttemptRefusesToOrphanCommittedWork covers the data-loss path a
// working-tree check cannot see. An agent that COMMITS leaves a clean status,
// so the dirty-worktree guard passes; only a delete that git can prove is safe
// keeps the commit from being orphaned when the branch ref is removed.
func TestRollbackAttemptRefusesToOrphanCommittedWork(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "gc-committed")
	spec := managedSpec(repo, root, wt, "work/gc-committed", base)
	report, err := Ensure(spec)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	runGit(t, wt, "commit", "--allow-empty", "-m", "agent work, committed not pushed")
	committed := runGit(t, wt, "rev-parse", "HEAD")

	status := runGit(t, wt, "status", "--porcelain")
	if status != "" {
		t.Fatalf("precondition: committed work should leave a clean status, got %q", status)
	}

	if err := RollbackAttempt(spec, report); err == nil {
		t.Fatal("RollbackAttempt deleted a branch carrying committed work, want refusal")
	}

	// The commit must still be reachable from the branch.
	if got := runGit(t, repo, "rev-parse", "work/gc-committed"); got != committed {
		t.Fatalf("branch tip = %q, want the committed work %q to survive rollback", got, committed)
	}
}

// A detached checkout has no branch to keep committed work reachable after
// removal. Exercise the two rollback paths that can run before provenance is
// published, using real Git without depending on detached Ensure support.
func TestDetachedRollbackPreservesCommittedWork(t *testing.T) {
	for _, kind := range []string{"post-create", "failed-create"} {
		t.Run(kind, func(t *testing.T) {
			repo, _ := initTestRepo(t)
			base := runGit(t, repo, "rev-parse", "HEAD")
			path := filepath.Join(t.TempDir(), "detached")
			runGit(t, repo, "worktree", "add", "--detach", path, base)
			runGit(t, path, "commit", "--allow-empty", "-m", "committed work without a branch")
			head := runGit(t, path, "rev-parse", "HEAD")
			if status := runGit(t, path, "status", "--porcelain"); status != "" {
				t.Fatalf("committed work is dirty: %q", status)
			}
			if refs := runGit(t, repo, "for-each-ref", "--contains", head, "--format=%(refname)", "refs/heads", "refs/tags", "refs/remotes"); refs != "" {
				t.Fatalf("setup left a ref protecting the new commit: %q", refs)
			}

			var err error
			if kind == "post-create" {
				err = rollbackCreated(git.New(repo), Spec{RepoDir: repo, Path: path, Mode: ModeDetached}, false, base, nil)
			} else {
				err = rollbackAfterFailedCreate(git.New(repo), Spec{RepoDir: repo, Path: path, Mode: ModeDetached}, false, base)
			}
			if err == nil {
				t.Fatal("rollback removed a clean detached checkout with committed work")
			}
			if got := runGit(t, path, "rev-parse", "HEAD"); got != head {
				t.Fatalf("rollback changed HEAD to %s, want preserved %s", got, head)
			}
			matches, err := matchingRegisteredWorktrees(git.New(repo), path)
			if err != nil || len(matches) != 1 || matches[0].Head != head || matches[0].Branch != "" {
				t.Fatalf("registration after refusal = %+v, %v", matches, err)
			}
		})
	}
}

// A direct Git user can occupy the requested path after Ensure observes it
// missing. The same base and an absent provenance file do not prove that this
// attempt owns even a clean, untouched checkout.
func TestDetachedRollbackRetainsUnownedBase(t *testing.T) {
	for _, kind := range []string{"post-create", "failed-create"} {
		t.Run(kind, func(t *testing.T) {
			repo, _ := initTestRepo(t)
			base := runGit(t, repo, "rev-parse", "HEAD")
			root := t.TempDir()
			spec := managedSpec(repo, root, filepath.Join(root, "detached"), "", base)
			spec.Mode = ModeDetached
			runGit(t, repo, "worktree", "add", "--detach", spec.Path, base)
			manifest, err := provenanceFilePath(spec.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(manifest); !os.IsNotExist(err) {
				t.Fatalf("foreign checkout has provenance: %v", err)
			}
			if status := runGit(t, spec.Path, "status", "--porcelain"); status != "" {
				t.Fatalf("foreign checkout is not untouched: %q", status)
			}
			refs := runGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)")
			registration := runGit(t, repo, "worktree", "list", "--porcelain")
			if kind == "failed-create" {
				if err := git.New(repo).WorktreeAddDetached(spec.Path, base); err == nil {
					t.Fatal("a second add unexpectedly acquired the foreign checkout")
				}
			}
			err = nil
			if kind == "post-create" {
				err = rollbackCreated(git.New(repo), spec, false, base, nil)
			} else {
				err = rollbackAfterFailedCreate(git.New(repo), spec, false, base)
			}
			if err == nil {
				t.Error("rollback accepted an unowned same-base checkout, want explicit ownership refusal")
			} else if !strings.Contains(err.Error(), "refus") {
				t.Errorf("rollback error = %v, want explicit refusal", err)
			}
			if _, err := os.Stat(spec.Path); err != nil {
				t.Fatalf("foreign checkout was not preserved: %v", err)
			}
			if got := runGit(t, spec.Path, "rev-parse", "HEAD"); got != base {
				t.Fatalf("foreign HEAD = %s, want preserved %s", got, base)
			}
			if got := runGit(t, repo, "worktree", "list", "--porcelain"); got != registration {
				t.Fatalf("rollback changed foreign registration: %q, want %q", got, registration)
			}
			if got := runGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
				t.Fatalf("rollback changed refs: %q, want %q", got, refs)
			}
			if _, err := os.Stat(manifest); !os.IsNotExist(err) {
				t.Fatalf("rollback published foreign provenance: %v", err)
			}
		})
	}
}

func TestDetachedCleanupAndRollbackPreserveAdvancedWork(t *testing.T) {
	for _, kind := range []string{"attempt", "post-create with own provenance"} {
		t.Run("untouched "+kind, func(t *testing.T) {
			repo, base := initTestRepo(t)
			root := t.TempDir()
			spec := managedSpec(repo, root, filepath.Join(root, "detached"), "", base)
			spec.Mode = ModeDetached
			refs := runGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)")
			report, err := Ensure(spec)
			if err != nil {
				t.Fatal(err)
			}
			if report.Provenance == nil || report.Provenance.AttemptID == "" {
				t.Fatal("untouched removal control lacks a published current attempt")
			}
			// Even a caller-supplied flag cannot authorize branch deletion in
			// detached mode; there was no attempt-created branch.
			report.BranchCreated = true
			if kind == "attempt" {
				err = RollbackAttempt(spec, report)
			} else {
				err = rollbackCreated(git.New(repo), spec, true, report.Provenance.BaseSHA, report.Provenance)
			}
			if err != nil {
				t.Fatalf("rollback untouched own attempt: %v", err)
			}
			if _, err := os.Stat(spec.Path); !os.IsNotExist(err) {
				t.Fatalf("untouched attempt remains: %v", err)
			}
			if matches, err := matchingRegisteredWorktrees(git.New(repo), spec.Path); err != nil || len(matches) != 0 {
				t.Fatalf("untouched owned registration remains: %+v, %v", matches, err)
			}
			if got := runGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
				t.Fatalf("detached rollback changed refs: %q, want %q", got, refs)
			}
		})
	}

	t.Run("advanced clean HEAD", func(t *testing.T) {
		repo, base := initTestRepo(t)
		root := t.TempDir()
		spec := managedSpec(repo, root, filepath.Join(root, "detached"), "", base)
		spec.Mode = ModeDetached
		report, err := Ensure(spec)
		if err != nil {
			t.Fatal(err)
		}
		spec.BaseSHA, spec.AttemptID = report.Provenance.BaseSHA, report.Provenance.AttemptID
		manifest, err := provenanceFilePath(spec.Path)
		if err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(manifest)
		if err != nil {
			t.Fatal(err)
		}
		runGit(t, spec.Path, "commit", "--allow-empty", "-m", "detached output")
		head := runGit(t, spec.Path, "rev-parse", "HEAD")
		assertRetained := func() {
			t.Helper()
			if got := runGit(t, spec.Path, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refusal changed HEAD to %s, want %s", got, head)
			}
			if got, err := os.ReadFile(manifest); err != nil || string(got) != string(before) {
				t.Fatalf("refusal rewrote provenance: %v", err)
			}
			if matches, err := matchingRegisteredWorktrees(git.New(repo), spec.Path); err != nil || len(matches) != 1 {
				t.Fatalf("refusal changed registration: %+v, %v", matches, err)
			}
		}
		if err := RollbackAttempt(spec, report); err == nil {
			t.Fatalf("advanced rollback = %v, want retained committed work", err)
		}
		pending, err := Cleanup(spec)
		if err == nil || pending.Error == nil || pending.Error.Code != CleanupErrorUnreachable {
			t.Fatalf("unreachable cleanup = %+v, %v", pending, err)
		}
		assertRetained()

		// A surviving ref proves reachability, but still does not authorize an
		// attempt rollback or cleanup before the requested base contains HEAD.
		runGit(t, repo, "tag", "published-output", head)
		if err := RollbackAttempt(spec, report); err == nil {
			t.Fatal("rollback removed advanced HEAD after output publication")
		}
		pending, err = Cleanup(spec)
		if err == nil || pending.Error == nil || pending.Error.Code != CleanupErrorUnmerged {
			t.Fatalf("unmerged cleanup = %+v, %v", pending, err)
		}
		assertRetained()
		runGit(t, repo, "merge", "--ff-only", head)
		removed, err := Cleanup(spec)
		if err != nil || !removed.Removed || removed.Mode != ModeDetached || removed.Branch != "" {
			t.Fatalf("merged detached cleanup = %+v, %v", removed, err)
		}
		if _, err := os.Stat(spec.Path); !os.IsNotExist(err) {
			t.Fatalf("merged checkout remains: %v", err)
		}
	})
}

func TestDetachedRollbackRetainsUnprovenOwnership(t *testing.T) {
	for _, kind := range []string{"rival published", "unreadable", "wrong attempt", "wrong owner", "changed base"} {
		t.Run(kind, func(t *testing.T) {
			repo, base := initTestRepo(t)
			root := t.TempDir()
			spec := managedSpec(repo, root, filepath.Join(root, "detached"), "", base)
			spec.Mode = ModeDetached
			report, err := Ensure(spec)
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := provenanceFilePath(spec.Path)
			if err != nil {
				t.Fatal(err)
			}
			expected := *report.Provenance
			switch kind {
			case "unreadable":
				if err := os.WriteFile(manifest, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "wrong attempt":
				expected.AttemptID = "stale-attempt"
			case "wrong owner":
				spec.Owner = "rival"
			case "changed base":
				got := *report.Provenance
				got.BaseSHA = strings.Repeat("f", 40)
				if err := writeProvenance(spec.Path, got); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "rival published" {
				err = rollbackAfterFailedCreate(git.New(repo), spec, false, report.Provenance.BaseSHA)
			} else {
				err = rollbackCreated(git.New(repo), spec, false, report.Provenance.BaseSHA, &expected)
			}
			if err == nil {
				t.Fatal("rollback removed a checkout without exact ownership proof")
			}
			if got := runGit(t, spec.Path, "rev-parse", "HEAD"); got != report.Head {
				t.Fatalf("refusal moved HEAD: %s", got)
			}
			if got, err := os.ReadFile(manifest); err != nil || string(got) != string(before) {
				t.Fatalf("refusal changed ownership evidence: %v", err)
			}
		})
	}
	t.Run("unregistered path", func(t *testing.T) {
		repo, _ := initTestRepo(t)
		path := t.TempDir()
		marker := filepath.Join(path, "preserve.txt")
		if err := os.WriteFile(marker, []byte("unowned work"), 0o644); err != nil {
			t.Fatal(err)
		}
		base := runGit(t, repo, "rev-parse", "HEAD")
		if err := rollbackAfterFailedCreate(git.New(repo), Spec{RepoDir: repo, Path: path, Mode: ModeDetached}, false, base); err == nil {
			t.Fatal("rollback claimed to remove an unregistered existing path")
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != "unowned work" {
			t.Fatalf("rollback changed unowned work: %v", err)
		}
	})
}

// provenanceEqual compares two provenance records by value. CreatedAt is a
// pointer so that planned provenance can omit it, which means struct equality
// would compare addresses instead of the recorded timestamps.
func provenanceEqual(a, b Provenance) bool {
	switch {
	case (a.CreatedAt == nil) != (b.CreatedAt == nil):
		return false
	case a.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt):
		return false
	}
	a.CreatedAt, b.CreatedAt = nil, nil
	return a == b
}
