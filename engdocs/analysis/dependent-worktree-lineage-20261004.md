---
title: "Dependent worktree lineage: source decision and first native increment"
description: "Pinned source findings, managed-base guard, deterministic fixture scope, and remaining delivery gates."
---

The first increment strengthens Gas City's existing managed workspace owner:
a managed worktree must contain its recorded input commit. It does not add a
scheduler, select integration policy, or claim accepted PM delivery.

## Source decision

Source inspection on 2026-10-04 separated these revisions:

| Surface | Exact revision | Disposition |
| --- | --- | --- |
| Installed GC 1.4.2 | `d4582166367aa687c1b62b296247ba0fb0a7e094` | Unchanged; lacks the selected managed-worktree module. |
| Private engine used by scope49 | `00d1127b16132c3a2d81d63b1594867024f625af` | Historical scoped proof; not installed adoption. |
| Observed fork default main | `24bb1b70cf53d50fa58ea41257992d3ad2d1d9d7` | Older default branch; active PR branch selected explicitly. |
| Nick's fork PR6847 | `8b3324c9cf093449f9d7a2ade8248fc870d21b90` | Open; base of this isolated increment. |
| Observed upstream main | `aa66f2b601a8314b635894d8aa2779e43c141db3` | Source inspected, not installed. Managed Verify has the same missing ancestry guard. |
| Observed pack main | `714ae5c017080ae5040a34c64cacc6b7c425f39c` | Existing do-work setup still uses a fetched default tip and plain work_dir. |
| Pack PR214 | `bdece0172f21f59efa0bc8a29e4b86c86c7f49a8` | Other author's open proposal, not adopted. |
| Reviewed PM adapter | `5a4a6c5954d69e96f2ee706506baebd57afd9e9b` | 308 author offline tests and 122 affected independent checks; no full live proof. |

