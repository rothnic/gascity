package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

func TestBindPoolSessionTriggerBeadVerifiesManagedWorktreeBeforePublishing(t *testing.T) {
	for _, mode := range []string{"", worktree.ModeAttached, worktree.ModeDetached} {
		t.Run("mode="+mode, func(t *testing.T) {
			repo, base := worktreeTestRepo(t)
			root := t.TempDir()
			path := filepath.Join(root, "gc-test")
			spec := worktree.Spec{
				RepoDir: repo, Root: root, Path: path, Branch: "work/gc-test", Base: base,
				BeadID: "gc-test", StoreRef: "rig:gascity", Creator: "gc-sling",
				Owner: "gc-sling", Generation: "1", Lifecycle: worktree.LifecycleActive,
			}
			spec.Mode = mode
			if mode == worktree.ModeDetached {
				spec.Branch = ""
			}
			report, err := worktree.Ensure(spec)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			spec.BaseSHA = report.Provenance.BaseSHA

			store := beads.NewMemStore()
			sessionBead, err := store.Create(beads.Bead{ID: "session-1", Type: "session"})
			if err != nil {
				t.Fatalf("Create session bead: %v", err)
			}
			info, err := sessionFrontDoor(store).Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("Get session info: %v", err)
			}
			cfg := &config.City{
				Workspace: config.Workspace{Name: "city"},
				Agents: []config.Agent{{
					Name: "worker", WorkDir: filepath.Join(root, "slot"),
				}},
			}
			bp := newAgentBuildParams("city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &bytes.Buffer{})

			bound, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, SessionRequest{
				WorkBeadID: "gc-test", WorkStoreRef: "rig:gascity", WorktreeSpec: &spec,
			})
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if bound.WorkDirCanonical != path || bound.WorkDir != path {
				t.Fatalf("bound work dirs = canonical %q legacy %q, want verified %q", bound.WorkDirCanonical, bound.WorkDir, path)
			}
			persisted, err := store.Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("Get persisted session: %v", err)
			}
			if persisted.Metadata[beadmeta.WorkDirMetadataKey] != path ||
				persisted.Metadata[beadmeta.LegacyWorkDirMetadataKey] != path {
				t.Fatalf("persisted metadata = %+v, want verified work dir twins", persisted.Metadata)
			}
		})
	}
}

func TestBindPoolSessionTriggerBeadRejectsCompetingOwnerBeforeMetadata(t *testing.T) {
	for _, mode := range []string{"", worktree.ModeDetached} {
		t.Run("mode="+mode, func(t *testing.T) {
			repo, base := worktreeTestRepo(t)
			root := t.TempDir()
			path := filepath.Join(root, "gc-test")
			spec := worktree.Spec{
				RepoDir: repo, Root: root, Path: path, Branch: "work/gc-test", Base: base,
				BeadID: "gc-test", StoreRef: "rig:gascity", Creator: "gc-sling",
				Owner: "gc-sling", Generation: "1", Lifecycle: worktree.LifecycleActive,
			}
			spec.Mode = mode
			if mode == worktree.ModeDetached {
				spec.Branch = ""
			}
			report, err := worktree.Ensure(spec)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			spec.BaseSHA = report.Provenance.BaseSHA
			spec.Owner = "formula"

			store := beads.NewMemStore()
			sessionBead, err := store.Create(beads.Bead{ID: "session-1", Type: "session"})
			if err != nil {
				t.Fatalf("Create session bead: %v", err)
			}
			info, err := sessionFrontDoor(store).Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("Get session info: %v", err)
			}
			cfg := &config.City{Workspace: config.Workspace{Name: "city"}, Agents: []config.Agent{{Name: "worker"}}}
			bp := newAgentBuildParams("city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &bytes.Buffer{})

			if _, err := bindPoolSessionTriggerBead(bp, &cfg.Agents[0], "worker", info, SessionRequest{
				WorkBeadID: "gc-test", WorkStoreRef: "rig:gascity", WorktreeSpec: &spec,
			}); err == nil {
				t.Fatal("bind accepted competing worktree owner")
			}
			persisted, err := store.Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("Get persisted session: %v", err)
			}
			if persisted.Metadata[beadmeta.WorkDirMetadataKey] != "" ||
				persisted.Metadata[beadmeta.LegacyWorkDirMetadataKey] != "" ||
				persisted.Metadata[beadmeta.TriggerBeadIDMetadataKey] != "" {
				t.Fatalf("failed verification published partial metadata: %+v", persisted.Metadata)
			}
		})
	}
}

