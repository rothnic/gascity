package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/worktree"
	"github.com/spf13/cobra"
)

// worktreeCmdOpts carries the flag values for gc worktree subcommands.
type worktreeCmdOpts struct {
	Repo        string
	Root        string
	Path        string
	Branch      string
	Mode        string
	Placement   string
	Base        string
	BaseSHA     string
	BeadID      string
	StoreRef    string
	Creator     string
	Owner       string
	Generation  string
	Lifecycle   string
	AttemptID   string
	Agent       string
	DryRun      bool
	JSON        bool
	LockTimeout time.Duration
}

func worktreeLockFlagSet(cmd *cobra.Command, opts *worktreeCmdOpts) {
	cmd.Flags().DurationVar(&opts.LockTimeout, "lock-timeout", worktree.DefaultLockTimeout,
		"maximum wait for the workspace lock (must be positive; does not limit the transaction)")
	previousPreRun := cmd.PreRunE
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if previousPreRun != nil {
			if err := previousPreRun(cmd, args); err != nil {
				return err
			}
		}
		if opts.LockTimeout <= 0 {
			return errors.New("--lock-timeout must be positive")
		}
		return nil
	}
}

// newWorktreeCmd returns the gc worktree command group. It is the CLI face
// of internal/worktree — the transactional workspace owner (gc-r9fx) that
// sling and formula-managed workspace setup can route through instead of
// running competing ad hoc provisioning.
func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Ensure or verify agent workspace worktrees",
		Long: `Ensure or verify agent workspace worktrees.

gc worktree is the single transactional owner for workspace provisioning.
Postconditions: the path is a direct child of the configured per-rig root and
the root of a worktree of the given repository. HEAD is attached to the requested
branch by default; --mode detached requires an empty branch.
--placement within-repo-root permits the selected primary or bare control root as
an ancestor while excluding Git administration and other worktrees. Durable provenance is
stored in the worktree's private git directory and returned as JSON so callers
can atomically publish the same evidence on the bead. A new branch is created
from --base, resolved verbatim against the local repository. Detached creation
uses that resolved commit without creating a branch. Failed creation rolls back
only untouched state; --dry-run plans without mutating anything.`,
	}
	cmd.AddCommand(newWorktreeEnsureCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeVerifyCmd(stdout, stderr))
	cmd.AddCommand(newWorktreeCleanupCmd(stdout, stderr))
	return cmd
}

func worktreeFlagSet(cmd *cobra.Command, opts *worktreeCmdOpts) {
	cmd.Flags().StringVar(&opts.Repo, "repo", "", "repository directory the worktree belongs to (required)")
	cmd.Flags().StringVar(&opts.Root, "root", "", "configured per-rig worktree root; path must be its direct child (required)")
	cmd.Flags().StringVar(&opts.Path, "path", "", "worktree path (required)")
	cmd.Flags().StringVar(&opts.Branch, "branch", "", "branch that must be checked out (required in attached mode)")
	cmd.Flags().StringVar(&opts.Mode, "mode", "", "attached or detached HEAD (default attached; explicit modes record schema 2 provenance)")
	cmd.Flags().StringVar(&opts.Placement, "placement", "", "outside-worktrees or within-repo-root (default outside-worktrees)")
	cmd.Flags().StringVar(&opts.Base, "base", "", "exact base ref used for this worktree (required)")
	cmd.Flags().StringVar(&opts.BaseSHA, "base-sha", "", "recorded base SHA to verify when reusing a worktree")
	cmd.Flags().StringVar(&opts.BeadID, "bead", "", "work bead bound to this worktree (required)")
	cmd.Flags().StringVar(&opts.StoreRef, "store-ref", "", "work bead store reference (required)")
	cmd.Flags().StringVar(&opts.Creator, "creator", "", "mechanism creating the worktree (required)")
	cmd.Flags().StringVar(&opts.Owner, "owner", "", "single selected provisioning owner (required)")
	cmd.Flags().StringVar(&opts.Generation, "generation", "", "provisioning generation fence (required)")
	cmd.Flags().StringVar(&opts.Lifecycle, "lifecycle", worktree.LifecycleActive, "worktree lifecycle state")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit the report as JSON")
	for _, name := range []string{
		"repo", "root", "path", "base", "bead", "store-ref", "creator", "owner", "generation",
	} {
		_ = cmd.MarkFlagRequired(name) //nolint:errcheck // flags exist
	}
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("mode") && opts.Mode == "" {
			return fmt.Errorf("--mode must be attached or detached")
		}
		if cmd.Flags().Changed("placement") && opts.Placement == "" {
			return fmt.Errorf("--placement must be outside-worktrees or within-repo-root")
		}
		return nil
	}
}

func newWorktreeEnsureCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "ensure",
		Short: "Ensure the worktree exists and satisfies all postconditions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runWorktreeEnsureContext(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	worktreeLockFlagSet(cmd, &opts)
	cmd.Flags().BoolVarP(&opts.DryRun, "dry-run", "n", false, "plan without mutating anything")
	cmd.Flags().StringVar(&opts.Agent, "agent", "",
		"qualified agent identity to best-effort materialize skills for after creation")
	return cmd
}

func newWorktreeVerifyCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the worktree satisfies all postconditions without mutating",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if runWorktreeVerify(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	return cmd
}

func newWorktreeCleanupCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove an owned worktree after all safety gates pass",
		Long: `Remove an owned worktree after all safety gates pass.

Cleanup verifies the canonical repository, path, HEAD mode, and durable ownership
provenance before acting. It refuses dirty worktrees, commits reachable from no
branch, tag, or remote-tracking ref, and commits not merged into --base.
--attempt-id binds the removal to one exact provisioning attempt, so a stale
request cannot remove a workspace re-created at the same path. There is no
force mode and no recursive-filesystem fallback. An already-absent,
unregistered path is an idempotent success. With --json, safety refusals return
a structured cleanup_pending result for formula automation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runWorktreeCleanupContext(cmd.Context(), opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	worktreeFlagSet(cmd, &opts)
	worktreeLockFlagSet(cmd, &opts)
	cmd.Flags().StringVar(&opts.AttemptID, "attempt-id", "",
		"attempt id returned by the ensure that created this worktree (required)")
	_ = cmd.MarkFlagRequired("attempt-id")
	return cmd
}

func (o worktreeCmdOpts) spec() (worktree.Spec, error) {
	if o.Placement == worktree.PlacementWithinRepoRoot {
		for _, path := range []string{o.Repo, o.Root, o.Path} {
			for _, component := range strings.Split(filepath.ToSlash(path), "/") {
				if component == ".." {
					return worktree.Spec{}, fmt.Errorf("within-repo-root paths must not contain traversal")
				}
			}
		}
	}
	if o.Placement != "" && o.Mode == "" {
		o.Mode = worktree.ModeAttached
	}
	repo, err := filepath.Abs(o.Repo)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --repo %q: %w", o.Repo, err)
	}
	path, err := filepath.Abs(o.Path)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --path %q: %w", o.Path, err)
	}
	root, err := filepath.Abs(o.Root)
	if err != nil {
		return worktree.Spec{}, fmt.Errorf("resolving --root %q: %w", o.Root, err)
	}
	return worktree.Spec{
		RepoDir:     repo,
		Root:        root,
		Path:        path,
		Branch:      o.Branch,
		Mode:        o.Mode,
		Placement:   o.Placement,
		Base:        o.Base,
		BaseSHA:     o.BaseSHA,
		BeadID:      o.BeadID,
		StoreRef:    o.StoreRef,
		Creator:     o.Creator,
		Owner:       o.Owner,
		Generation:  o.Generation,
		Lifecycle:   o.Lifecycle,
		AttemptID:   o.AttemptID,
		DryRun:      o.DryRun,
		LockTimeout: o.LockTimeout,
	}, nil
}

func runWorktreeEnsure(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	return runWorktreeEnsureContext(context.Background(), opts, stdout, stderr)
}

func runWorktreeEnsureContext(ctx context.Context, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree ensure: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.EnsureContext(ctx, spec)
	if err != nil {
		code := ""
		switch {
		case errors.Is(err, worktree.ErrLockTimeout):
			code = worktree.CleanupErrorLockTimeout
		case errors.Is(err, context.Canceled):
			code = worktree.CleanupErrorCanceled
		}
		if opts.JSON && code != "" {
			result := worktreeJSONResult{
				SchemaVersion: "1",
				Command:       "worktree ensure",
				Action:        "ensure",
				Report:        worktree.Report{Path: spec.Path, Branch: spec.Branch},
				Error:         &cliJSONError{Code: code, Message: err.Error(), ExitCode: 1},
			}
			if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
				fmt.Fprintf(stderr, "gc worktree ensure: encoding report: %v\n", encodeErr) //nolint:errcheck
			}
		}
		fmt.Fprintf(stderr, "gc worktree ensure: %v\n", err) //nolint:errcheck
		return 1
	}
	if !opts.DryRun && opts.Agent != "" {
		materializeWorktreeAgentSkills(opts.Agent, rep.Path, stderr)
	}
	return writeWorktreeReport("ensure", rep, opts, stdout, stderr)
}

// materializeWorktreeAgentSkills best-effort materializes agent's skills
// into path, reusing the same resolve-city/resolve-agent/materialize.Run
// sequence the per-session PreStart hook drives via
// `gc internal materialize-skills` (see materializeSkillsIntoWorkdir in
// cmd_internal_materialize_skills.go). A failure at any step is logged to
// stderr and never returned: worktree existence is ensure's postcondition,
// not a materialized skill catalog. All materializer output goes to stderr
// so stdout carries only the ensure report (and stays parseable under --json).
func materializeWorktreeAgentSkills(agentName, path string, stderr io.Writer) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree ensure: skill materialization skipped: city not found: %v\n", err) //nolint:errcheck
		return
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree ensure: skill materialization skipped: city config unavailable: %v\n", err) //nolint:errcheck
		return
	}
	agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
	if !ok {
		fmt.Fprintf(stderr, "gc worktree ensure: skill materialization skipped: unknown agent %q\n", agentName) //nolint:errcheck
		return
	}
	if err := materializeSkillsIntoWorkdir(cfg, &agent, cityPath, path, nil, stderr, stderr); err != nil {
		fmt.Fprintln(stderr, "gc worktree ensure: skill materialization failed (see above)") //nolint:errcheck
	}
}

func runWorktreeVerify(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree verify: %v\n", err) //nolint:errcheck
		return 1
	}
	rep, err := worktree.Verify(spec)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree verify: %v\n", err) //nolint:errcheck
		return 1
	}
	return writeWorktreeReport("verify", rep, opts, stdout, stderr)
}

func runWorktreeCleanup(opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	return runWorktreeCleanupContext(context.Background(), opts, stdout, stderr)
}

func runWorktreeCleanupContext(ctx context.Context, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	spec, err := opts.spec()
	if err != nil {
		report := worktree.CleanupReport{
			Path:           opts.Path,
			Branch:         opts.Branch,
			CleanupPending: true,
			Error: &worktree.CleanupError{
				Code:     worktree.CleanupErrorInvalidSpec,
				Message:  err.Error(),
				ExitCode: 1,
			},
		}
		return writeWorktreeCleanupReport(report, opts, stdout, stderr)
	}
	report, cleanupErr := worktree.CleanupContext(ctx, spec)
	code := writeWorktreeCleanupReport(report, opts, stdout, stderr)
	if cleanupErr != nil {
		return 1
	}
	return code
}

type worktreeJSONResult struct {
	SchemaVersion string        `json:"schema_version"`
	OK            bool          `json:"ok"`
	Command       string        `json:"command"`
	Action        string        `json:"action"`
	Error         *cliJSONError `json:"error,omitempty"`
	worktree.Report
}

func writeWorktreeReport(action string, rep worktree.Report, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	if opts.JSON {
		result := worktreeJSONResult{
			SchemaVersion: "1",
			OK:            true,
			Command:       "worktree " + action,
			Action:        action,
			Report:        rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
	}
	switch {
	case len(rep.Planned) > 0:
		fmt.Fprintf(stdout, "would run (dry-run):\n") //nolint:errcheck
		for _, action := range rep.Planned {
			fmt.Fprintf(stdout, "  %s\n", action) //nolint:errcheck
		}
	case rep.Created:
		if rep.Mode == worktree.ModeDetached {
			fmt.Fprintf(stdout, "created detached worktree %s at %s\n", rep.Path, rep.Head) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "created worktree %s on branch %s at %s\n", rep.Path, rep.Branch, rep.Head) //nolint:errcheck
		}
	default:
		if rep.Mode == worktree.ModeDetached {
			fmt.Fprintf(stdout, "detached worktree %s at %s\n", rep.Path, rep.Head) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "worktree %s on branch %s at %s\n", rep.Path, rep.Branch, rep.Head) //nolint:errcheck
		}
	}
	return 0
}

type worktreeCleanupJSONResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Command       string `json:"command"`
	Action        string `json:"action"`
	worktree.CleanupReport
}

func writeWorktreeCleanupReport(rep worktree.CleanupReport, opts worktreeCmdOpts, stdout, stderr io.Writer) int {
	if opts.JSON {
		result := worktreeCleanupJSONResult{
			SchemaVersion: "1",
			OK:            rep.Error == nil,
			Command:       "worktree cleanup",
			Action:        "cleanup",
			CleanupReport: rep,
		}
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc worktree cleanup: encoding report: %v\n", err) //nolint:errcheck
			return 1
		}
		if rep.Error != nil {
			fmt.Fprintf(stderr, "gc worktree cleanup: %s\n", rep.Error.Message) //nolint:errcheck
			return 1
		}
		return 0
	}
	if rep.Error != nil {
		fmt.Fprintf(stderr, "gc worktree cleanup: %s\n", rep.Error.Message) //nolint:errcheck
		return 1
	}
	switch {
	case rep.AlreadyAbsent:
		fmt.Fprintf(stdout, "worktree %s is already absent\n", rep.Path) //nolint:errcheck
	case rep.Removed:
		if rep.Mode == worktree.ModeDetached {
			fmt.Fprintf(stdout, "removed detached worktree %s\n", rep.Path) //nolint:errcheck
		} else {
			fmt.Fprintf(stdout, "removed worktree %s on branch %s\n", rep.Path, rep.Branch) //nolint:errcheck
		}
	default:
		fmt.Fprintf(stdout, "worktree %s required no cleanup\n", rep.Path) //nolint:errcheck
	}
	return 0
}
