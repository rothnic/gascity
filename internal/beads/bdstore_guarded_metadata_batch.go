package beads

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/steveyegge/beads/issueops"
)

const (
	bdGuardedBatchCapability   = "issues.batchApply.cli.json.v1"
	bdExactStoreReadCapability = "issues.getExact.cli.json.v1"
)

var _ GuardedMetadataBatchStore = (*BdStore)(nil)

// SnapshotMetadata uses the native reader's raw metadata and opaque revisions.
// The store-free probe requires the exact-store reader and JSON batch grammar.
// An ordinary show can route a locally missing ID into a foreign store.
func (s *BdStore) SnapshotMetadata(ids []string) ([]MetadataSnapshot, error) {
	if len(ids) == 0 || len(ids) > issueops.MaxApplyBatchItems {
		return nil, fmt.Errorf("metadata snapshot requires 1..%d IDs: %w", issueops.MaxApplyBatchItems, issueops.ErrValidation)
	}
	for _, id := range ids {
		if id == "" {
			return nil, fmt.Errorf("metadata snapshot requires an exact ID: %w", issueops.ErrValidation)
		}
	}
	version, err := s.runner(s.dir, "bd", "version", "--json")
	if err != nil {
		return nil, fmt.Errorf("probing native exact-read and guarded batch capabilities: %w", err)
	}
	var capability struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(bdGuardedJSONBody(version), &capability); err != nil {
		return nil, fmt.Errorf("parsing native exact-read and guarded batch capabilities: %w", err)
	}
	batchCapable, readCapable := false, false
	for _, name := range capability.Capabilities {
		batchCapable = batchCapable || name == bdGuardedBatchCapability
		readCapable = readCapable || name == bdExactStoreReadCapability
	}
	if !batchCapable || !readCapable {
		return nil, ErrConditionalWriteUnsupported
	}
	args := append([]string{"show", "--exact-store", "--json", "--"}, ids...)
	out, err := s.runner(s.dir, "bd", args...)
	if err != nil {
		return nil, fmt.Errorf("reading native metadata snapshots: %w", err)
	}
	var details []issueops.IssueDetails
	if err := json.Unmarshal(bdGuardedJSONBody(out), &details); err != nil {
		return nil, fmt.Errorf("parsing native metadata snapshots: %w", err)
	}
	if len(details) != len(ids) {
		return nil, fmt.Errorf("native metadata snapshot returned %d rows for %d exact IDs", len(details), len(ids))
	}
	rows := make([]MetadataSnapshot, len(ids))
	for i, id := range ids {
		rows[i], err = metadataSnapshotFromDetails(id, &details[i])
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// ApplyGuardedMetadataBatch delegates one complete native transaction. An error
// or missing acknowledgment is returned without retry or ordinary-write fallback.
func (s *BdStore) ApplyGuardedMetadataBatch(request GuardedMetadataBatchRequest) ([]GuardedMetadataRowResult, error) {
	plan, err := prepareGuardedMetadataBatch(request, nativeDoltStoreActor)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(request.Rows))
	for i, row := range request.Rows {
		ids[i] = row.ID
	}
	rows, err := s.SnapshotMetadata(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]MetadataSnapshot, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	return applyGuardedMetadataBatchPlan(request, plan,
		func(id string) (MetadataSnapshot, error) { return byID[id], nil }, s.applyNativeGuardedBatch)
}

// These wire structs spell only the update/normal-close subset emitted by the
// guarded planner. Actor remains a CLI flag, outside the request document.
type bdGuardedMetadataRef struct {
	ID string `json:"id"`
}

type bdGuardedMetadataEdits struct {
	Set   map[string]json.RawMessage `json:"set,omitempty"`
	Unset []string                   `json:"unset,omitempty"`
}

type bdGuardedMetadataUpdate struct {
	Target           bdGuardedMetadataRef `json:"target"`
	ExpectedVersion  string               `json:"expected_version"`
	ExpectedStatus   *issueops.Status     `json:"expected_status,omitempty"`
	ExpectedAssignee *string              `json:"expected_assignee,omitempty"`
	Patch            struct {
		Metadata bdGuardedMetadataEdits `json:"metadata"`
	} `json:"patch"`
}

type bdGuardedMetadataClose struct {
	Target  bdGuardedMetadataRef `json:"target"`
	Reason  string               `json:"reason"`
	Session string               `json:"session"`
	Force   bool                 `json:"force"`
}

type bdGuardedMetadataItem struct {
	Kind   issueops.ItemKind        `json:"kind"`
	Update *bdGuardedMetadataUpdate `json:"update,omitempty"`
	Close  *bdGuardedMetadataClose  `json:"close,omitempty"`
}

type bdGuardedMetadataWireRequest struct {
	SchemaVersion string `json:"schema_version"`
	Request       struct {
		Items      []bdGuardedMetadataItem `json:"items"`
		Provenance string                  `json:"provenance"`
	} `json:"request"`
}

func (s *BdStore) applyNativeGuardedBatch(batch issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	var request bdGuardedMetadataWireRequest
	request.SchemaVersion = "1"
	request.Request.Provenance = batch.Provenance
	request.Request.Items = make([]bdGuardedMetadataItem, len(batch.Items))
	for i, item := range batch.Items {
		wire := bdGuardedMetadataItem{Kind: item.Kind}
		switch item.Kind {
		case issueops.ItemUpdate:
			u := item.Update
			wire.Update = &bdGuardedMetadataUpdate{
				Target:          bdGuardedMetadataRef{ID: u.Target.ID},
				ExpectedVersion: strconv.FormatInt(*u.ExpectedVersion, 10),
				ExpectedStatus:  u.ExpectedStatus, ExpectedAssignee: u.ExpectedAssignee,
			}
			wire.Update.Patch.Metadata = bdGuardedMetadataEdits{Set: u.Patch.Metadata.Set, Unset: u.Patch.Metadata.Unset}
		case issueops.ItemClose:
			c := item.Close
			wire.Close = &bdGuardedMetadataClose{Target: bdGuardedMetadataRef{ID: c.Target.ID}, Reason: c.Reason, Session: c.Session}
		default:
			return issueops.ApplyBatchResult{}, fmt.Errorf("unsupported guarded native item %q", item.Kind)
		}
		request.Request.Items[i] = wire
	}
	wire, err := json.Marshal(request)
	if err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	file, err := os.CreateTemp("", "gc-guarded-batch-*.json")
	if err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	defer os.Remove(file.Name()) //nolint:errcheck // private, bounded request file
	if _, err := file.Write(wire); err != nil {
		_ = file.Close()
		return issueops.ApplyBatchResult{}, err
	}
	if err := file.Close(); err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	out, err := s.runner(s.dir, "bd", "--actor", batch.Actor, "batch", "--input-format=json", "--file", file.Name(), "--json")
	if err != nil {
		if body, ok := parseBdConditionalErrorBody(out, err); ok {
			var refusal error
			switch body.Code {
			case "version_mismatch":
				refusal = issueops.ErrVersionMismatch
			case "status_mismatch":
				refusal = issueops.ErrStatusMismatch
			case "assignee_mismatch":
				refusal = issueops.ErrAssigneeMismatch
			case "validation_failed":
				refusal = issueops.ErrValidation
			case "close_blocked":
				refusal = issueops.ErrCloseBlocked
			}
			if refusal != nil {
				return issueops.ApplyBatchResult{}, fmt.Errorf("%w: %w", refusal, err)
			}
		}
		return issueops.ApplyBatchResult{}, err
	}
	var schema struct {
		Version *int `json:"schema_version"`
	}
	if err := json.Unmarshal(out, &schema); err != nil {
		return issueops.ApplyBatchResult{}, fmt.Errorf("parsing native guarded batch acknowledgment (commit outcome unknown): %w", err)
	}
	if schema.Version == nil || *schema.Version != 1 {
		return issueops.ApplyBatchResult{}, fmt.Errorf("unsupported native guarded batch acknowledgment schema (commit outcome unknown)")
	}
	var response struct {
		Keys  map[string]string `json:"keys"`
		Items []struct {
			Kind        *issueops.ItemKind `json:"kind"`
			IssueID     *string            `json:"issue_id"`
			Changed     *bool              `json:"changed"`
			Revision    *string            `json:"revision"`
			DependsOnID string             `json:"depends_on_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(bdGuardedJSONBody(out), &response); err != nil {
		return issueops.ApplyBatchResult{}, fmt.Errorf("parsing native guarded batch acknowledgment (commit outcome unknown): %w", err)
	}
	if response.Keys == nil || len(response.Keys) != 0 || len(response.Items) != len(batch.Items) {
		return issueops.ApplyBatchResult{}, fmt.Errorf("incomplete native guarded batch acknowledgment (commit outcome unknown)")
	}
	result := issueops.ApplyBatchResult{Keys: response.Keys, Items: make([]issueops.ItemResult, len(response.Items))}
	for i, item := range response.Items {
		if item.Kind == nil || item.IssueID == nil || item.Changed == nil || item.Revision == nil {
			return issueops.ApplyBatchResult{}, fmt.Errorf("incomplete native guarded batch acknowledgment item %d (commit outcome unknown)", i)
		}
		revision, err := strconv.ParseInt(*item.Revision, 10, 64)
		if err != nil {
			return issueops.ApplyBatchResult{}, fmt.Errorf("invalid native guarded batch acknowledgment revision (commit outcome unknown): %w", err)
		}
		result.Items[i] = issueops.ItemResult{Kind: *item.Kind, IssueID: *item.IssueID, Changed: *item.Changed, RowVersion: revision, DependsOnID: item.DependsOnID}
	}
	return result, nil
}

func bdGuardedJSONBody(raw []byte) []byte {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Data) != 0 {
		return envelope.Data
	}
	return raw
}