func TestWorktreeSpecForBeadRequiresCompletePublishedEvidence(t *testing.T) {
	metadata := map[string]string{
		beadmeta.WorkDirMetadataKey:            "/worktrees/gc-test",
		beadmeta.WorkBranchMetadataKey:         "work/gc-test",
		beadmeta.WorktreeRootMetadataKey:       "/worktrees",
		beadmeta.WorktreeRepoMetadataKey:       "/repos/gascity",
		beadmeta.WorktreeBaseRefMetadataKey:    "main",
		beadmeta.WorktreeBaseSHAMetadataKey:    strings.Repeat("a", 40),
		beadmeta.WorktreeCreatorMetadataKey:    "gc-sling",
		beadmeta.WorktreeOwnerMetadataKey:      "gc-sling",
		beadmeta.WorktreeGenerationMetadataKey: "7",
		beadmeta.WorktreeLifecycleMetadataKey:  worktree.LifecycleActive,
	}
	bead := beads.Bead{ID: "gc-test", Metadata: metadata}

	spec, err := worktreeSpecForBead(bead, "rig:gascity")
	if err != nil {
		t.Fatalf("worktreeSpecForBead: %v", err)
	}
	if spec == nil || spec.BeadID != bead.ID || spec.StoreRef != "rig:gascity" ||
		spec.RepoDir != metadata[beadmeta.WorktreeRepoMetadataKey] ||
		spec.Root != metadata[beadmeta.WorktreeRootMetadataKey] ||
		spec.Path != metadata[beadmeta.WorkDirMetadataKey] ||
		spec.Branch != metadata[beadmeta.WorkBranchMetadataKey] ||
		spec.Base != metadata[beadmeta.WorktreeBaseRefMetadataKey] ||
		spec.BaseSHA != metadata[beadmeta.WorktreeBaseSHAMetadataKey] ||
		spec.Creator != metadata[beadmeta.WorktreeCreatorMetadataKey] ||
		spec.Owner != metadata[beadmeta.WorktreeOwnerMetadataKey] ||
		spec.Generation != metadata[beadmeta.WorktreeGenerationMetadataKey] ||
		spec.Lifecycle != metadata[beadmeta.WorktreeLifecycleMetadataKey] {
		t.Fatalf("spec = %+v, want exact published evidence", spec)
	}
	for _, mode := range []string{worktree.ModeAttached, worktree.ModeDetached} {
		published := maps.Clone(metadata)
		published[beadmeta.WorktreeModeMetadataKey] = mode
		published[beadmeta.WorktreePlacementMetadataKey] = worktree.PlacementOutsideWorktrees
		if mode == worktree.ModeDetached {
			delete(published, beadmeta.WorkBranchMetadataKey)
		}
		decoded, err := worktreeSpecForBead(beads.Bead{ID: bead.ID, Metadata: published}, "rig:gascity")
		if err != nil || decoded == nil || decoded.Mode != mode || decoded.Placement != worktree.PlacementOutsideWorktrees ||
			decoded.BeadID != bead.ID || decoded.BaseSHA != spec.BaseSHA || decoded.Owner != spec.Owner ||
			(mode == worktree.ModeDetached && decoded.Branch != "") {
			t.Fatalf("mode-aware published input = %+v, %v", decoded, err)
		}
	}

	delete(metadata, beadmeta.WorktreeOwnerMetadataKey)
	if _, err := worktreeSpecForBead(bead, "rig:gascity"); err == nil ||
		!strings.Contains(err.Error(), beadmeta.WorktreeOwnerMetadataKey) {
		t.Fatalf("incomplete evidence error = %v, want missing owner key", err)
	}

	delete(metadata, beadmeta.WorkDirMetadataKey)
	if spec, err := worktreeSpecForBead(bead, "rig:gascity"); err != nil || spec != nil {
		t.Fatalf("bead without work_dir = spec %+v err %v, want no managed workspace", spec, err)
	}
}

