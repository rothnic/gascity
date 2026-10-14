package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/steveyegge/beads/issueops"
)

var _ GuardedMetadataBatchStore = (*NativeDoltStore)(nil)

// SnapshotMetadata reads exact physical IDs from this store, retaining raw
// metadata and the reader's opaque revision. These are separate row reads.
func (s *NativeDoltStore) SnapshotMetadata(ids []string) ([]MetadataSnapshot, error) {
	if len(ids) == 0 || len(ids) > issueops.MaxApplyBatchItems {
		return nil, fmt.Errorf("metadata snapshot requires 1..%d IDs: %w", issueops.MaxApplyBatchItems, issueops.ErrValidation)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	reader, err := storage.IssueReader()
	if err != nil {
		return nil, err
	}
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	rows := make([]MetadataSnapshot, len(ids))
	for i, id := range ids {
		rows[i], err = readNativeMetadataSnapshot(ctx, reader, id)
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func readNativeMetadataSnapshot(ctx context.Context, reader issueops.Reader, id string) (MetadataSnapshot, error) {
	if id == "" {
		return MetadataSnapshot{}, fmt.Errorf("metadata snapshot requires an exact ID: %w", issueops.ErrValidation)
	}
	row, err := reader.Get(ctx, issueops.GetRequest{ID: id})
	if err != nil {
		return MetadataSnapshot{}, nativeReadNotFound(id, err)
	}
	if row == nil {
		return MetadataSnapshot{}, fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if row.ID != id {
		return MetadataSnapshot{}, fmt.Errorf("metadata snapshot for %q returned a different ID %q", id, row.ID)
	}
	if _, err := strconv.ParseInt(row.Revision, 10, 64); err != nil {
		return MetadataSnapshot{}, fmt.Errorf("metadata snapshot for %q has an invalid revision %q: %w", id, row.Revision, err)
	}
	return MetadataSnapshot{
		ID: id, Revision: row.Revision, Status: string(row.Status), Assignee: row.Assignee,
		Type: string(row.IssueType), Labels: append([]string(nil), row.Labels...),
		Metadata:    append(json.RawMessage(nil), row.Metadata...),
		CloseReason: row.CloseReason, ClosedBySession: row.ClosedBySession,
	}, nil
}

// ApplyGuardedMetadataBatch applies one revision-guarded batch of metadata
// deltas and optional normal closes. Every designated metadata guard must change.
// Requests are never split or sent through ordinary writes. Retries keep the
// original revisions, predicates and close intent.
func (s *NativeDoltStore) ApplyGuardedMetadataBatch(request GuardedMetadataBatchRequest) ([]GuardedMetadataRowResult, error) {
	if err := s.readOnlyGuard(); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	batch := issueops.ApplyBatchRequest{Actor: s.actor, Provenance: "gc: guarded metadata batch", Items: make([]issueops.ApplyItem, len(request.Rows))}
	seen := make(map[string]bool, len(request.Rows))
	guards := make([]bool, len(request.Rows))
	closeItems := make(map[int]int)
	hasGuard := false
	for i, row := range request.Rows {
		if row.ID == "" || seen[row.ID] {
			return nil, fmt.Errorf("guarded metadata batch requires unique exact IDs; got %q: %w", row.ID, issueops.ErrValidation)
		}
		seen[row.ID] = true
		version, err := strconv.ParseInt(row.ExpectedRevision, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("guarded metadata batch row %q has an invalid revision: %w: %w", row.ID, issueops.ErrValidation, err)
		}
		for key := range row.Set {
			if err := beadmeta.ValidateKey(key); err != nil {
				return nil, fmt.Errorf("guarded metadata batch row %q: %w: %w", row.ID, issueops.ErrValidation, err)
			}
		}
		for _, key := range row.Delete {
			if err := beadmeta.ValidateKey(key); err != nil {
				return nil, fmt.Errorf("guarded metadata batch row %q: %w: %w", row.ID, issueops.ErrValidation, err)
			}
			if _, exists := row.Set[key]; exists {
				return nil, fmt.Errorf("guarded metadata batch row %q both sets and deletes %q: %w", row.ID, key, issueops.ErrValidation)
			}
		}
		patch, err := nativeIssuePatchFromUpdateOpts(UpdateOpts{Metadata: row.Set})
		if err != nil {
			return nil, err
		}
		patch.Metadata.Unset = append([]string(nil), row.Delete...)
		update := &issueops.UpdateItem{Target: issueops.Ref{ID: row.ID}, Patch: patch, ExpectedVersion: &version}
		if row.ExpectedStatus != nil {
			status := issueops.Status(*row.ExpectedStatus)
			update.ExpectedStatus = &status
		}
		if row.ExpectedAssignee != nil {
			assignee := *row.ExpectedAssignee
			update.ExpectedAssignee = &assignee
		}
		batch.Items[i] = issueops.ApplyItem{Kind: issueops.ItemUpdate, Update: update}
		if row.CloseReason != nil {
			if strings.TrimSpace(request.CloseSession) == "" {
				return nil, fmt.Errorf("guarded terminal close requires an admitted session: %w", issueops.ErrValidation)
			}
			closeItems[i] = len(batch.Items)
			// Only the update carries the original revision: the native batch
			// forbids a second revision guard on a row already touched in it.
			batch.Items = append(batch.Items, issueops.ApplyItem{Kind: issueops.ItemClose, Close: &issueops.CloseItem{
				Target: issueops.Ref{ID: row.ID}, Reason: *row.CloseReason, Session: request.CloseSession, Force: false,
			}})
		}
		guards[i] = row.RequireChange
		hasGuard = hasGuard || row.RequireChange
	}
	if !hasGuard {
		return nil, fmt.Errorf("guarded metadata batch requires a changing guard: %w", issueops.ErrValidation)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	reader, err := storage.IssueReader()
	if err != nil {
		return nil, err
	}
	applier, err := storage.BatchApplier()
	if err != nil {
		return nil, err
	}
	// Guard effectiveness is checked on the exact read revision. The batch
	// rechecks that original revision inside its transaction, so a competing
	// mutation cannot turn this check into permission for a stale write.
	err = func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		for i, item := range batch.Items[:len(request.Rows)] {
			update := item.Update
			current, err := readNativeMetadataSnapshot(ctx, reader, update.Target.ID)
			if err != nil {
				return err
			}
			if current.Revision != request.Rows[i].ExpectedRevision {
				return fmt.Errorf("guarded metadata batch row %q: %w", current.ID, issueops.ErrVersionMismatch)
			}
			if guards[i] {
				changed, err := nativeMetadataPatchChanges(current.Metadata, update.Patch.Metadata)
				if err != nil {
					return fmt.Errorf("guarded metadata batch row %q: %w", current.ID, err)
				}
				if !changed {
					return fmt.Errorf("guarded metadata batch guard %q would not change: %w", current.ID, issueops.ErrValidation)
				}
			}
			if sessionHooked() {
				keys := make(map[string]string, len(update.Patch.Metadata.Set)+len(update.Patch.Metadata.Unset))
				for key := range update.Patch.Metadata.Set {
					keys[key] = request.Rows[i].Set[key]
				}
				for _, key := range update.Patch.Metadata.Unset {
					keys[key] = ""
				}
				noteSessionKeys(Bead{ID: current.ID, Type: current.Type, Labels: current.Labels}, keys)
			}
		}
		return nil
	}()
	if err != nil {
		return nil, err
	}
	var result issueops.ApplyBatchResult
	err = retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		var applyErr error
		result, applyErr = applier.ApplyBatch(ctx, batch)
		return applyErr
	})
	if err != nil {
		return nil, fmt.Errorf("applying guarded metadata batch: %w", err)
	}
	if len(result.Items) != len(batch.Items) {
		return nil, fmt.Errorf("guarded metadata batch returned an incomplete commit result")
	}
	rows := make([]GuardedMetadataRowResult, len(request.Rows))
	for i, item := range result.Items[:len(request.Rows)] {
		if item.Kind != issueops.ItemUpdate || item.IssueID != request.Rows[i].ID || item.DependsOnID != "" || (guards[i] && !item.Changed) {
			return nil, fmt.Errorf("guarded metadata batch returned an invalid commit result for row %d", i)
		}
		rows[i] = GuardedMetadataRowResult{ID: item.IssueID, Revision: strconv.FormatInt(item.RowVersion, 10), Changed: item.Changed}
		if index, closes := closeItems[i]; closes {
			closed := result.Items[index]
			if closed.Kind != issueops.ItemClose || closed.IssueID != item.IssueID || closed.DependsOnID != "" ||
				(closed.Issue != nil && (closed.Issue.ID != item.IssueID || closed.Issue.Status != issueops.StatusClosed || closed.Issue.RowVersion != closed.RowVersion)) {
				return nil, fmt.Errorf("guarded metadata batch returned an invalid close result for row %d", i)
			}
			rows[i].Revision = strconv.FormatInt(closed.RowVersion, 10)
			rows[i].Changed = item.Changed || closed.Changed
		}
	}
	return rows, nil
}

func nativeMetadataPatchChanges(raw json.RawMessage, patch issueops.MetadataPatch) (bool, error) {
	var current map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return false, fmt.Errorf("decoding metadata: %w", err)
		}
	}
	for key, next := range patch.Set {
		var oldString, nextString *string
		if err := json.Unmarshal(next, &nextString); err != nil {
			return false, err
		}
		old, exists := current[key]
		if !exists || json.Unmarshal(old, &oldString) != nil || oldString == nil || *oldString != *nextString {
			return true, nil
		}
	}
	for _, key := range patch.Unset {
		if _, exists := current[key]; exists {
			return true, nil
		}
	}
	return false, nil
}