[PR6847](https://github.com/gastownhall/gascity/pull/6847) repairs rig-store
refresh after direct suspension changes. Its completion-signal fix is separate
from branch lineage. [PR214](https://github.com/gastownhall/gascity-packs/pull/214)
proposes deterministic workspace lifecycle; it is not a landed dependency.
Recent [PR6992](https://github.com/gastownhall/gascity/pull/6992) concerns terminal
step closure before drain acknowledgement; [PR5095](https://github.com/gastownhall/gascity/pull/5095)
concerns named-session workdir identity. Neither supplies the missing managed
input-commit ancestry assertion found in the pinned worktree source.

The old native capability digest pins are lookup guidance only. This decision
uses the actual installed, fork, upstream, and pack trees above.

## What the historical evidence proves

- Attempt37a63dec ran independent Pi1.0.2 worktrees and produced tested help
  `4bc9f961723705b9c07c37834667335057ac8113` and API
  `66d9f6d185cd39d49e493ae2a16363dfb7e664b3`. Stale native closure reads,
  terminal reservation re-entry, and early receiver cleanup blocked overall
  acceptance. Those failures do not demonstrate inability to parallelize.
- `91365d61251e7c1d4f8e0ee763830900df51fba6` has an independent exact-head
  acceptance receipt classified ASSISTED_CONTROLLED_DELIVERY_INDEPENDENTLY_ACCEPTED.
  That receipt explicitly holds clean repeat and gives no singleton, crash
  recovery, general CAS, or PM product-feedback credit.
- Scope49's clean `e30d172a89ade6c6556fac9b38e60337424c882b` proved A then B
  in one shared branch/worktree, combined independent review, original-source
  and convoy closure, event return, and natural drain on private engine00d1127b
  and pack5ca06292. The parent verified 209 sealed files. This is a serial
  dependent-source proof, not parallel dependent-worktree proof.
- Existing continuation source `1feac8ed06189757dc069a62514186b0bcc73fc8`
  already addresses concrete pool launch workdirs, retry workdirs, frozen
  descriptions, and assigned task launch options. Reuse applicable native
  behavior instead of rebuilding a coordinator or treating old tracker gaps
  as current source facts.

## Contract and ownership

The native graph remains the sole scheduler. Drain projects source dependencies
onto item roots; work/check/control boundaries own readiness and completion.
Dependency closure establishes ordering, not Git input identity.

The installed file-store agent-script route selects ready work without an
atomic bd claim. Its terminal per-role adapter reservation limits effects in
that fixed attempt; it is not a general source singleton or crash-replay CAS.
This workspace patch does not change claim behavior or establish those
guarantees on a different store.

| Owner | Required behavior |
| --- | --- |
| Gas City engine | Durable dependencies and claim/lifecycle transitions; transactional managed Ensure/Verify; launch in the selected workdir; preserve supported retry identity; emit native results. |
| Pack and project policy | Freeze independent A/B input commit P; integrate checked outputs into exact commit M; bind C to M and its parent source/output identities; bind D to accepted C output or its explicitly verified integration; produce exact integrated-head tests and distinct review. |
| PM adapter | Persist the genuine originating request and original source/store ID before sling; distinguish source from workflow root; verify source/root/store/result correlation and accepted artifact evidence; return the accepted result to the originating PM context. |

For a managed workspace, existing Spec/Provenance fields remain authoritative:
repository identity, direct-child configured root/path, branch, exact BaseSHA,
bead/store, creator/owner, generation/lifecycle, and provisioning AttemptID.
The branch HEAD must contain BaseSHA; legitimate descendant commits remain
valid. Missing managed paths are creatable only through the existing transaction.
An existing stale branch is refused before planning or creation. Verification
of an already occupied invalid path must preserve the user's work.

This increment uses the existing isAncestor helper, existing managed/unmanaged
split, and existing transaction/rollback. It changes no ownership keys, schemas,
session selection, attempt budgets, cleanup policy, or native claim semantics.

A source worktree belongs to the original source anchor. Pack setup must retain
that ownership when a generated step accesses it: copying all source ownership
keys onto a different step bead would falsely change the provenance BeadID.
Current plain work_dir recipe steps remain unmanaged and do not gain protection
from this guard. The pack's source-anchor resolution and complete provenance
handoff remain a required follow-up.

Root closure alone, warn-only close gates, a claimed worker, a successful model
exit, or tests of an individual branch do not prove shipped integration.
Acceptance must bind the actual final integrated HEAD to its tests and a
distinct independent review, native source/root/store outcome, and genuine PM
request. Manual fixture IDs do not count as PM intake.

## Proof scope

The smallest owning tests use real temporary Git repositories for descendant
acceptance, reset rejection, and a stale preexisting branch including pure
dry-run refusal and preservation. Existing worktree tests cover transactional
rollback, ownership, concurrent provisioning, and cleanup fences.

The CLI testscript `cmd/gc/testdata/worktree-dependent-lineage.txtar` uses real
`gc init --no-start` with existing test-only fake-session/file-store settings.
Concurrent worker stand-ins write A/B in separate managed worktrees from P.
Actual Git integration creates M; C uses exact M, D uses C's exact output; a
fresh workspace checks the distinct final integrated HEAD. The stale C branch
is rejected through the public CLI. No native supervisor, model, or live
worker starts. This proves managed workspace composition, not live scheduling,
atomic graph admission, independent model review, or returned PM acceptance.

## Installation and remaining gates

This branch is source-only and local. Installed GC/Pi, shared config, services,
product branches, spent receipts, UC07/f8 holds, and denied private reads remain
unchanged. Pi1.0.2 direct is the worker target; OpenClaw is not a worker
prerequisite.

The guard cannot be applied alone to installed d458, which lacks this module.
Any runtime adoption must select the reviewed fork revision and build its own
binary in a fresh owner-controlled disposable scope; do not replace
`/home/ubuntu/.local/bin/gc`. That candidate would reject managed branches that
lack their recorded input commit. Pack provenance/parent-output handoff must
also be reviewed before claiming protection for do-work delivery.

No installation or live attempt is admitted here. Root must reconcile the sole
native owner01a10013-2d2f-7a53-a256-d35d1d0ebeae and cloud relay, select a fresh
immutable budget tied to the exact reviewed fix, and retain all spent cohorts
before any disposable live run. End-to-end parallel dependent worktrees,
integrated-head independent model acceptance, and genuine PM intake/result
return remain unproved.