func TestWorktreeSpecForBeadRejectsInvalidModeEvidence(t *testing.T) {
	for _, metadata := range []map[string]string{
		{beadmeta.WorktreeModeMetadataKey: ""},
		{beadmeta.WorktreePlacementMetadataKey: ""},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorktreeModeMetadataKey: worktree.ModeDetached},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorktreeModeMetadataKey: "unknown", beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorktreeModeMetadataKey: worktree.ModeDetached, beadmeta.WorktreePlacementMetadataKey: "within-repo-root"},
		{beadmeta.WorktreeModeMetadataKey: worktree.ModeDetached, beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorktreeModeMetadataKey: worktree.ModeDetached, beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorkBranchMetadataKey: "ambient", beadmeta.WorktreeModeMetadataKey: worktree.ModeDetached, beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees},
		{beadmeta.WorkDirMetadataKey: "/worktrees/owned", beadmeta.WorkBranchMetadataKey: "ambient", beadmeta.WorktreeModeMetadataKey: worktree.ModeAttached, beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees},
	} {
		if spec, err := worktreeSpecForBead(beads.Bead{ID: "gc-test", Metadata: metadata}, "rig:gascity"); err == nil || spec != nil {
			t.Fatalf("new mode declaration fell through to unmanaged: metadata=%v, spec=%+v, err=%v", metadata, spec, err)
		}
	}
}

// TestWorktreeSpecForBeadTreatsWorkDirOnlyAsLegacy covers a bead carrying
// work_dir without ownership evidence -- the shape stampDrainItemRecipe still
// mints for every drain recipe step. That is not "incomplete evidence" (the
// bead never claimed to publish any), so the pool must plan the seat unmanaged
// instead of erroring it into permanent starvation. Both work_dir spellings
// are covered: old beads carry the bare key, not the gc.-prefixed one.
func TestWorktreeSpecForBeadTreatsWorkDirOnlyAsLegacy(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{name: "canonical", key: beadmeta.WorkDirMetadataKey},
		{name: "legacy", key: beadmeta.LegacyWorkDirMetadataKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bead := beads.Bead{ID: "gc-test", Metadata: map[string]string{
				tc.key: "/worktrees/gc-test",
			}}

			spec, err := worktreeSpecForBead(bead, "rig:gascity")
			if err != nil {
				t.Fatalf("worktreeSpecForBead: %v, want a work_dir-only bead to be treated as unmanaged, not errored", err)
			}
			if spec != nil {
				t.Fatalf("spec = %+v, want nil so the seat spawns exactly as it did before #5193", spec)
			}
		})
	}
}

// TestWorktreeSpecForBeadTreatsWorkBranchOnlyAsUnmanaged covers the shape a
// hook claim's ambient branch stamp actually produces on a legitimately
// unmanaged bead (ga-ryeij1.1): work_dir + work_branch set, none of the other
// eight ownership keys published. A hook claim stamps gc.work_branch alone
// the moment it adopts any bead with a resolvable worktree branch, regardless
// of whether the bead ever published ownership evidence -- so this shape is
// not "incomplete evidence" any more than the zero-key case above is. Treat
// it as unmanaged, not a hard error, or the pool seat starves permanently the
// first time a hook claim touches it.
func TestWorktreeSpecForBeadTreatsWorkBranchOnlyAsUnmanaged(t *testing.T) {
	bead := beads.Bead{ID: "gc-test", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey:    "/worktrees/gc-test",
		beadmeta.WorkBranchMetadataKey: "work/gc-test",
	}}

	spec, err := worktreeSpecForBead(bead, "rig:gascity")
	if err != nil {
		t.Fatalf("worktreeSpecForBead: %v, want a work_dir+work_branch-only bead to be treated as unmanaged, not errored", err)
	}
	if spec != nil {
		t.Fatalf("spec = %+v, want nil so the seat spawns unmanaged instead of starving", spec)
	}
}

