package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/steveyegge/beads/issueops"
)

// settleRunRootFromStep performs one original-revision write, never a refreshed
// write intent. Only a proven CAS loss may be settled by reading the winner.
// An uncertain commit result remains an error even if a later read looks useful.
func settleRunRootFromStep(cfg *config.City, store beads.Store, step beads.Bead, sessionName, workDir string, allowUnownedWorkDir bool, sessions map[string]sessionAssigneeMatch) error {
	commit, err := prepareRunRootStamp(cfg, store, step, sessionName, workDir, allowUnownedWorkDir, sessions)
	if err != nil || commit == nil {
		return err
	}
	return commit(store)
}

// prepareRunRootStamp retains the original read decision so hook preparation
// cannot spend the write's reserve or refresh its guarded intent.
func prepareRunRootStamp(cfg *config.City, store beads.Store, step beads.Bead, sessionName, workDir string, allowUnownedWorkDir bool, sessions map[string]sessionAssigneeMatch) (func(beads.Store) error, error) {
	rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || rootID == step.ID {
		return nil, nil
	}
	batch, hasBatch := beads.GuardedMetadataBatchFor(store)
	if hasBatch {
		snapshots, err := batch.SnapshotMetadata([]string{rootID})
		if err == nil {
			root, err := runRootMetadataSnapshot(rootID, snapshots)
			if err != nil {
				return nil, err
			}
			patch := runRootStampPatch(cfg, root, sessionName, workDir, allowUnownedWorkDir, sessions)
			if len(patch) == 0 {
				return nil, nil
			}
			snapshot := snapshots[0]
			return func(writeStore beads.Store) error {
				writer, ok := beads.GuardedMetadataBatchFor(writeStore)
				if !ok {
					return beads.ErrConditionalWriteUnsupported
				}
				result, err := writer.ApplyGuardedMetadataBatch(beads.GuardedMetadataBatchRequest{Rows: []beads.GuardedMetadataPatch{{
					ID: rootID, ExpectedRevision: snapshot.Revision, ExpectedStatus: &snapshot.Status,
					ExpectedAssignee: &snapshot.Assignee, RequireChange: true, Set: patch,
				}}})
				if err == nil && (len(result) != 1 || result[0].ID != rootID || !result[0].Changed || result[0].Revision == "") {
					return fmt.Errorf("root %q metadata write returned an unverified result", rootID)
				}
				if errors.Is(err, issueops.ErrVersionMismatch) || errors.Is(err, issueops.ErrStatusMismatch) || errors.Is(err, issueops.ErrAssigneeMismatch) {
					winner, readErr := writer.SnapshotMetadata([]string{rootID})
					if readErr == nil {
						current, decodeErr := runRootMetadataSnapshot(rootID, winner)
						if decodeErr == nil && stableRunRootWinner(root, current) {
							return nil
						}
					}
				}
				return err
			}, nil
		}
		// A capability refusal at the read cannot have committed anything. Mem/
		// File wrappers may expose this optional door while forwarding a native
		// revision-CAS instead. Never take this path after a write attempt.
		if !beads.IsConditionalWriteUnsupported(err) {
			return nil, err
		}
	}
	root, err := store.Get(rootID)
	if err != nil {
		return nil, err
	}
	if root.ID != rootID {
		return nil, fmt.Errorf("root %q resolved to physical row %q", rootID, root.ID)
	}
	patch := runRootStampPatch(cfg, root, sessionName, workDir, allowUnownedWorkDir, sessions)
	if len(patch) == 0 {
		return nil, nil
	}
	return func(writeStore beads.Store) error {
		writer, ok := beads.ConditionalWriterForTarget(writeStore)
		if !ok {
			return beads.ErrConditionalWriteUnsupported
		}
		err := writer.UpdateIfMatch(rootID, root.Revision, beads.UpdateOpts{Metadata: patch})
		if beads.IsPreconditionFailed(err) {
			current, readErr := writeStore.Get(rootID)
			if readErr == nil && stableRunRootWinner(root, current) {
				return nil
			}
		}
		return err
	}, nil
}

func runRootMetadataSnapshot(rootID string, snapshots []beads.MetadataSnapshot) (beads.Bead, error) {
	if len(snapshots) != 1 || snapshots[0].ID != rootID || snapshots[0].Revision == "" {
		return beads.Bead{}, fmt.Errorf("root %q requires one exact physical metadata snapshot", rootID)
	}
	row := snapshots[0]
	var metadata beads.StringMap
	if len(row.Metadata) != 0 {
		if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
			return beads.Bead{}, fmt.Errorf("root %q metadata: %w", rootID, err)
		}
	}
	return beads.Bead{ID: row.ID, Status: row.Status, Assignee: row.Assignee, Metadata: metadata}, nil
}

func stableRunRootWinner(before, current beads.Bead) bool {
	return current.ID == before.ID && current.Status == before.Status && unassignedWorkflowRoot(before) &&
		unassignedWorkflowRoot(current) && strings.TrimSpace(current.Metadata[beadmeta.SessionNameMetadataKey]) != ""
}

// hookPrepareRunRoot is scoped to a natively verified session's hook claim.
// Raw `gc bd --claim` remains an ownership operation without session correlation.
func hookPrepareRunRoot(cfg *config.City, cityPath string, opts hookClaimOptions, route *hookClaimClassRoute) func(context.Context, string, []string, beads.Bead) error {
	return func(ctx context.Context, dir string, env []string, step beads.Bead) error {
		rootID := strings.TrimSpace(step.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" || rootID == step.ID || opts.VerifiedSession == nil || opts.VerifiedSession.InstanceToken == "" ||
			beadmeta.IsControlKind(strings.TrimSpace(step.Metadata[beadmeta.KindMetadataKey])) {
			return nil
		}
		info := *opts.VerifiedSession
		name := sessionBeadIdentifierInfo(info)
		if strings.TrimSpace(info.ID) == "" || name == "" {
			return fmt.Errorf("root correlation requires the verified session's exact identity and name")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var store beads.Store = hookClaimBdStoreContext(ctx, dir, env, opts.Assignee)
		classResident := false
		if route != nil {
			if route.knownResident(rootID) {
				store = route.class
				classResident = true
			} else {
				_, readErr := store.Get(rootID)
				routed, err := route.routes(ctx, hookStore{dir: dir, env: env}, rootID, opts.Assignee, readErr)
				if err != nil {
					return err
				}
				if routed {
					store = route.class
					classResident = true
				} else if readErr != nil {
					return readErr
				}
			}
		}
		cityStore, err := openCityStoreAtWithConfig(cityPath, cfg)
		if err != nil {
			return err
		}
		snapshot, err := loadSessionBeadSnapshotLive(cliSessionStore(cityStore, cfg, cityPath), true)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		commit, err := prepareRunRootStamp(cfg, store, step, name, strings.TrimSpace(info.WorkDir), !isPoolManagedSessionInfo(info), buildSessionAssigneeIndex(snapshot))
		if err != nil || commit == nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(ctx, hookClaimMutationTimeout)
		defer cancel()
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if !classResident {
			store = hookClaimBdStoreContext(writeCtx, dir, env, opts.Assignee)
		}
		if err := commit(store); err != nil {
			return err
		}
		return writeCtx.Err()
	}
}
