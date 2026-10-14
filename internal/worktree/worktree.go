// Package worktree is the transactional owner for agent workspace
// worktrees, exposed through the gc worktree CLI so provisioning paths can
// route through Ensure/Verify instead of running ad hoc git commands. When
// a path goes through this owner, the postconditions are uniform:
//
//   - the path is the root of a git worktree of the requested repository,
//   - HEAD is attached to the requested branch by default, or detached when
//     explicitly requested,
//   - an explicit base keeps its verbatim local meaning (no remote
//     fallback), and
//   - a failed creation rolls back only state that has not acquired work.
//
// Dry-run is observationally pure: it plans and validates without touching
// the filesystem or the repository.
package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// ErrWorktreeMissing reports that the spec path does not exist at all. It is
// the only verification failure Ensure treats as creatable; every other
// failure means the path holds state Ensure must not clobber.
var ErrWorktreeMissing = errors.New("worktree path does not exist")

const (
	provenanceSchemaVersion     = 1
	modeProvenanceSchemaVersion = 2
	provenanceFilename          = "gascity-worktree-provenance.json"

	// ModeAttached preserves the default branch-backed workspace contract.
	ModeAttached = "attached"
	// ModeDetached provisions a branchless workspace at the resolved input.
	ModeDetached = "detached"
	// PlacementOutsideWorktrees retains the existing placement policy.
	PlacementOutsideWorktrees = "outside-worktrees"
	// PlacementWithinRepoRoot permits the selected primary/control root as an ancestor.
	PlacementWithinRepoRoot = "within-repo-root"

	// LifecycleActive is the initial lifecycle state of a usable workspace.
	LifecycleActive = "active"

	// CleanupErrorInvalidSpec reports incomplete or invalid ownership input.
	CleanupErrorInvalidSpec = "invalid_spec"
	// CleanupErrorAmbiguous reports a path whose registered identity cannot be proven.
	CleanupErrorAmbiguous = "ambiguous_path"
	// CleanupErrorLockTimeout reports an expired lock acquisition deadline.
	CleanupErrorLockTimeout = "lock_timeout"
	// CleanupErrorCanceled reports caller cancellation before lock admission.
	CleanupErrorCanceled = "canceled"
	// CleanupErrorOwnership reports durable provenance that does not match the caller.
	CleanupErrorOwnership = "ownership_mismatch"
	// CleanupErrorStaleAttempt reports a worktree owned by the caller but
	// created by a different provisioning attempt than the one the request
	// names. It is distinct from CleanupErrorOwnership because the two demand
	// opposite responses: a stale attempt is the caller's own out-of-date
	// handle, recoverable by re-reading the current attempt id with verify,
	// while an ownership mismatch means the workspace belongs to somebody else
	// and retrying is exactly the wrong move.
	CleanupErrorStaleAttempt = "stale_attempt"
	// CleanupErrorDirty reports staged, unstaged, or untracked work.
	CleanupErrorDirty = "dirty_worktree"
	// CleanupErrorUnreachable reports commits that removing the worktree would
	// orphan: commits reachable from no branch, tag, or remote-tracking ref.
	//
	// The test is reachability rather than push state on purpose. `git worktree
	// remove` deletes the checkout, not refs/heads, so commits some local ref
	// still reaches survive removal. Gating on push state instead makes cleanup
	// a permanent no-op for exactly the worktrees it exists to collect: once a
	// branch is squash-merged and deleted from the remote, no remote-tracking
	// ref reaches that HEAD ever again, so the gate latches on forever and the
	// leak becomes monotonic (#4816).
	CleanupErrorUnreachable = "unreachable_commits"
	// CleanupErrorUnmerged reports a HEAD not contained in the requested base.
	CleanupErrorUnmerged = "unmerged_commits"
	// CleanupErrorRemove reports a safe git worktree remove failure.
	CleanupErrorRemove = "remove_failed"
	// CleanupErrorPrune reports incomplete post-removal registration pruning.
	CleanupErrorPrune = "prune_failed"
)

// Spec describes the desired workspace worktree.
type Spec struct {
	// RepoDir is the repository (main checkout or any of its worktrees)
	// the workspace must belong to. Absolute.
	RepoDir string
	// Path is where the worktree must exist. Absolute.
	Path string
	// Root is the configured per-rig worktree root. Managed worktrees must be
	// direct children of this root; callers must not derive it from their cwd.
	Root string
	// Mode defaults to attached when omitted. Explicit modes use provenance
	// schema 2; legacy callers continue to create schema 1 records.
	Mode string
	// Placement defaults to outside-worktrees. Explicit within-repo-root
	// permits only the selected primary/control repository as an ancestor.
	Placement string
	// Branch must be checked out in attached mode and empty in detached mode.
	Branch string
	// Base is the ref a new branch or detached worktree is created from. It is resolved
	// verbatim against the local repository — "main" means local main,
	// never a remote fallback. Required only when Branch does not exist.
	Base string
	// BaseSHA is the exact base commit recorded by a prior successful Ensure.
	// On first creation it may be empty; Ensure resolves Base and returns the
	// resulting SHA for atomic publication by the caller.
	BaseSHA string
	// BeadID and StoreRef bind the worktree to its work item.
	BeadID   string
	StoreRef string
	// Creator identifies the mechanism that created the workspace; Owner is
	// the single provisioner selected for its lifecycle.
	Creator string
	Owner   string
	// Generation fences stale metadata from an earlier provisioning attempt.
	Generation string
	// Lifecycle is the durable lifecycle state, initially "active".
	Lifecycle string
	// AttemptID names the exact provisioning attempt a Cleanup is authorized to
	// remove, as returned by the Ensure that created it. Cleanup requires it and
	// removes nothing unless the worktree's durable provenance carries the same
	// id.
	//
	// Without it, ownership is only bound to bead, owner, and generation, which
	// a re-provisioned workspace at the same path reproduces exactly. A cleanup
	// holding one attempt's arguments would then verify successfully against a
	// LATER attempt's worktree and delete live work. Every attempt mints a fresh
	// random id, so requiring an exact match makes stale cleanups fail closed.
	//
	// Ensure ignores this field; it mints its own.
	AttemptID string
	// LockTimeout bounds lock acquisition, not the transaction that follows.
	// Zero uses DefaultLockTimeout; negative values are invalid.
	LockTimeout time.Duration
	// DryRun plans without mutating anything.
	DryRun bool
}