// TestWorktreeSpecForBeadRejectsSingleOwnershipKey pins the boundary the
// unmanaged branch creates: nine missing ownership keys is "never published",
// but eight missing is partial evidence and must still fail closed. An
// off-by-one in the len(missing) == len(values) test would silently launch a
// seat into a directory whose ownership was only half-published.
func TestWorktreeSpecForBeadRejectsSingleOwnershipKey(t *testing.T) {
	bead := beads.Bead{ID: "gc-test", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey: "/worktrees/gc-test",
		// Exactly one of the nine, and deliberately the last one checked, so
		// the reported missing key is the first of the remaining eight.
		beadmeta.WorktreeLifecycleMetadataKey: worktree.LifecycleActive,
	}}

	spec, err := worktreeSpecForBead(bead, "rig:gascity")
	if err == nil {
		t.Fatalf("spec = %+v err = nil, want partial ownership evidence to fail closed", spec)
	}
	if spec != nil {
		t.Fatalf("spec = %+v, want nil alongside the error", spec)
	}
	if !strings.Contains(err.Error(), beadmeta.WorktreeRepoMetadataKey) {
		t.Fatalf("error = %v, want it to name the first missing key %q", err, beadmeta.WorktreeRepoMetadataKey)
	}
}

// TestWorktreeSpecForBeadRejectsWorkBranchPlusOneOtherKey pins the boundary
// the work_branch-only carve-out below must NOT swallow: work_dir +
// work_branch plus exactly one of the other eight ownership keys is still a
// partial, half-published shape and must fail closed exactly like
// TestWorktreeSpecForBeadRejectsSingleOwnershipKey above. An overly broad
// carve-out that matched on "work_branch present" rather than "work_branch is
// the ONLY one of the nine present" would wrongly wave this shape through as
// unmanaged too.
func TestWorktreeSpecForBeadRejectsWorkBranchPlusOneOtherKey(t *testing.T) {
	bead := beads.Bead{ID: "gc-test", Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey:       "/worktrees/gc-test",
		beadmeta.WorkBranchMetadataKey:    "work/gc-test",
		beadmeta.WorktreeOwnerMetadataKey: "gc-sling",
	}}

	spec, err := worktreeSpecForBead(bead, "rig:gascity")
	if err == nil {
		t.Fatalf("spec = %+v err = nil, want work_branch plus one other key to still fail closed", spec)
	}
	if spec != nil {
		t.Fatalf("spec = %+v, want nil alongside the error", spec)
	}
}

func TestWorktreeSpecForBeadPrefersCanonicalStoreRef(t *testing.T) {
	metadata := map[string]string{
		beadmeta.WorkDirMetadataKey:            "/worktrees/gc-test",
		beadmeta.WorkBranchMetadataKey:         "work/gc-test",
		beadmeta.WorktreeRootMetadataKey:       "/worktrees",
		beadmeta.WorktreeRepoMetadataKey:       "/repos/gascity",
		beadmeta.WorktreeBaseRefMetadataKey:    "main",
		beadmeta.WorktreeBaseSHAMetadataKey:    strings.Repeat("a", 40),
		beadmeta.WorktreeCreatorMetadataKey:    "gc-sling",
		beadmeta.WorktreeOwnerMetadataKey:      "gc-sling",
		beadmeta.WorktreeGenerationMetadataKey: "7",
		beadmeta.WorktreeLifecycleMetadataKey:  worktree.LifecycleActive,
		beadmeta.RootStoreRefMetadataKey:       "city:test-city",
	}
	bead := beads.Bead{ID: "gc-test", Metadata: metadata}

	// The probe shorthand "city" would not match the provenance the creating
	// side published under the canonical ref, so verification would refuse a
	// workspace that is ours.
	spec, err := worktreeSpecForBead(bead, "city")
	if err != nil {
		t.Fatalf("worktreeSpecForBead: %v", err)
	}
	if spec.StoreRef != "city:test-city" {
		t.Fatalf("StoreRef = %q, want the bead's canonical %q", spec.StoreRef, "city:test-city")
	}

	delete(metadata, beadmeta.RootStoreRefMetadataKey)
	fallback, err := worktreeSpecForBead(beads.Bead{ID: "gc-test", Metadata: metadata}, "city")
	if err != nil {
		t.Fatalf("worktreeSpecForBead without canonical ref: %v", err)
	}
	if fallback.StoreRef != "city" {
		t.Fatalf("fallback StoreRef = %q, want the caller's %q", fallback.StoreRef, "city")
	}
}

func TestWorktreeReferencePresenceRefusesUnmanagedCompatibility(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "{", `{"version":2}`, `{"version":1,"generation":"1"}`} {
		t.Run(raw, func(t *testing.T) {
			bead := beads.Bead{ID: "step", Metadata: map[string]string{"gc.worktree_ref": raw, "work_dir": t.TempDir()}}
			if spec, err := worktreeSpecForBead(bead, "city:fixture"); err == nil || spec != nil {
				t.Fatalf("declared reference reached unmanaged compatibility: %+v, %v", spec, err)
			}
		})
	}
}

