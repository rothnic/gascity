package beads

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/steveyegge/beads/issueops"
)

// MetadataSnapshot is an exact native row read with an opaque revision.
// Metadata retains the backend's JSON values without the string projection in Bead.
type MetadataSnapshot struct {
	ID              string          `json:"id"`
	Revision        string          `json:"revision"`
	Status          string          `json:"status"`
	Assignee        string          `json:"assignee"`
	Type            string          `json:"type"`
	Labels          []string        `json:"labels"`
	Metadata        json.RawMessage `json:"metadata"`
	CloseReason     string          `json:"close_reason,omitempty"`
	ClosedBySession string          `json:"closed_by_session,omitempty"`
}

// GuardedMetadataPatch changes keys on one existing physical row.
// ExpectedRevision is equality-only. RequireChange designates a guard whose
// patch must make a real metadata change, including when other rows also change.
type GuardedMetadataPatch struct {
	ID               string            `json:"id"`
	ExpectedRevision string            `json:"expected_revision"`
	ExpectedStatus   *string           `json:"expected_status,omitempty"`
	ExpectedAssignee *string           `json:"expected_assignee,omitempty"`
	RequireChange    bool              `json:"require_change"`
	Set              map[string]string `json:"set,omitempty"`
	Delete           []string          `json:"delete,omitempty"`
	// CloseReason requests a normal native close after the guarded metadata update.
	CloseReason *string `json:"close_reason,omitempty"`
}

// GuardedMetadataBatchRequest is one transaction of unique physical-row patches.
// At least one row must require a change. A request above the native batch cap
// is refused whole, never chunked. This request contains no workflow policy.
type GuardedMetadataBatchRequest struct {
	Rows []GuardedMetadataPatch `json:"rows"`
	// CloseSession is supplied by verified native admission, never by request JSON.
	CloseSession string `json:"-"`
}

// Validate checks close reasons and the complete native item bound before admission.
// Each row costs one update item; each terminal close costs one additional item.
func (r GuardedMetadataBatchRequest) Validate() error {
	if len(r.Rows) == 0 || len(r.Rows) > issueops.MaxApplyBatchItems {
		return fmt.Errorf("guarded metadata batch requires 1..%d rows: %w", issueops.MaxApplyBatchItems, issueops.ErrValidation)
	}
	items := len(r.Rows)
	for _, row := range r.Rows {
		if row.CloseReason != nil {
			items++
			if !utf8.ValidString(*row.CloseReason) || strings.TrimSpace(*row.CloseReason) == "" {
				return fmt.Errorf("guarded metadata batch row %q requires a nonempty UTF-8 close_reason: %w", row.ID, issueops.ErrValidation)
			}
		}
	}
	if items > issueops.MaxApplyBatchItems {
		return fmt.Errorf("guarded metadata batch requires %d native items, at most %d allowed: %w", items, issueops.MaxApplyBatchItems, issueops.ErrValidation)
	}
	return nil
}

// GuardedMetadataRowResult reports one row's result from the committed batch.
type GuardedMetadataRowResult struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Changed  bool   `json:"changed"`
}

// GuardedMetadataBatchStore is the optional native metadata transaction door.
// Snapshots are separate reads, not a multi-row transaction snapshot. Apply
// checks the original revisions in one native transaction and never falls back
// to ordinary writes. Providers without this door cannot emulate its guarantee.
type GuardedMetadataBatchStore interface {
	SnapshotMetadata(ids []string) ([]MetadataSnapshot, error)
	ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest) ([]GuardedMetadataRowResult, error)
}
