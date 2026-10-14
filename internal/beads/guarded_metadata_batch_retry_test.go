package beads

import (
	"errors"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

// These doubles test retry and error policy at the production batch door.
// They do not prove a backend commit, rollback or unknown-commit readback.
func TestNativeGuardedMetadataRetryReplaysWholeOriginalBatch(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	spy.recorder.apply = func(request issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
		if len(request.Items) != 2 {
			t.Fatalf("retry split the batch: %+v", request.Items)
		}
		for _, item := range request.Items {
			if item.Update == nil || item.Update.ExpectedVersion == nil || *item.Update.ExpectedVersion != -17 {
				t.Fatalf("retry changed the original revision: %+v", item.Update)
			}
		}
		if string(request.Items[0].Update.Patch.Metadata.Set["nonce"]) != `"next"` ||
			string(request.Items[1].Update.Patch.Metadata.Set["payload"]) != `"next"` {
			t.Fatalf("retry changed the original deltas: %+v", request.Items)
		}
		if len(spy.recorder.requests) == 1 {
			return issueops.ApplyBatchResult{}, errors.New(serializationConflictErr)
		}
		return issueops.ApplyBatchResult{Items: []issueops.ItemResult{
			{Kind: issueops.ItemUpdate, IssueID: "gc-guard", Changed: true, RowVersion: 18},
			{Kind: issueops.ItemUpdate, IssueID: "gc-payload", Changed: true, RowVersion: 19},
		}}, nil
	}
	request := GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
		{ID: "gc-payload", ExpectedRevision: "-17", Set: map[string]string{"payload": "next"}},
	}}
	if _, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(request); err != nil {
		t.Fatalf("batch after one serialization conflict: %v", err)
	}
	if len(spy.recorder.requests) != 2 {
		t.Fatalf("attempts = %d, want 2", len(spy.recorder.requests))
	}
}

func TestNativeGuardedMetadataRetryStopsAtExistingAttemptLimit(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
		return issueops.ApplyBatchResult{}, errors.New(serializationConflictErr)
	}
	_, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
	}})
	if !isNativeDoltSerializationConflict(err) || len(spy.recorder.requests) != nativeWriteAttempts {
		t.Fatalf("error/attempts = %v/%d, want serialization refusal after %d", err, len(spy.recorder.requests), nativeWriteAttempts)
	}
}

func TestNativeGuardedMetadataRetryDoesNotRepeatRevisionRefusal(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
		return issueops.ApplyBatchResult{}, issueops.ErrVersionMismatch
	}
	_, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
	}})
	var precondition *PreconditionFailedError
	if err == nil || (!errors.Is(err, issueops.ErrVersionMismatch) && !errors.As(err, &precondition)) || len(spy.recorder.requests) != 1 {
		t.Fatalf("revision refusal error/attempts = %v/%d", err, len(spy.recorder.requests))
	}
}

func TestNativeGuardedMetadataRetryDoesNotAcceptAmbiguousCommit(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	ambiguous := errors.New("connection lost while receiving commit acknowledgement")
	spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
		// A partial result beside an error cannot certify a committed batch.
		return issueops.ApplyBatchResult{Items: []issueops.ItemResult{
			{Kind: issueops.ItemUpdate, IssueID: "gc-guard", Changed: true, RowVersion: 18},
		}}, ambiguous
	}
	rows, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
	}})
	if !errors.Is(err, ambiguous) || len(rows) != 0 || len(spy.recorder.requests) != 1 {
		t.Fatalf("ambiguous result = (%+v, %v), attempts=%d; want unknown, no accepted rows and one attempt", rows, err, len(spy.recorder.requests))
	}
}

func TestNativeGuardedMetadataRetryDoesNotCertifyInvalidCommitResults(t *testing.T) {
	for _, tc := range []struct {
		name string
		item *issueops.ItemResult
	}{
		{"missing row", nil},
		{"different row", &issueops.ItemResult{Kind: issueops.ItemUpdate, IssueID: "gc-other", Changed: true, RowVersion: 18}},
		{"different operation", &issueops.ItemResult{Kind: issueops.ItemClose, IssueID: "gc-guard", Changed: true, RowVersion: 18}},
		{"unchanged guard", &issueops.ItemResult{Kind: issueops.ItemUpdate, IssueID: "gc-guard", Changed: false, RowVersion: -17}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
				result := issueops.ApplyBatchResult{}
				if tc.item != nil {
					result.Items = []issueops.ItemResult{*tc.item}
				}
				return result, nil
			}
			rows, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
				{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
			}})
			if err == nil || len(rows) != 0 || len(spy.recorder.requests) != 1 {
				t.Fatalf("invalid commit result = (%+v, %v), attempts=%d; want no certification and no replay", rows, err, len(spy.recorder.requests))
			}
		})
	}
}