func TestPoolTriggerNativeReferencePlanOnlyDefersVerification(t *testing.T) {
	ref := &worktreeReference{BindingStepID: "step", StoreRef: "city:fixture"}
	request := SessionRequest{WorkBeadID: "step", WorkStoreRef: ref.StoreRef, WorktreeRef: ref}
	plan := &agentBuildParams{planOnly: true}
	if dir, err := verifiedPoolTriggerWorkDir(plan, nil, "worker", request); dir != "" || err != nil {
		t.Fatalf("plan-only native reference read a missing work store: dir=%q err=%v", dir, err)
	}
	request.WorkBeadID = "other-step"
	if _, err := verifiedPoolTriggerWorkDir(plan, nil, "worker", request); err == nil {
		t.Fatal("plan-only reference accepted another invoking step")
	}
	request.WorkBeadID = "step"
	request.WorkStoreRef = "rig:other"
	if _, err := verifiedPoolTriggerWorkDir(plan, nil, "worker", request); err == nil {
		t.Fatal("plan-only reference accepted another selected store")
	}
}

func TestSharedWorktreeReferenceBinding(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	parent := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: parent, Path: filepath.Join(parent, "owner"), Mode: worktree.ModeDetached,
		Base: base, BeadID: "physical-workspace", StoreRef: "city:fixture", Creator: "test", Owner: "stable-owner", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: "session", Type: sessionpkg.BeadType}); err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(beads.Bead{ID: spec.BeadID, Metadata: map[string]string{
		beadmeta.WorkDirMetadataKey: spec.Path, beadmeta.WorktreeRepoMetadataKey: spec.RepoDir,
		beadmeta.WorktreeRootMetadataKey: spec.Root, beadmeta.WorktreeBaseRefMetadataKey: spec.Base,
		beadmeta.WorktreeBaseSHAMetadataKey: report.Provenance.BaseSHA, beadmeta.WorktreeCreatorMetadataKey: spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey: spec.Owner, beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
		beadmeta.WorktreeLifecycleMetadataKey: spec.Lifecycle, beadmeta.RootBeadIDMetadataKey: "workflow-root",
		beadmeta.RootStoreRefMetadataKey: spec.StoreRef, beadmeta.WorktreeModeMetadataKey: spec.Mode,
		beadmeta.WorktreePlacementMetadataKey: worktree.PlacementOutsideWorktrees,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"member-a", "member-b"} {
		ref := map[string]any{
			"version": 1, "binding_step_id": id, "workflow_root_id": "workflow-root", "generation": 1,
			"workspace_owner_id": spec.Owner, "workspace_bead_id": owner.ID, "store_ref": spec.StoreRef, "native_attempt_id": report.Provenance.AttemptID,
		}
		raw, err := json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		step, err := store.Create(beads.Bead{ID: id, Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "workflow-root", beadmeta.RootStoreRefMetadataKey: spec.StoreRef, "gc.worktree_ref": string(raw)}})
		if err != nil {
			t.Fatal(err)
		}
		got, binding, err := worktreeBindingForBead(store, step, spec.StoreRef)
		if err != nil || got == nil || binding == nil || got.BeadID != owner.ID || got.Owner != spec.Owner {
			t.Fatalf("member %s binding = %+v, %+v, %v", id, got, binding, err)
		}
		if verified, err := verifyReferencedWorktree(store, id, spec.StoreRef, binding, got, worktree.Verify); err != nil || verified.Path != spec.Path {
			t.Fatalf("member %s verification = %+v, %v", id, verified, err)
		}
		encodedBinding, err := json.Marshal(binding)
		if err != nil {
			t.Fatal(err)
		}
		candidate := startCandidate{info: sessionpkg.Info{ID: "session", TriggerBeadID: id, TriggerBeadStoreRef: spec.StoreRef, WorktreeReference: string(encodedBinding), SessionKey: "preserved"}}
		if dir, err := resolvePreparedTaskWorkDir(candidate, "", &config.City{Workspace: config.Workspace{Name: "fixture"}}, store, nil, dispatchOptionSources{}); err != nil || dir != spec.Path {
			t.Fatalf("prepared shared workspace = %q, %v", dir, err)
		}
		for _, change := range []string{"unchanged", "removed", "rebound"} {
			t.Run(id+"/assigned-"+change, func(t *testing.T) {
				status, assignee := "in_progress", "session"
				if err := store.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee, Metadata: map[string]string{
					beadmeta.WorktreeRefMetadataKey: string(raw), beadmeta.LegacyWorkDirMetadataKey: spec.Path,
					beadmeta.RootBeadIDMetadataKey: "workflow-root",
				}}); err != nil {
					t.Fatal(err)
				}
				observed, err := store.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				cfg := &config.City{Workspace: config.Workspace{Name: "fixture"}}
				resolver := newAssignedTaskWorkDirResolver("", []beads.Bead{observed}, []string{spec.StoreRef}, func(string) (beads.Store, error) { return store, nil })
				switch change {
				case "removed":
					replacement := observed
					replacement.Metadata = maps.Clone(observed.Metadata)
					delete(replacement.Metadata, beadmeta.WorktreeRefMetadataKey)
					if err := store.Delete(id); err != nil {
						t.Fatal(err)
					}
					if _, err := store.Create(replacement); err != nil {
						t.Fatal(err)
					}
				case "rebound":
					rebound := maps.Clone(ref)
					rebound["workflow_root_id"] = "replacement-root"
					encoded, err := json.Marshal(rebound)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.SetMetadataBatch(id, map[string]string{beadmeta.WorktreeRefMetadataKey: string(encoded), beadmeta.RootBeadIDMetadataKey: "replacement-root"}); err != nil {
						t.Fatal(err)
					}
					if err := store.SetMetadata(owner.ID, beadmeta.RootBeadIDMetadataKey, "replacement-root"); err != nil {
						t.Fatal(err)
					}
					fresh, err := store.Get(id)
					if err != nil {
						t.Fatal(err)
					}
					if _, present, err := nativeReferenceWorkDir(store, fresh, spec.StoreRef); !present || err != nil {
						t.Fatalf("replacement must be independently valid: present=%v, err=%v", present, err)
					}
				}
				candidate := startCandidate{info: sessionpkg.Info{ID: assignee, SessionKey: "preserved"}, tp: TemplateParams{TemplateName: "worker", SessionName: "worker", Command: "agent"}}
				prepared, returned, err := buildPreparedStartWithWorkDirResolver(candidate, "", cfg, store, resolver, dispatchOptionSources{})
				if change == "unchanged" {
					if err != nil || prepared == nil || prepared.cfg.WorkDir != spec.Path {
						t.Fatalf("unchanged assigned reference: prepared=%v, err=%v", prepared, err)
					}
				} else if err == nil || prepared != nil || returned.SessionKey != "preserved" {
					t.Fatalf("changed reference reached preparation or reset: prepared=%v, key=%q, err=%v", prepared, returned.SessionKey, err)
				}
				if err := store.SetMetadata(owner.ID, beadmeta.RootBeadIDMetadataKey, "workflow-root"); err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadataBatch(id, map[string]string{beadmeta.WorktreeRefMetadataKey: string(raw), beadmeta.RootBeadIDMetadataKey: "workflow-root"}); err != nil {
					t.Fatal(err)
				}
			})
		}
		t.Run(id+"/assigned-new-reference", func(t *testing.T) {
			observed, err := store.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			observed.Metadata = maps.Clone(observed.Metadata)
			delete(observed.Metadata, beadmeta.WorktreeRefMetadataKey)
			observed.Metadata[beadmeta.LegacyWorkDirMetadataKey] = t.TempDir()
			resolver := newAssignedTaskWorkDirResolver("", []beads.Bead{observed}, []string{spec.StoreRef}, func(string) (beads.Store, error) { return store, nil })
			got, err := resolver(startCandidate{info: sessionpkg.Info{ID: "session"}}, &config.City{Workspace: config.Workspace{Name: "fixture"}})
			if err != nil || got != spec.Path {
				t.Fatalf("new managed reference did not replace the legacy snapshot: dir=%q err=%v", got, err)
			}
		})
		for _, changedID := range []string{id, owner.ID} {
			t.Run(id+"/cached-"+changedID, func(t *testing.T) {
				cache := beads.NewCachingStore(store, nil)
				for _, key := range []string{id, owner.ID} {
					if _, err := beads.HandlesFor(cache).Cached.Get(key); err != nil {
						t.Fatal(err)
					}
				}
				cfg := &config.City{Workspace: config.Workspace{Name: "fixture"}}
				selected, err := workLegsFromCensus("", cfg, cityWorkLeg{store: cache}, nil).readOnly().nativeStore(spec.StoreRef)
				if err != nil {
					t.Fatal(err)
				}
				if err := beads.HandlesFor(selected).Writer.SetMetadata(changedID, "unexpected", "write"); err == nil {
					t.Fatal("read-only handles exposed the original writer")
				}
				current, err := store.Get(changedID)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Delete(changedID); err != nil {
					t.Fatal(err)
				}
				if _, err := verifyReferencedWorktree(selected, id, spec.StoreRef, binding, got, worktree.Verify); err == nil {
					t.Fatal("read-only cache authorized a removed step or owner")
				}
				if _, err := store.Create(current); err != nil {
					t.Fatal(err)
				}
			})
		}
		for _, field := range []string{"binding_step_id", "workflow_root_id", "generation", "workspace_owner_id", "workspace_bead_id", "store_ref", "native_attempt_id"} {
			t.Run(id+"/"+field, func(t *testing.T) {
				bad := maps.Clone(ref)
				if field == "generation" {
					bad[field] = 2
				} else {
					bad[field] = "foreign"
				}
				encoded, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadata(id, "gc.worktree_ref", string(encoded)); err != nil {
					t.Fatal(err)
				}
				if _, err := verifyReferencedWorktree(store, id, spec.StoreRef, binding, got, worktree.Verify); err == nil {
					t.Fatalf("changed %s passed cached binding", field)
				}
				if err := store.SetMetadata(id, "gc.worktree_ref", string(raw)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	if got := testutil.RunGit(t, spec.Path, "rev-parse", "HEAD"); got != report.Head {
		t.Fatal("reference verification changed HEAD")
	}
	if got := testutil.RunGit(t, repo, "worktree", "list", "--porcelain"); !strings.Contains(got, spec.Path) {
		t.Fatal("reference verification removed workspace")
	}
}

func TestPreparedNativeReferenceRefusesLegacyDirectory(t *testing.T) {
	for _, raw := range []string{"", "null", "{", `{"version":2}`} {
		t.Run(raw, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			if _, err := store.Create(beads.Bead{ID: "session", Type: sessionpkg.BeadType}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if _, err := store.Create(beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.WorktreeRefMetadataKey: raw, beadmeta.LegacyWorkDirMetadataKey: dir}}); err != nil {
				t.Fatal(err)
			}
			candidate := startCandidate{info: sessionpkg.Info{ID: "session", TriggerBeadID: "step", TriggerBeadStoreRef: "city:fixture", WorkDir: dir, SessionKey: "preserved"}}
			prepared, returned, err := buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{Workspace: config.Workspace{Name: "fixture"}}, store, func(startCandidate, *config.City) (string, error) { return dir, nil }, dispatchOptionSources{})
			if err == nil || !strings.Contains(err.Error(), "reference") || prepared != nil || returned.SessionKey != "preserved" {
				t.Fatalf("invalid native reference reached directory fallback: prepared=%v key=%q err=%v", prepared, returned.SessionKey, err)
			}
		})
	}
}

