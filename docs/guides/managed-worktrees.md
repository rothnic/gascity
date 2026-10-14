---
title: Configure Managed Worktrees
description: Choose workspace inputs and reuse policies for pack workflows.
---

Packs decide where work runs and how its checkout is prepared. Managed
worktrees provide separate checkouts on an attached branch or detached HEAD,
with checks that the workspace belongs to its owner and contains its required
starting commit.

This guide covers workspace choices, the input to supply, and the behavior
you can expect from `gc worktree`. See [Understanding Packs](/guides/understanding-packs)
for importing and customizing a pack, and the
[worktree CLI reference](/reference/cli#gc-worktree) for the complete flag list.

## Choose A Workspace Approach

| Approach | Where you configure it | Behavior |
|---|---|---|
| Shared checkout | The worker's `work_dir` | Work runs in the selected directory. Tasks share its files and branch. |
| Git worktree | A pack's preparation script or formula step runs `git worktree add` | Tasks have separate checkouts. The pack implements input selection, ownership and reuse checks. |
| Managed worktree | A pack's preparation script or formula step calls `gc worktree ensure` and `gc worktree verify` | Each workspace has recorded ownership and an attached branch by default. Explicit detached mode uses the supplied commit without creating a branch. Reuse checks identity and required input ancestry. |

Setting `work_dir` chooses a working directory; it does not create a worktree
or enable the managed-worktree checks. Check the preparation step in the pack
version you use to see which approach it implements.

## Choose The Input Commit

Supply `--base` with the commit the task needs to start from. It must be
available in the local repository. For independent work, this might be a
selected commit from the project's default branch. For dependent work, it
should be the result of integrating the accepted prerequisite outputs.

For a fixed input with later cleanup after merge, supply the merge branch ref
as `--base` and the selected input SHA as `--base-sha`. At creation, the ref
must resolve to that SHA; the commit is recorded for later reuse checks.
Keep the original ref and SHA when verifying or reusing the workspace. The
worktree commands do not fetch or integrate prerequisite outputs for you.

## Prepare A Managed Workspace

Call `gc worktree ensure` from the pack's workspace preparation step. Give each
workspace a distinct path and keep its ownership values consistent when
verifying or reusing it. Attached workspaces also need a distinct branch.

For example, replace these paths, input SHA and work identity with the values
selected by your pack:

```bash
gc worktree ensure \
  --repo /path/to/project --root /path/to/workspaces \
  --path /path/to/workspaces/task-c --branch work/task-c \
  --base refs/heads/integration --base-sha REPLACE_WITH_INPUT_COMMIT_SHA \
  --bead task-c --store-ref city:my-city \
  --creator do-work --owner workspace-provisioner \
  --generation attempt-1 --json
```

For a detached workspace, add `--mode detached` and omit `--branch` from the
same command. Its initial HEAD must equal the resolved input commit. Creation
does not create, adopt or delete a branch. An existing attached branch may
already contain later commits when it satisfies the input ancestry check.

The path must be a direct child of `--root`. The bead and store identify the
work item that owns the workspace; creator and owner identify its provisioner.
Generation must match when the workspace is reused. Each creation also gets
an attempt ID, which binds
cleanup to that specific workspace.

The command returns a JSON report with the workspace and its recorded
provenance. The preparation step should retain that report and give the worker
the resulting workspace path only after the command succeeds.

Omitting `--mode` retains attached behavior and creates schema 1 provenance.
New creation with explicit `--mode attached` or `--mode detached` records
schema 2 provenance with the mode and `outside-worktrees` placement. Schema 1
records remain readable as
attached workspaces without rewriting their files. Both modes use the same
existing placement rules.

When publishing schema 2 ownership metadata for native launch verification,
carry both `gc.worktree_mode` and `gc.worktree_placement` from the provenance
alongside the existing repository, path, owner and input fields. Detached
ownership has an empty branch value. Missing, empty or unknown mode/placement
evidence refuses verification instead of falling back to an unmanaged path.

| Option | Use it to |
|---|---|
| `--mode` | Choose `attached` with a branch or `detached` with an empty branch. Use the same mode for creation, verification and cleanup. |
| `--base` | Select the required starting commit or a locally resolvable ref. |
| `--base-sha` | Require the expected input SHA at creation and check it on reuse. |
| `--dry-run` | Plan creation or reuse without changing the repository or filesystem. The same input checks apply. |
| `--lock-timeout` | Set a positive maximum wait for the workspace lock, such as `5s`. Ensure and cleanup default to `30s`. |
| `--json` | Return a structured report for the pack's next step. |

Ensure and cleanup serialize changes to each workspace path. The timeout
bounds lock acquisition; an admitted transaction continues under the lock until
its checks and changes finish. With `--json`, an expired wait returns `ok: false`
and `error.code: "lock_timeout"`. Cleanup also returns `cleanup_pending: true`
and `removed: false`. Retry after the operation holding the lock has finished.

Verify and `ensure --dry-run` neither wait for the lock nor create a lock file.

Use `gc worktree verify` with the matching path, mode, branch when attached,
ownership and input flags to check a workspace without changing it. See the
[configuration reference](/reference/config) for `work_dir` path handling and
[Understanding Formulas](/guides/understanding-formulas) for composing preparation
and implementation steps.

## Reuse And Recovery

A managed workspace is reusable when its identity matches and its current
commit descends from the recorded input. Additional commits remain usable.
If that input is missing from HEAD's history, verification and reuse fail,
including dry-run planning. Wrong mode or ownership also refuses reuse. The
refusal leaves existing refs, commits, provenance and working files intact.

If you see `HEAD ... does not contain recorded base ...`:

1. Inspect HEAD and any uncommitted work.
2. Create a distinct workspace from the required input, or review and
   integrate that input into the existing checkout.
3. Verify the selected workspace with its matching identity and input before
   continuing.

Changing the workflow's input requires an explicit workspace decision. The
commands do not automatically reset HEAD or update its recorded base.
Later commits retain the workspace owner's original input and attempt identity;
the pack separately records each workflow member's exact input and output.

## Guarantees For Dependent Work

Managed-worktree verification checks the repository, path, declared HEAD mode,
branch when attached, recorded ownership and input ancestry. It proves that
the required commit is in the checkout's history.

The pack still controls two other parts of delivery:

- **Input selection:** wait for the required work, select accepted outputs,
  integrate them, and supply that exact result as the next task's input.
- **Behavior:** run project tests and any task-specific checks against the
  resulting commit, and bind review to that same commit. A later change needs
  fresh evidence; retaining the input in history does not ensure its behavior
  remains intact.

For removal, use [gc worktree cleanup](/reference/cli#gc-worktree-cleanup) with
the attempt ID returned during creation. Cleanup also checks ownership and Git
safety. It refuses dirty workspaces, commits reachable from no branch, tag or
remote-tracking ref, and HEADs not merged into `--base`.

Rollback also preserves uncommitted work. For a detached checkout, it refuses
to remove an advanced HEAD even when the working tree is clean, because no
branch remains to protect those commits. A failed creation that cannot prove
an untouched, attempt-owned checkout leaves it for inspection.

Use the same base ref and input SHA supplied at creation. Cleanup checks the
current tip of that ref for the merge. A fixed SHA supplied as `--base` remains
the original commit, so cleanup refuses later commits even if they were merged
elsewhere. Choose the merge branch ref at creation when post-merge cleanup is
needed.

## Place a workspace beneath the repository

Use `--placement within-repo-root` to put a workspace beneath the selected
primary repository or bare control root. The workspace remains a direct child
of `--root`. Git administration directories and other linked or foreign
worktrees remain excluded. For a bare control root, use a parent such as
`<repo>/managed`; `<repo>/worktrees` is Git's administration namespace.

Explicit placement records schema 2 provenance. Omitted mode and placement
retain attached, outside-worktrees behavior. New placement rejects traversal
and symlink aliases before provisioning or reuse.

## Share one native owner across steps

Publish `gc.worktree_ref` as a JSON string on each invoking step:

```json
{
  "version": 1,
  "binding_step_id": "step-1",
  "workflow_root_id": "run-1",
  "generation": 1,
  "workspace_owner_id": "source-owner",
  "workspace_bead_id": "workspace-1",
  "store_ref": "rig:project",
  "native_attempt_id": "attempt-returned-by-ensure"
}
```

The workspace bead publishes the native owner fields, `gc.root_bead_id`,
`gc.root_store_ref`, `gc.worktree_mode`, and `gc.worktree_placement`. Its bead
ID and owner remain stable as different steps invoke it. Launch and resume
read the step and owner in that exact store, verify the native workspace and
compare the published provisioning attempt. A declared invalid reference
refuses launch even when a cached directory exists. Workflow source selection
and member acceptance remain the pack's responsibility.