func TestNativeGuardedMetadataTerminalCloseRetryKeepsOriginalIntent(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	status, assignee, reason := "open", "", "retained reason"
	spy.recorder.apply = func(request issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
		if len(request.Items) != 3 {
			t.Fatalf("retry split terminal intent: %+v", request.Items)
		}
		for i := 0; i < 2; i++ {
			update := request.Items[i].Update
			if update == nil || update.ExpectedVersion == nil || *update.ExpectedVersion != -17 || string(update.Patch.Metadata.Set["nonce"]) != `"next"` {
				t.Fatalf("retry refreshed original row %d: %+v", i, update)
			}
		}
		update, closeItem := request.Items[0].Update, request.Items[2].Close
		if update.ExpectedStatus == nil || string(*update.ExpectedStatus) != status || update.ExpectedAssignee == nil || *update.ExpectedAssignee != assignee ||
			closeItem == nil || closeItem.Reason != reason || closeItem.Session != "gc-admitted-session" || closeItem.Force || closeItem.ExpectedVersion != nil {
			t.Fatalf("retry changed close authority: %+v", request)
		}
		if len(spy.recorder.requests) == 1 {
			return issueops.ApplyBatchResult{}, errors.New(serializationConflictErr)
		}
		// A competing authority revision after the first attempt must refuse the
		// whole original intent rather than refreshing it into a new close.
		return issueops.ApplyBatchResult{}, issueops.ErrVersionMismatch
	}
	rows, err := newNativeDoltStoreForTest(spy).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{CloseSession: "gc-admitted-session", Rows: []GuardedMetadataPatch{
		{ID: "gc-source", ExpectedRevision: "-17", ExpectedStatus: &status, ExpectedAssignee: &assignee, RequireChange: true, Set: map[string]string{"nonce": "next"}, CloseReason: &reason},
		{ID: "gc-authority", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
	}})
	if !errors.Is(err, issueops.ErrVersionMismatch) || len(rows) != 0 || len(spy.recorder.requests) != 2 {
		t.Fatalf("terminal retry = (%+v, %v), attempts=%d", rows, err, len(spy.recorder.requests))
	}
}

func TestNativeGuardedMetadataTerminalCloseValidatesCommitResults(t *testing.T) {
	for _, name := range []string{"wrong-close-id", "wrong-close-kind", "incomplete", "wrong-close-issue", "wrong-close-status", "wrong-close-revision", "unchanged-metadata-guard"} {
		t.Run(name, func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
				items := []issueops.ItemResult{
					{Kind: issueops.ItemUpdate, IssueID: "gc-source", RowVersion: 18, Changed: true},
					{
						Kind: issueops.ItemClose, IssueID: "gc-source", RowVersion: 19, Changed: true,
						Issue: &issueops.Issue{ID: "gc-source", RowVersion: 19, Status: issueops.StatusClosed},
					},
				}
				switch name {
				case "wrong-close-id":
					items[1].IssueID = "gc-other"
				case "wrong-close-kind":
					items[1].Kind = issueops.ItemUpdate
				case "incomplete":
					items = items[:1]
				case "wrong-close-issue":
					items[1].Issue.ID = "gc-other"
				case "wrong-close-status":
					items[1].Issue.Status = issueops.StatusOpen
				case "wrong-close-revision":
					items[1].Issue.RowVersion = 20
				case "unchanged-metadata-guard":
					items[0].Changed = false
				}
				return issueops.ApplyBatchResult{Items: items}, nil
			}
			reason := "accepted"
			rows, err := newNativeDoltStoreForTest(spy).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{CloseSession: "gc-session", Rows: []GuardedMetadataPatch{
				{ID: "gc-source", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}, CloseReason: &reason},
			}})
			if err == nil || len(rows) != 0 || len(spy.recorder.requests) != 1 {
				t.Fatalf("invalid terminal result certified or replayed: rows=%+v err=%v calls=%d", rows, err, len(spy.recorder.requests))
			}
		})
	}
}