func TestPreparedLegacyTriggerRefusesUnboundNativeReference(t *testing.T) {
	for _, location := range []string{"session", "trigger", "source"} {
		t.Run(location, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			if _, err := store.Create(beads.Bead{ID: "session", Type: sessionpkg.BeadType}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			step := beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "workflow-root"}}
			source := beads.Bead{ID: "source", Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: "workflow-root", beadmeta.LegacyWorkDirMetadataKey: dir,
			}}
			ref := worktreeReference{
				Version: 1, BindingStepID: "step", WorkflowRootID: "workflow-root", Generation: 1,
				WorkspaceOwnerID: "owner", WorkspaceBeadID: "workspace", StoreRef: "rig:missing", NativeAttemptID: "attempt",
			}
			if location == "source" {
				ref.BindingStepID = source.ID
			}
			raw, err := json.Marshal(ref)
			if err != nil {
				t.Fatal(err)
			}
			candidate := startCandidate{
				info: sessionpkg.Info{ID: "session", TriggerBeadID: step.ID, TriggerBeadStoreRef: ref.StoreRef, WorkDir: dir, SessionKey: "preserved"},
				tp:   TemplateParams{TemplateName: "worker", SessionName: "worker", Command: "agent"},
			}
			switch location {
			case "session":
				candidate.info.WorktreeReference = string(raw)
			case "trigger":
				step.Metadata[beadmeta.WorktreeRefMetadataKey] = string(raw)
				step.Metadata[beadmeta.RootStoreRefMetadataKey] = ref.StoreRef
				if _, err := worktreeReferenceForBead(step, ref.StoreRef); err != nil {
					t.Fatalf("the declared trigger reference must be well formed: %v", err)
				}
			case "source":
				source.Metadata[beadmeta.WorktreeRefMetadataKey] = string(raw)
				source.Metadata[beadmeta.RootStoreRefMetadataKey] = ref.StoreRef
				if _, err := worktreeReferenceForBead(source, ref.StoreRef); err != nil {
					t.Fatalf("the declared source reference must be well formed: %v", err)
				}
			}
			for _, bead := range []beads.Bead{
				step, source, {ID: "workflow-root", Metadata: map[string]string{beadmeta.DrainMemberIDMetadataKey: source.ID}},
			} {
				if _, err := store.Create(bead); err != nil {
					t.Fatal(err)
				}
			}
			prepared, returned, err := buildPreparedStartWithWorkDirResolver(candidate, "", &config.City{Workspace: config.Workspace{Name: "fixture"}}, store,
				func(startCandidate, *config.City) (string, error) { return dir, nil }, dispatchOptionSources{})
			want := "step/root/store mismatch"
			if location == "session" {
				want = "selected store is unavailable"
			}
			if err == nil || !strings.Contains(err.Error(), want) || prepared != nil || returned.SessionKey != "preserved" {
				t.Fatalf("unbound %s reference reached legacy launch: prepared=%v key=%q err=%v", location, prepared, returned.SessionKey, err)
			}
			persisted, err := store.Get("session")
			if err != nil || persisted.Metadata["instance_token"] != "" {
				t.Fatalf("refused native evidence changed the session: row=%+v err=%v", persisted, err)
			}
		})
	}
}