// Provenance is the durable, worktree-specific ownership record. It lives in
// the worktree's private git directory, never in the checked-out files.
type Provenance struct {
	SchemaVersion int    `json:"schema_version"`
	BeadID        string `json:"bead_id"`
	StoreRef      string `json:"store_ref"`
	RepoIdentity  string `json:"repo_identity"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Mode          string `json:"mode,omitempty"`
	Placement     string `json:"placement,omitempty"`
	BaseRef       string `json:"base_ref"`
	BaseSHA       string `json:"base_sha"`
	Creator       string `json:"creator"`
	Owner         string `json:"owner"`
	Generation    string `json:"generation"`
	// CreatedAt and AttemptID exist only once a worktree has actually been
	// created. They are pointers/omitempty so planned provenance omits them
	// instead of emitting a zero time and an empty id, which a consumer
	// publishing this evidence onto a bead would otherwise store as though
	// they were real.
	CreatedAt *time.Time `json:"created_at,omitempty"`
	Lifecycle string     `json:"lifecycle"`
	AttemptID string     `json:"attempt_id,omitempty"`
}

// Report describes the observed or planned workspace state.
type Report struct {
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	Mode      string `json:"mode,omitempty"`
	Placement string `json:"placement,omitempty"`
	// Head is the commit SHA at the worktree's HEAD. Empty when a dry-run
	// only planned creation.
	Head string `json:"head,omitempty"`
	// Created reports that Ensure created the worktree in this call.
	Created bool `json:"created"`
	// BranchCreated reports that Ensure created the branch in this call.
	BranchCreated bool `json:"branch_created"`
	// Planned lists the actions a dry-run would have executed.
	Planned []string `json:"planned,omitempty"`
	// Provenance is the verified durable identity, or the planned identity in
	// dry-run output (without CreatedAt or AttemptID).
	Provenance *Provenance `json:"provenance,omitempty"`
}

// CleanupError is the stable machine-readable reason a managed worktree could
// not be removed safely. Cleanup never offers a force mode: callers must
// resolve the reported state rather than bypassing the ownership gates.
type CleanupError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	ExitCode int    `json:"exit_code"`
}

// CleanupReport describes the result of owner-mediated worktree removal.
// CleanupPending is true whenever operator or caller action is still required.
type CleanupReport struct {
	Path           string        `json:"path"`
	Branch         string        `json:"branch"`
	Mode           string        `json:"mode,omitempty"`
	Placement      string        `json:"placement,omitempty"`
	Head           string        `json:"head,omitempty"`
	Removed        bool          `json:"removed"`
	AlreadyAbsent  bool          `json:"already_absent"`
	CleanupPending bool          `json:"cleanup_pending"`
	Provenance     *Provenance   `json:"provenance,omitempty"`
	Error          *CleanupError `json:"error,omitempty"`
}

func (s Spec) validate() error {
	if s.LockTimeout < 0 {
		return errors.New("worktree spec: lock timeout must not be negative")
	}
	if s.RepoDir == "" {
		return errors.New("worktree spec: repo dir must not be empty")
	}
	if !filepath.IsAbs(s.RepoDir) {
		return fmt.Errorf("worktree spec: repo dir %q must be absolute", s.RepoDir)
	}
	if s.Path == "" {
		return errors.New("worktree spec: path must not be empty")
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("worktree spec: path %q must be absolute", s.Path)
	}
	if s.Mode != "" && s.Mode != ModeAttached && s.Mode != ModeDetached {
		return fmt.Errorf("worktree spec: unknown mode %q", s.Mode)
	}
	if s.Placement != "" && (s.Mode == "" || (s.Placement != PlacementOutsideWorktrees && s.Placement != PlacementWithinRepoRoot)) {
		return fmt.Errorf("worktree spec: placement %q requires an explicit mode and must be %q or %q", s.Placement, PlacementOutsideWorktrees, PlacementWithinRepoRoot)
	}
	if s.effectiveMode() == ModeAttached && s.Branch == "" {
		return errors.New("worktree spec: branch must not be empty")
	}
	if s.Mode == ModeDetached && s.Branch != "" {
		return errors.New("worktree spec: branch must be empty in detached mode")
	}
	if s.managed() {
		required := []struct {
			name  string
			value string
		}{
			{"root", s.Root},
			{"base", s.Base},
			{"bead id", s.BeadID},
			{"store ref", s.StoreRef},
			{"creator", s.Creator},
			{"owner", s.Owner},
			{"generation", s.Generation},
			{"lifecycle", s.Lifecycle},
		}
		for _, field := range required {
			if strings.TrimSpace(field.value) == "" {
				return fmt.Errorf("worktree spec: %s must not be empty for a managed worktree", field.name)
			}
		}
		if !filepath.IsAbs(s.Root) {
			return fmt.Errorf("worktree spec: root %q must be absolute", s.Root)
		}
		if s.Placement == PlacementWithinRepoRoot {
			if err := validatePlacementSpelling(s.RepoDir, s.Root, s.Path); err != nil {
				return err
			}
		}
		if err := validateManagedPath(s); err != nil {
			return err
		}
		if err := rejectRegisteredWorktreeAncestor(s); err != nil {
			return err
		}
	}
	return nil
}

func (s Spec) managed() bool {
	return s.Root != "" || s.BeadID != "" || s.StoreRef != "" || s.Creator != "" ||
		s.Owner != "" || s.Generation != "" || s.Lifecycle != "" || s.BaseSHA != "" || s.Mode != "" || s.Placement != ""
}

func (s Spec) effectiveMode() string {
	if s.Mode == "" {
		return ModeAttached
	}
	return s.Mode
}

// Verify checks that the spec's path is the root of a worktree of the
// spec's repository with the requested HEAD mode and branch.
// Managed worktrees must also retain the recorded base commit in HEAD's ancestry.
// It never mutates anything. A missing path returns ErrWorktreeMissing;
// any other failure describes the postcondition that does not hold.
func Verify(spec Spec) (Report, error) {
	if err := spec.validate(); err != nil {
		return Report{}, err
	}
	rep, err := verifyGitState(spec)
	if err != nil {
		return rep, err
	}
	if !spec.managed() {
		return rep, nil
	}
	provenance, err := readProvenance(spec.Path)
	if err != nil {
		return rep, fmt.Errorf("verifying worktree %q provenance: %w", spec.Path, err)
	}
	if err := verifyProvenance(spec, provenance); err != nil {
		return rep, fmt.Errorf("verifying worktree %q provenance: %w", spec.Path, err)
	}
	if err := verifyBaseAncestry(spec.Path, provenance.BaseSHA, rep.Head); err != nil {
		return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, err)
	}
	rep.Provenance = &provenance
	if provenance.SchemaVersion == modeProvenanceSchemaVersion {
		rep.Mode, rep.Placement = provenance.Mode, provenance.Placement
	}
	return rep, nil
}

func verifyGitState(spec Spec) (Report, error) {
	rep := Report{Path: spec.Path, Branch: spec.Branch}
	if spec.Mode != "" {
		rep.Mode, rep.Placement = spec.Mode, spec.effectivePlacement()
	}

	if _, err := os.Stat(spec.Path); err != nil {
		if os.IsNotExist(err) {
			return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, ErrWorktreeMissing)
		}
		return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, err)
	}

	repoGit := git.New(spec.RepoDir)
	if !repoGit.IsRepo() {
		return rep, fmt.Errorf("verifying worktree %q: repo dir %q is not a git repository", spec.Path, spec.RepoDir)
	}
	wtGit := git.New(spec.Path)
	if !wtGit.IsRepo() {
		return rep, fmt.Errorf("verifying worktree %q: path exists but is not inside a git repository", spec.Path)
	}

	// Repository identity: every worktree of one repository shares its
	// common git dir.
	repoCommon, err := canonicalCommonDir(repoGit)
	if err != nil {
		return rep, fmt.Errorf("verifying worktree %q: repo %q: %w", spec.Path, spec.RepoDir, err)
	}
	wtCommon, err := canonicalCommonDir(wtGit)
	if err != nil {
		return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, err)
	}
	if repoCommon != wtCommon {
		return rep, fmt.Errorf("verifying worktree %q: belongs to a different repository (common dir %q, want %q)", spec.Path, wtCommon, repoCommon)
	}

	// The path must be the worktree root itself, not a directory inside one.
	top, err := wtGit.TopLevel()
	if err != nil {
		return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, err)
	}
	if !pathutil.SamePath(top, spec.Path) {
		return rep, fmt.Errorf("verifying worktree %q: path is inside worktree rooted at %q, not a worktree root", spec.Path, top)
	}

	// Only symbolic-ref's documented exit 1 proves a detached HEAD. Other
	// failures are inconclusive and cannot satisfy either mode.
	ref, err := wtGit.HeadSymbolicRef()
	if spec.Mode == ModeDetached {
		var exitErr *exec.ExitError
		if err == nil {
			return rep, fmt.Errorf("verifying worktree %q: HEAD is attached to %q, want detached", spec.Path, ref)
		}
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return rep, fmt.Errorf("verifying worktree %q: checking detached HEAD: %w", spec.Path, err)
		}
	} else if err != nil {
		return rep, fmt.Errorf("verifying worktree %q: HEAD is detached: %w", spec.Path, err)
	} else if want := "refs/heads/" + spec.Branch; ref != want {
		return rep, fmt.Errorf("verifying worktree %q: checked-out ref is %q, want %q", spec.Path, ref, want)
	}
	head, err := wtGit.RevParseVerifyCommit("HEAD")
	if err != nil {
		return rep, fmt.Errorf("verifying worktree %q: %w", spec.Path, err)
	}
	rep.Head = head
	return rep, nil
}

// Ensure makes the spec's worktree exist and verify, creating it when the
// path is absent. It is verify-first and transactional:
//
//   - an already-valid worktree returns unchanged (Created=false);
//   - a path that exists in any other state is an error — Ensure never
//     clobbers or "repairs" state it did not create;
//   - attached creation keeps the verbatim local base and branch semantics;
//     explicit detached creation uses the resolved full base commit;
//   - a managed existing branch must contain the resolved base before
//     planning or creation;
//   - every postcondition is re-verified after creation, and a failure
//     rolls back untouched state and retains work acquired during creation;
//   - with Spec.DryRun, Ensure plans and validates but mutates nothing.
func Ensure(spec Spec) (Report, error) {
	return EnsureContext(context.Background(), spec)
}

// EnsureContext is Ensure with cancelable lock acquisition. Once admitted,
// the transaction finishes synchronously under the lock even if ctx expires.
func EnsureContext(ctx context.Context, spec Spec) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, lockWaitError(spec.Path, err)
	}
	if err := spec.validate(); err != nil {
		return Report{}, err
	}
	// Serialize against a concurrent Ensure, Cleanup, or rollback on this path,
	// from the FIRST observation through the last write. Ensure decides whether
	// to create from state it observed -- does the path exist, does the branch
	// exist -- so a lock taken only around the write is too late: a rival Ensure
	// creates the workspace in between, this call's `git worktree add` fails
	// against a path it still believes is free, and the rollback for that
	// failure removes the winner's workspace.
	//
	// A dry run observes and plans but mutates nothing, so it neither takes the
	// lock nor creates the lock file. Its purity is the contract, and a plan
	// that blocks on another caller's write would be a worse answer than a
	// plan that reports the state it saw.
	if !spec.DryRun {
		lock, lockErr := lockPathContext(ctx, spec.RepoDir, spec.Path, spec.LockTimeout)
		if lockErr != nil {
			return Report{}, lockErr
		}
		defer lock.unlock()
	}

	rep, verifyErr := Verify(spec)
	if verifyErr == nil {
		return rep, nil
	}
	if !errors.Is(verifyErr, ErrWorktreeMissing) {
		return rep, verifyErr
	}

	repoGit, branchExists, resolvedBase, err := resolveCreationState(spec)
	if err != nil {
		return rep, err
	}
	if spec.DryRun {
		return planDryRun(spec, rep, branchExists, resolvedBase)
	}

	branchCreated := spec.Mode != ModeDetached && !branchExists
	if err := createWorktree(repoGit, spec, branchExists, resolvedBase); err != nil {
		return rep, rollbackResult(err, rollbackAfterFailedCreate(repoGit, spec, branchCreated, resolvedBase))
	}
	created, err := verifyCreatedWorktree(spec, branchExists, resolvedBase)
	if err != nil {
		return rep, rollbackResult(err, rollbackCreated(repoGit, spec, branchCreated, resolvedBase, nil))
	}
	if spec.managed() {
		created, err = publishAndVerifyProvenance(spec, resolvedBase)
		if err != nil {
			return rep, rollbackResult(err, rollbackCreated(repoGit, spec, branchCreated, resolvedBase, created.Provenance))
		}
	}
	created.Created = true
	created.BranchCreated = branchCreated
	return created, nil
}

// Cleanup removes a managed worktree only after proving that the exact
// canonical path is registered to the requested repository and that its
// durable provenance matches the caller's complete ownership spec. It then
// fails closed for dirty, unpushed, or unmerged work. Removal is never forced
// and never falls back to recursive filesystem deletion.
//
// A path that is absent and has no matching worktree registration is an
// idempotent success. A missing path with a stale registration is ambiguous:
// Cleanup cannot read and verify its worktree-private provenance, so it leaves
// the registration untouched and reports cleanup_pending.
func Cleanup(spec Spec) (CleanupReport, error) {
	return CleanupContext(context.Background(), spec)
}

// CleanupContext is Cleanup with cancelable lock acquisition. Once admitted,
// verification and removal finish synchronously under the lock.
func CleanupContext(ctx context.Context, spec Spec) (CleanupReport, error) {
	report := CleanupReport{Path: spec.Path, Branch: spec.Branch}
	if spec.Mode != "" {
		report.Mode, report.Placement = spec.Mode, spec.effectivePlacement()
	}
	if err := ctx.Err(); err != nil {
		return cleanupLockFailure(report, lockWaitError(spec.Path, err))
	}
	if !spec.managed() {
		return cleanupFailure(report, CleanupErrorInvalidSpec, "cleanup requires a complete managed worktree spec")
	}
	if err := spec.validate(); err != nil {
		return cleanupFailure(report, CleanupErrorInvalidSpec, err.Error())
	}
	if strings.TrimSpace(spec.AttemptID) == "" {
		return cleanupFailure(report, CleanupErrorInvalidSpec,
			"cleanup requires the attempt id returned by the Ensure that created this worktree")
	}

	// Hold the path lock across verification AND removal. Every check below is
	// worthless if the workspace can be replaced between the check and the
	// remove.
	lock, err := lockPathContext(ctx, spec.RepoDir, spec.Path, spec.LockTimeout)
	if err != nil {
		return cleanupLockFailure(report, err)
	}
	defer lock.unlock()

	repoGit := git.New(spec.RepoDir)
	registered, err := matchingRegisteredWorktrees(repoGit, spec.Path)
	if err != nil {
		return cleanupFailure(report, CleanupErrorAmbiguous, fmt.Sprintf("listing registered worktrees: %v", err))
	}
	_, statErr := os.Stat(spec.Path)
	if os.IsNotExist(statErr) {
		if len(registered) == 0 {
			report.AlreadyAbsent = true
			return report, nil
		}
		return cleanupFailure(report, CleanupErrorAmbiguous,
			fmt.Sprintf("path %q is missing but remains registered; provenance cannot be verified", spec.Path))
	}
	if statErr != nil {
		return cleanupFailure(report, CleanupErrorAmbiguous,
			fmt.Sprintf("inspecting path %q: %v", spec.Path, statErr))
	}
	if len(registered) != 1 {
		return cleanupFailure(report, CleanupErrorAmbiguous,
			fmt.Sprintf("path %q has %d canonical worktree registrations, want exactly 1", spec.Path, len(registered)))
	}

	verified, err := Verify(spec)
	if err != nil {
		return cleanupFailure(report, CleanupErrorOwnership, err.Error())
	}
	report.Head = verified.Head
	report.Provenance = verified.Provenance
	report.Mode, report.Placement = verified.Mode, verified.Placement

	// Bind removal to one exact provisioning attempt. Bead, owner, and
	// generation are all reproduced by a re-provisioned workspace at the same
	// path, so they identify the SLOT rather than the occupant; only the
	// attempt id distinguishes this worktree from its replacement.
	if verified.Provenance == nil || verified.Provenance.AttemptID != spec.AttemptID {
		return cleanupFailure(report, CleanupErrorStaleAttempt,
			fmt.Sprintf("worktree attempt %q does not match the requested attempt %q; refusing to remove a workspace this request did not create",
				provenanceAttempt(verified.Provenance), spec.AttemptID))
	}

	worktreeGit := git.New(spec.Path)
	status, err := worktreeGit.StatusPorcelain()
	if err != nil {
		return cleanupFailure(report, CleanupErrorDirty, fmt.Sprintf("checking worktree status: %v", err))
	}
	if strings.TrimSpace(status) != "" {
		return cleanupFailure(report, CleanupErrorDirty, "worktree contains staged, unstaged, or untracked changes")
	}
	hasUnreachable, err := worktreeGit.HasUnreachableCommitsResult()
	if err != nil {
		return cleanupFailure(report, CleanupErrorUnreachable, err.Error())
	}
	if hasUnreachable {
		return cleanupFailure(report, CleanupErrorUnreachable, "worktree HEAD contains commits reachable from no branch, tag, or remote-tracking ref")
	}
	baseSHA, err := repoGit.RevParseVerifyCommit(spec.Base)
	if err != nil {
		return cleanupFailure(report, CleanupErrorUnmerged, fmt.Sprintf("resolving merge target %q: %v", spec.Base, err))
	}
	merged, err := isAncestor(spec.Path, "HEAD", baseSHA)
	if err != nil {
		return cleanupFailure(report, CleanupErrorUnmerged, fmt.Sprintf("checking merge state against %q: %v", spec.Base, err))
	}
	if !merged {
		return cleanupFailure(report, CleanupErrorUnmerged,
			fmt.Sprintf("worktree HEAD is not merged into local base %q at %s", spec.Base, baseSHA))
	}

	if err := repoGit.WorktreeRemove(spec.Path, false); err != nil {
		return cleanupFailure(report, CleanupErrorRemove, err.Error())
	}
	report.Removed = true
	if err := repoGit.WorktreePrune(); err != nil {
		return cleanupFailure(report, CleanupErrorPrune, err.Error())
	}
	return report, nil
}

func cleanupFailure(report CleanupReport, code, message string) (CleanupReport, error) {
	report.CleanupPending = true
	report.Error = &CleanupError{Code: code, Message: message, ExitCode: 1}
	return report, errors.New(message)
}

func cleanupLockFailure(report CleanupReport, err error) (CleanupReport, error) {
	code := CleanupErrorAmbiguous
	switch {
	case errors.Is(err, ErrLockTimeout):
		code = CleanupErrorLockTimeout
	case errors.Is(err, context.Canceled):
		code = CleanupErrorCanceled
	}
	report, _ = cleanupFailure(report, code, err.Error())
	return report, err
}

func matchingRegisteredWorktrees(repoGit *git.Git, path string) ([]git.Worktree, error) {
	entries, err := repoGit.WorktreeList()
	if err != nil {
		return nil, err
	}
	candidate, err := canonicalPathAllowMissing(path)
	if err != nil {
		return nil, err
	}
	matches := make([]git.Worktree, 0, 1)
	for _, entry := range entries {
		registered, pathErr := canonicalPathAllowMissing(entry.Path)
		if pathErr != nil {
			// A registration we cannot canonicalize may be the very entry
			// we are looking for; reporting "no match" would let a caller
			// treat a still-registered path as absent.
			return nil, fmt.Errorf("canonicalizing registered worktree %q: %w", entry.Path, pathErr)
		}
		if pathutil.SamePath(candidate, registered) {
			matches = append(matches, entry)
		}
	}
	return matches, nil
}

func isAncestor(workDir, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = workDir
	cmd.Env = git.SanitizedEnv()
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %s: %w", strings.TrimSpace(string(output)), err)
}

// verifyBaseAncestry checks the recorded input commit against the observed HEAD.
func verifyBaseAncestry(workDir, baseSHA, head string) error {
	containsBase, err := isAncestor(workDir, baseSHA, head)
	if err != nil {
		return fmt.Errorf("checking recorded base %s against HEAD %s: %w", baseSHA, head, err)
	}
	if !containsBase {
		return fmt.Errorf("HEAD %s does not contain recorded base %s", head, baseSHA)
	}
	return nil
}

func resolveCreationState(spec Spec) (*git.Git, bool, string, error) {
	repoGit := git.New(spec.RepoDir)
	if !repoGit.IsRepo() {
		return nil, false, "", fmt.Errorf("ensuring worktree %q: repo dir %q is not a git repository", spec.Path, spec.RepoDir)
	}
	branchExists := false
	if spec.Mode != ModeDetached {
		var err error
		branchExists, err = repoGit.BranchExists(spec.Branch)
		if err != nil {
			return nil, false, "", fmt.Errorf("ensuring worktree %q: %w", spec.Path, err)
		}
	}
	if !branchExists && spec.Base == "" {
		return nil, false, "", fmt.Errorf("ensuring worktree %q: branch %q does not exist and no base was given", spec.Path, spec.Branch)
	}
	var resolvedBase string
	if spec.Base != "" {
		sha, err := repoGit.RevParseVerifyCommit(spec.Base)
		if err != nil {
			return nil, false, "", fmt.Errorf("ensuring worktree %q: %w", spec.Path, err)
		}
		resolvedBase = sha
	}
	if spec.BaseSHA != "" && resolvedBase != spec.BaseSHA {
		return nil, false, "", fmt.Errorf("ensuring worktree %q: base ref %q resolves to %s, want recorded base SHA %s",
			spec.Path, spec.Base, resolvedBase, spec.BaseSHA)
	}
	if spec.managed() && branchExists {
		head, err := repoGit.RevParseVerifyCommit("refs/heads/" + spec.Branch)
		if err != nil {
			return nil, false, "", fmt.Errorf("ensuring worktree %q: %w", spec.Path, err)
		}
		if err := verifyBaseAncestry(spec.RepoDir, resolvedBase, head); err != nil {
			return nil, false, "", fmt.Errorf("ensuring worktree %q: %w", spec.Path, err)
		}
	}
	return repoGit, branchExists, resolvedBase, nil
}

func planDryRun(spec Spec, rep Report, branchExists bool, resolvedBase string) (Report, error) {
	switch {
	case spec.Mode == ModeDetached:
		rep.Planned = []string{fmt.Sprintf("git worktree add --detach %s %s", spec.Path, resolvedBase)}
	case branchExists:
		rep.Planned = []string{fmt.Sprintf("git worktree add %s %s", spec.Path, spec.Branch)}
	default:
		rep.Planned = []string{fmt.Sprintf("git worktree add -b %s %s %s", spec.Branch, spec.Path, spec.Base)}
	}
	if spec.managed() {
		provenance, err := plannedProvenance(spec, resolvedBase)
		if err != nil {
			return rep, err
		}
		rep.Provenance = &provenance
	}
	return rep, nil
}

func createWorktree(repoGit *git.Git, spec Spec, branchExists bool, resolvedBase string) error {
	var err error
	switch {
	case spec.Mode == ModeDetached:
		err = repoGit.WorktreeAddDetached(spec.Path, resolvedBase)
	case branchExists:
		err = repoGit.WorktreeAddExistingBranch(spec.Path, spec.Branch)
	default:
		// Preserve the explicit base verbatim so git's own branch-setup
		// semantics follow the operator's stated intent.
		err = repoGit.WorktreeAddNewBranch(spec.Path, spec.Branch, spec.Base)
	}
	if err != nil {
		return fmt.Errorf("ensuring worktree %q: %w", spec.Path, err)
	}
	return nil
}

func verifyCreatedWorktree(spec Spec, branchExists bool, resolvedBase string) (Report, error) {
	created, err := verifyGitState(spec)
	if err != nil {
		return Report{}, fmt.Errorf("worktree %q failed post-create verification: %w", spec.Path, err)
	}
	if !branchExists && created.Head != resolvedBase {
		return Report{}, fmt.Errorf("worktree %q: created HEAD is at %s, want base %q = %s",
			spec.Path, created.Head, spec.Base, resolvedBase)
	}
	return created, nil
}

func publishAndVerifyProvenance(spec Spec, resolvedBase string) (Report, error) {
	provenance, err := plannedProvenance(spec, resolvedBase)
	if err != nil {
		return Report{}, fmt.Errorf("worktree %q provenance preparation failed: %w", spec.Path, err)
	}
	createdAt := time.Now().UTC()
	provenance.CreatedAt = &createdAt
	provenance.AttemptID, err = newAttemptID()
	if err != nil {
		return Report{}, fmt.Errorf("worktree %q provenance attempt id failed: %w", spec.Path, err)
	}
	// Preserve this attempt's identity for a failed publication rollback.
	// A different or unreadable record must never be treated as ours.
	partial := Report{Provenance: &provenance}
	if err := writeProvenance(spec.Path, provenance); err != nil {
		return partial, fmt.Errorf("worktree %q provenance publish failed: %w", spec.Path, err)
	}
	verifySpec := spec
	verifySpec.BaseSHA = provenance.BaseSHA
	verified, err := Verify(verifySpec)
	if err != nil {
		return partial, fmt.Errorf("worktree %q failed provenance verification: %w", spec.Path, err)
	}
	return verified, nil
}

// RollbackAttempt removes only a workspace created by the supplied Ensure
// report. It fences against stale/cross-owner reports using the durable
// attempt ID and refuses to remove a worktree that acquired WIP.
func RollbackAttempt(spec Spec, report Report) error {
	return RollbackAttemptContext(context.Background(), spec, report)
}

// RollbackAttemptContext is RollbackAttempt with cancelable lock acquisition.
// Once admitted, rollback finishes synchronously under the lock.
func RollbackAttemptContext(ctx context.Context, spec Spec, report Report) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("rollback refused: %w", lockWaitError(spec.Path, err))
	}
	// Validate before touching anything. Taking the lock first would resolve
	// the lock file through an unvalidated RepoDir, and git resolves an empty
	// dir against the calling process's own working directory: an incomplete
	// spec then writes into whatever repository the caller happens to be
	// standing in, and reports only the validation error.
	if err := spec.validate(); err != nil {
		return fmt.Errorf("rollback refused: %w", err)
	}
	if !report.Created {
		return errors.New("rollback refused: report does not describe an attempt-created worktree")
	}
	if report.Provenance == nil || report.Provenance.AttemptID == "" {
		return errors.New("rollback refused: report has no provenance attempt id")
	}

	// Rollback verifies and then removes, so it needs the same lock Ensure and
	// Cleanup hold: without it the attempt this call verified can be cleaned up
	// and replaced before the removal lands, and rollback deletes the successor.
	lock, lockErr := lockPathContext(ctx, spec.RepoDir, spec.Path, spec.LockTimeout)
	if lockErr != nil {
		return fmt.Errorf("rollback refused: %w", lockErr)
	}
	defer lock.unlock()

	verified, err := Verify(spec)
	if err != nil {
		return fmt.Errorf("rollback refused: %w", err)
	}
	if verified.Provenance == nil || verified.Provenance.AttemptID != report.Provenance.AttemptID {
		return fmt.Errorf("rollback refused: provenance attempt %q does not match report attempt %q",
			provenanceAttempt(verified.Provenance), report.Provenance.AttemptID)
	}
	if spec.Mode == ModeDetached {
		if err := verifyDetachedRollback(spec, verified.Provenance.BaseSHA, report.Provenance); err != nil {
			return fmt.Errorf("rollback refused: %w", err)
		}
	}
	status, err := git.New(spec.Path).StatusPorcelain()
	if err != nil {
		return fmt.Errorf("rollback refused: checking worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("rollback refused: worktree contains changes")
	}
	repoGit := git.New(spec.RepoDir)
	if err := repoGit.WorktreeRemove(spec.Path, false); err != nil {
		return fmt.Errorf("rolling back worktree: %w", err)
	}
	if err := repoGit.WorktreePrune(); err != nil {
		return fmt.Errorf("pruning rolled-back worktree: %w", err)
	}
	if spec.Mode != ModeDetached && report.BranchCreated {
		if err := repoGit.BranchDeleteIfMerged(spec.Branch); err != nil {
			return fmt.Errorf("deleting attempt-created branch %q: %w", spec.Branch, err)
		}
	}
	return nil
}

// rollbackAfterFailedCreate undoes whatever a failed creation actually left
// behind.
//
// `git worktree add` is not atomic. It registers the worktree and creates the
// branch before running post-checkout work, so a hook, an LFS smudge, or a
// disk error can fail the command after those artifacts exist. Returning the
// error without reconciling leaks a registered worktree and a branch, and the
// caller cannot tell from the error that anything was created.
//
// Rollback therefore inspects current state rather than assuming the add
// either fully succeeded or did nothing: it removes the registration only if
// one is present, and deletes the branch only if this call was the one that
// would have created it and it now exists. Removal is never forced, so a
// partial checkout that left real content is retained and reported rather than
// deleted.
// rollbackAfterFailedCreate undoes a failed create. Ensure holds the path
// lock across the whole observe-then-create window, so a rival Ensure cannot
// interleave; a git process outside gc still can, and this refuses to delete
// anything it cannot show is its own. The branch is removed only when it is
// still at the base this attempt would have created it from, and only when
// git agrees it is merged.
func rollbackAfterFailedCreate(repoGit *git.Git, spec Spec, branchCreated bool, wantBaseSHA string) error {
	path, branch := spec.Path, spec.Branch
	var errs []error
	matches, err := matchingRegisteredWorktrees(repoGit, path)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("listing worktrees for rollback: %w", err))
	case spec.Mode == ModeDetached && len(matches) == 0:
		if _, statErr := os.Lstat(path); statErr == nil {
			return fmt.Errorf("refusing detached rollback: path %q is unregistered and ownership is inconclusive", path)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("refusing detached rollback: path %q is unregistered and ownership is inconclusive: %w", path, statErr)
		}
	case len(matches) > 0:
		if spec.Mode == ModeDetached {
			if err := verifyDetachedRollback(spec, wantBaseSHA, nil); err != nil {
				return err
			}
		}
		// Published provenance means a completed Ensure owns this workspace:
		// this attempt never reaches the publish step, so the registration
		// belongs to a rival creator and must survive.
		// Only a missing provenance file establishes that no completed Ensure
		// owns this workspace. A malformed or unreadable one leaves ownership
		// inconclusive, which is not a license to remove it.
		if _, provErr := readProvenance(path); !errors.Is(provErr, fs.ErrNotExist) {
			if provErr == nil {
				errs = append(errs, fmt.Errorf("refusing to remove worktree %q: it carries published provenance this attempt did not write", path))
			} else {
				errs = append(errs, fmt.Errorf("refusing to remove worktree %q: ownership is inconclusive: %w", path, provErr))
			}
			break
		}
		if err := repoGit.WorktreeRemove(path, false); err != nil {
			if spec.Mode == ModeDetached {
				return err
			}
			errs = append(errs, err)
		}
		if err := repoGit.WorktreePrune(); err != nil {
			errs = append(errs, err)
		}
	}
	if spec.Mode != ModeDetached && branchCreated {
		exists, err := repoGit.BranchExists(branch)
		switch {
		case err != nil:
			// Refuse to delete on an inconclusive probe: the branch may
			// predate this attempt.
			errs = append(errs, fmt.Errorf("rollback branch probe: %w", err))
		case exists:
			head, headErr := repoGit.RevParseVerifyCommit(branch)
			switch {
			case headErr != nil:
				errs = append(errs, fmt.Errorf("rollback branch head: %w", headErr))
			case wantBaseSHA != "" && head != wantBaseSHA:
				errs = append(errs, fmt.Errorf("refusing to delete branch %q: head %s is not the base %s this attempt would have created it from", branch, head, wantBaseSHA))
			default:
				if err := repoGit.BranchDeleteIfMerged(branch); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// rollbackCreated undoes only artifacts created by the current Ensure call.
// Removal is never forced: if the new worktree acquired WIP, rollback retains
// it and reports the cleanup failure.
func rollbackCreated(repoGit *git.Git, spec Spec, branchCreated bool, wantBaseSHA string, expected *Provenance) error {
	path, branch := spec.Path, spec.Branch
	if spec.Mode == ModeDetached {
		if err := verifyDetachedRollback(spec, wantBaseSHA, expected); err != nil {
			return err
		}
	}
	var errs []error
	if err := repoGit.WorktreeRemove(path, false); err != nil {
		if spec.Mode == ModeDetached {
			return err
		}
		errs = append(errs, err)
	}
	if err := repoGit.WorktreePrune(); err != nil {
		errs = append(errs, err)
	}
	if spec.Mode != ModeDetached && branchCreated {
		if err := repoGit.BranchDeleteIfMerged(branch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// A clean detached checkout can still hold commits no surviving branch
// protects. Before rollback, prove this exact registration has not advanced
// beyond the attempt's initial input and carries this attempt's published ownership.
func verifyDetachedRollback(spec Spec, wantBaseSHA string, expected *Provenance) error {
	if expected == nil || expected.AttemptID == "" {
		return errors.New("refusing detached rollback: expected provisioning attempt is missing")
	}
	matches, err := matchingRegisteredWorktrees(git.New(spec.RepoDir), spec.Path)
	if err != nil {
		return fmt.Errorf("refusing detached rollback: listing registrations for %q: %w", spec.Path, err)
	}
	if len(matches) != 1 {
		return fmt.Errorf("refusing detached rollback: expected one registration for %q, got %d", spec.Path, len(matches))
	}
	verified, err := verifyGitState(spec)
	if err != nil {
		return fmt.Errorf("refusing detached rollback: %w", err)
	}
	if wantBaseSHA == "" || verified.Head != wantBaseSHA || matches[0].Head != verified.Head || matches[0].Branch != "" {
		return fmt.Errorf("refusing detached rollback: HEAD %s or registration differs from initial base %s", verified.Head, wantBaseSHA)
	}
	got, err := readProvenance(spec.Path)
	if err != nil {
		return fmt.Errorf("refusing detached rollback: ownership is inconclusive: %w", err)
	}
	if got.AttemptID != expected.AttemptID {
		return errors.New("refusing detached rollback: published provenance belongs to another attempt")
	}
	if got.BaseSHA != wantBaseSHA {
		return errors.New("refusing detached rollback: published base differs from the attempt's initial input")
	}
	if err := verifyProvenance(spec, got); err != nil {
		return fmt.Errorf("refusing detached rollback: %w", err)
	}
	return nil
}

func rollbackResult(cause, rollbackErr error) error {
	if rollbackErr != nil {
		return fmt.Errorf("%w; rollback incomplete: %w", cause, rollbackErr)
	}
	return fmt.Errorf("%w (rolled back)", cause)
}

// canonicalCommonDir returns the repository common dir with symlinks
// resolved, so identity comparison is not defeated by symlinked paths.
func canonicalCommonDir(g *git.Git) (string, error) {
	common, err := g.CommonDir()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(common)
	if err != nil {
		return "", fmt.Errorf("resolving common dir %q: %w", common, err)
	}
	return resolved, nil
}

func validateManagedPath(spec Spec) error {
	root, err := canonicalPathAllowMissing(spec.Root)
	if err != nil {
		return fmt.Errorf("worktree spec: canonicalizing root %q: %w", spec.Root, err)
	}
	path, err := canonicalPathAllowMissing(spec.Path)
	if err != nil {
		return fmt.Errorf("worktree spec: canonicalizing path %q: %w", spec.Path, err)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("worktree spec: comparing path %q to root %q: %w", path, root, err)
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		strings.Contains(rel, string(filepath.Separator)) {
		return fmt.Errorf("worktree spec: path %q must be a direct child of configured root %q", path, root)
	}
	return nil
}

func rejectRegisteredWorktreeAncestor(spec Spec) error {
	worktrees, err := git.New(spec.RepoDir).WorktreeList()
	if err != nil {
		return fmt.Errorf("worktree spec: listing registered worktrees: %w", err)
	}
	candidate, err := canonicalPathAllowMissing(spec.Path)
	if err != nil {
		return fmt.Errorf("worktree spec: canonicalizing candidate path: %w", err)
	}
	if spec.Placement == PlacementWithinRepoRoot {
		return validateWithinRepoPlacement(spec, worktrees, candidate)
	}
	for _, existing := range worktrees {
		existingPath, pathErr := canonicalPathAllowMissing(existing.Path)
		if pathErr != nil {
			return fmt.Errorf("worktree spec: canonicalizing registered worktree %q: %w", existing.Path, pathErr)
		}
		if pathutil.SamePath(existingPath, candidate) {
			continue
		}
		rel, relErr := filepath.Rel(existingPath, candidate)
		if relErr == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("worktree spec: path %q is inside registered worktree %q", candidate, existingPath)
		}
	}
	return nil
}

func canonicalPathAllowMissing(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func plannedProvenance(spec Spec, resolvedBase string) (Provenance, error) {
	repoIdentity, err := canonicalCommonDir(git.New(spec.RepoDir))
	if err != nil {
		return Provenance{}, fmt.Errorf("resolving repository identity: %w", err)
	}
	path, err := canonicalPathAllowMissing(spec.Path)
	if err != nil {
		return Provenance{}, fmt.Errorf("canonicalizing worktree path: %w", err)
	}
	if spec.BaseSHA != "" {
		resolvedBase = spec.BaseSHA
	}
	provenance := Provenance{
		SchemaVersion: provenanceSchemaVersion,
		BeadID:        strings.TrimSpace(spec.BeadID),
		StoreRef:      strings.TrimSpace(spec.StoreRef),
		RepoIdentity:  repoIdentity,
		Path:          path,
		Branch:        strings.TrimSpace(spec.Branch),
		BaseRef:       strings.TrimSpace(spec.Base),
		BaseSHA:       strings.TrimSpace(resolvedBase),
		Creator:       strings.TrimSpace(spec.Creator),
		Owner:         strings.TrimSpace(spec.Owner),
		Generation:    strings.TrimSpace(spec.Generation),
		Lifecycle:     strings.TrimSpace(spec.Lifecycle),
	}
	if spec.Mode != "" {
		provenance.SchemaVersion = modeProvenanceSchemaVersion
		provenance.Mode, provenance.Placement = spec.Mode, spec.effectivePlacement()
	}
	return provenance, nil
}

func verifyProvenance(spec Spec, got Provenance) error {
	want, err := plannedProvenance(spec, got.BaseSHA)
	if err != nil {
		return err
	}
	mode := got.Mode
	switch got.SchemaVersion {
	case provenanceSchemaVersion:
		if got.Mode != "" || got.Placement != "" {
			return errors.New("schema 1 provenance must not declare mode or placement")
		}
		mode = ModeAttached
	case modeProvenanceSchemaVersion:
		if (got.Mode != ModeAttached && got.Mode != ModeDetached) || (got.Placement != PlacementOutsideWorktrees && got.Placement != PlacementWithinRepoRoot) {
			return fmt.Errorf("schema 2 provenance has invalid mode %q or placement %q", got.Mode, got.Placement)
		}
	default:
		return fmt.Errorf("unsupported provenance schema version %d", got.SchemaVersion)
	}
	placement := got.Placement
	if got.SchemaVersion == provenanceSchemaVersion {
		placement = PlacementOutsideWorktrees
	}
	if placement != spec.effectivePlacement() {
		return fmt.Errorf("placement is %q, want %q", placement, spec.effectivePlacement())
	}
	if mode != spec.effectiveMode() {
		return fmt.Errorf("mode is %q, want %q", mode, spec.effectiveMode())
	}
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"bead id", got.BeadID, want.BeadID},
		{"store ref", got.StoreRef, want.StoreRef},
		{"repository identity", got.RepoIdentity, want.RepoIdentity},
		{"path", got.Path, want.Path},
		{"branch", got.Branch, want.Branch},
		{"base ref", got.BaseRef, want.BaseRef},
		{"creator", got.Creator, want.Creator},
		{"owner", got.Owner, want.Owner},
		{"generation", got.Generation, want.Generation},
		{"lifecycle", got.Lifecycle, want.Lifecycle},
	}
	for _, check := range checks {
		if check.got != check.want {
			return fmt.Errorf("%s is %q, want %q", check.name, check.got, check.want)
		}
	}
	if spec.BaseSHA != "" && got.BaseSHA != spec.BaseSHA {
		return fmt.Errorf("base SHA is %q, want %q", got.BaseSHA, spec.BaseSHA)
	}
	if got.BaseSHA == "" {
		return errors.New("base SHA is empty")
	}
	if got.CreatedAt == nil || got.CreatedAt.IsZero() {
		return errors.New("creation time is empty")
	}
	if got.AttemptID == "" {
		return errors.New("attempt id is empty")
	}
	return nil
}

func provenanceFilePath(worktreePath string) (string, error) {
	gitDir, err := git.New(worktreePath).GitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, provenanceFilename), nil
}

func readProvenance(worktreePath string) (Provenance, error) {
	path, err := provenanceFilePath(worktreePath)
	if err != nil {
		return Provenance{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Provenance{}, fmt.Errorf("reading %q: %w", path, err)
	}
	var provenance Provenance
	if err := json.Unmarshal(data, &provenance); err != nil {
		return Provenance{}, fmt.Errorf("decoding %q: %w", path, err)
	}
	if provenance.SchemaVersion == provenanceSchemaVersion {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return Provenance{}, fmt.Errorf("decoding %q: %w", path, err)
		}
		for key := range fields {
			if strings.EqualFold(key, "mode") || strings.EqualFold(key, "placement") {
				return Provenance{}, fmt.Errorf("decoding %q: schema 1 must not declare %s", path, key)
			}
		}
	}
	return provenance, nil
}

func writeProvenance(worktreePath string, provenance Provenance) error {
	path, err := provenanceFilePath(worktreePath)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), "."+provenanceFilename+".*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func newAttemptID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func provenanceAttempt(provenance *Provenance) string {
	if provenance == nil {
		return ""
	}
	return provenance.AttemptID
}

// validateWithinRepoPlacement permits only the selected primary/control
// registration as an ancestor; a linked checkout never grants that exception.
func validateWithinRepoPlacement(spec Spec, worktrees []git.Worktree, candidate string) error {
	g := git.New(spec.RepoDir)
	common, err := canonicalCommonDir(g)
	if err != nil {
		return fmt.Errorf("worktree spec: repository identity: %w", err)
	}
	private, err := g.GitDir()
	if err != nil {
		return fmt.Errorf("worktree spec: private Git directory: %w", err)
	}
	private, err = filepath.EvalSymlinks(private)
	if err != nil {
		return fmt.Errorf("worktree spec: canonical private Git directory: %w", err)
	}
	if !pathutil.SamePath(private, common) {
		return errors.New("worktree spec: within-repo-root requires a primary or bare control repository")
	}
	bare, err := g.IsBareRepository()
	if err != nil {
		return fmt.Errorf("worktree spec: repository topology: %w", err)
	}
	control := common
	if bare {
		controls := 0
		for _, entry := range worktrees {
			if !entry.Bare || entry.Prunable {
				continue
			}
			path, err := canonicalPathAllowMissing(entry.Path)
			if err != nil {
				return fmt.Errorf("worktree spec: bare control registration: %w", err)
			}
			if !pathutil.SamePath(path, common) && (filepath.Base(common) != ".git" || !pathutil.SamePath(path, filepath.Dir(common))) {
				continue
			}
			identity, err := canonicalCommonDir(git.New(path))
			if err != nil {
				return fmt.Errorf("worktree spec: bare control identity: %w", err)
			}
			if !pathutil.SamePath(identity, common) {
				return errors.New("worktree spec: bare control repository identity differs")
			}
			control = path
			controls++
		}
		if controls != 1 {
			return errors.New("worktree spec: bare control registration is ambiguous")
		}
	}
	if !bare {
		control, err = g.TopLevel()
		if err != nil {
			return fmt.Errorf("worktree spec: primary checkout: %w", err)
		}
		control, err = filepath.EvalSymlinks(control)
		if err != nil {
			return fmt.Errorf("worktree spec: canonical primary checkout: %w", err)
		}
	}
	if pathutil.SamePath(candidate, control) {
		return errors.New("worktree spec: workspace cannot replace the repository control root")
	}
	controlCount, candidateCount := 0, 0
	for _, existing := range worktrees {
		path, err := canonicalPathAllowMissing(existing.Path)
		if err != nil {
			return fmt.Errorf("worktree spec: registered identity %q: %w", existing.Path, err)
		}
		if pathutil.SamePath(path, control) {
			controlCount++
			if existing.Bare != bare || existing.Prunable {
				return errors.New("worktree spec: selected control registration has inconsistent topology or is prunable")
			}
		}
		if pathutil.SamePath(path, candidate) {
			candidateCount++
			if existing.Bare || existing.Prunable {
				return errors.New("worktree spec: candidate registration is bare or prunable")
			}
		} else if pathutil.PathWithin(path, candidate) && !pathutil.SamePath(path, control) {
			return fmt.Errorf("worktree spec: path %q is inside another registered worktree %q", candidate, path)
		}
	}
	if controlCount != 1 || candidateCount > 1 {
		return errors.New("worktree spec: repository or workspace registration is ambiguous")
	}
	if (!bare || !pathutil.SamePath(control, common)) && pathutil.PathWithin(common, candidate) {
		return errors.New("worktree spec: workspace is inside Git administration")
	}
	if bare {
		for _, name := range []string{"objects", "refs", "logs", "hooks", "info", "branches", "worktrees", "modules", "rr-cache", "rebase-apply", "rebase-merge", "HEAD", "index", "config", "packed-refs", "shallow"} {
			admin, err := g.GitPath(name)
			if err != nil {
				return fmt.Errorf("worktree spec: resolving Git administration %q: %w", name, err)
			}
			admin, err = canonicalPathAllowMissing(admin)
			if err != nil {
				return fmt.Errorf("worktree spec: canonical Git administration %q: %w", name, err)
			}
			if pathutil.PathWithin(admin, candidate) {
				return fmt.Errorf("worktree spec: workspace is inside Git administration %q", admin)
			}
		}
	}
	// Probe only existing ancestors that declare a repository marker. A
	// failed native identity read is a refusal, never an absent repository.
	for parent := filepath.Dir(candidate); ; parent = filepath.Dir(parent) {
		_, markerErr := os.Lstat(filepath.Join(parent, ".git"))
		marker := markerErr == nil
		if markerErr != nil && !errors.Is(markerErr, fs.ErrNotExist) {
			return fmt.Errorf("worktree spec: reading ancestor repository marker: %w", markerErr)
		}
		bareMarker := false
		if !marker {
			bareMarker = true
			for _, name := range []string{"HEAD", "objects", "refs"} {
				if _, err := os.Lstat(filepath.Join(parent, name)); errors.Is(err, fs.ErrNotExist) {
					bareMarker = false
					break
				} else if err != nil {
					return fmt.Errorf("worktree spec: reading bare ancestor marker: %w", err)
				}
			}
		}
		if marker || bareMarker {
			identity, err := canonicalCommonDir(git.New(parent))
			if err != nil {
				return fmt.Errorf("worktree spec: ancestor repository %q: %w", parent, err)
			}
			if !pathutil.SamePath(identity, common) {
				return fmt.Errorf("worktree spec: foreign repository ancestor %q", parent)
			}
			if !pathutil.SamePath(parent, control) && (!bare || !pathutil.SamePath(parent, filepath.Dir(common)) || filepath.Base(common) != ".git") {
				return fmt.Errorf("worktree spec: unselected repository ancestor %q", parent)
			}
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	return nil
}

func validatePlacementSpelling(paths ...string) error {
	for _, path := range paths {
		for _, component := range strings.Split(filepath.ToSlash(path), "/") {
			if component == ".." {
				return errors.New("worktree spec: within-repo-root path must not contain traversal")
			}
		}
		canonical, err := canonicalPathAllowMissing(path)
		if err != nil {
			return fmt.Errorf("worktree spec: placement path %q: %w", path, err)
		}
		if filepath.Clean(path) != canonical {
			return fmt.Errorf("worktree spec: within-repo-root path %q uses a symlink alias", path)
		}
	}
	return nil
}

func (s Spec) effectivePlacement() string {
	if s.Placement == "" {
		return PlacementOutsideWorktrees
	}
	return s.Placement
}