func TestAssignedWorkDirPreservesLegacyAndChecksNativeStore(t *testing.T) {
	for _, shape := range []string{"unknown-legacy", "known-legacy", "unknown-native", "store-unavailable", "store-nil", "fresh-read-failed", "new-invalid-reference"} {
		t.Run(shape, func(t *testing.T) {
			dir := t.TempDir()
			observed := beads.Bead{ID: "step", Status: "in_progress", Assignee: "session", Metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: dir}}
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			var refs []string
			if shape != "unknown-legacy" && shape != "unknown-native" {
				refs = []string{"city:fixture"}
			}
			if shape == "unknown-native" {
				observed.Metadata[beadmeta.WorktreeRefMetadataKey] = "null"
			}
			if shape == "new-invalid-reference" {
				fresh := observed
				fresh.Metadata = maps.Clone(observed.Metadata)
				fresh.Metadata[beadmeta.WorktreeRefMetadataKey] = "null"
				if _, err := store.Create(fresh); err != nil {
					t.Fatal(err)
				}
			}
			if shape == "known-legacy" {
				if _, err := store.Create(observed); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			resolver := newAssignedTaskWorkDirResolver("", []beads.Bead{observed}, refs, func(string) (beads.Store, error) {
				called = true
				if shape == "store-unavailable" {
					return nil, beads.ErrNotFound
				}
				if shape == "store-nil" {
					return nil, nil
				}
				return store, nil
			})
			got, err := resolver(startCandidate{info: sessionpkg.Info{ID: "session"}}, &config.City{Workspace: config.Workspace{Name: "fixture"}})
			if shape == "unknown-legacy" || shape == "known-legacy" {
				if err != nil || got != dir || called != (shape == "known-legacy") {
					t.Fatalf("legacy snapshot changed its store selection: dir=%q called=%v err=%v", got, called, err)
				}
			} else if err == nil || got != "" {
				t.Fatalf("%s fell back to a cached directory: dir=%q err=%v", shape, got, err)
			}
			if shape == "unknown-native" && called {
				t.Fatal("declared native evidence inferred an unrecorded store")
			}
		})
	}
}
