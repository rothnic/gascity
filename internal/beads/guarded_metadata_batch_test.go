package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func requireGuardedMetadataBatchStore(t *testing.T, store Store) GuardedMetadataBatchStore {
	t.Helper()
	capability, ok := store.(GuardedMetadataBatchStore)
	if !ok {
		t.Fatalf("%T has no native guarded metadata snapshot/batch door", store)
	}
	return capability
}

func TestNativeGuardedMetadataSnapshotKeepsOpaqueRevisionAndRawJSON(t *testing.T) {
	raw := json.RawMessage(`{"nonce":"old","bool":true,"large":9007199254740993123456789,"null":null,"object":{"x":1},"array":[1,false]}`)
	issue := &beadslib.Issue{
		ID: "gc-exact", Title: "exact snapshot", Status: beadslib.StatusOpen,
		IssueType: beadslib.TypeTask, RowVersion: -9223372036854775808, Metadata: raw,
	}
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			if id != issue.ID {
				t.Fatalf("snapshot read %q, want exact %q", id, issue.ID)
			}
			return cloneNativeIssueForTest(issue), nil
		},
	})
	snapshots, err := requireGuardedMetadataBatchStore(t, store).SnapshotMetadata([]string{issue.ID})
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("SnapshotMetadata = (%+v, %v), want one row", snapshots, err)
	}
	got := snapshots[0]
	if got.ID != issue.ID || got.Revision != "-9223372036854775808" {
		t.Fatalf("identity/revision = %q/%q", got.ID, got.Revision)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got.Metadata, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("raw metadata changed: %s -> %s", raw, got.Metadata)
	}
}

func TestNativeGuardedMetadataSnapshotRefusesAnAliasResolution(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: "gc-successor", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, RowVersion: 19}, nil
		},
	})
	rows, err := requireGuardedMetadataBatchStore(t, store).SnapshotMetadata([]string{"gc-exact"})
	if err == nil || len(rows) != 0 {
		t.Fatalf("exact snapshot accepted an alias resolution: rows=%+v err=%v", rows, err)
	}
}

type guardedMetadataMissingRevisionReader struct{ issueops.Reader }

func (r guardedMetadataMissingRevisionReader) Get(ctx context.Context, request issueops.GetRequest) (*issueops.IssueDetails, error) {
	row, err := r.Reader.Get(ctx, request)
	if err == nil && row != nil {
		row.Revision = ""
	}
	return row, err
}

type guardedMetadataMissingRevisionStorage struct{ *nativeDoltStorageSpy }

func (s guardedMetadataMissingRevisionStorage) IssueReader() (issueops.Reader, error) {
	reader, err := s.nativeDoltStorageSpy.IssueReader()
	return guardedMetadataMissingRevisionReader{Reader: reader}, err
}

func TestNativeGuardedMetadataSnapshotDoesNotInventAnAbsentWireRevision(t *testing.T) {
	spy := &nativeDoltStorageSpy{getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
		return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, RowVersion: 19}, nil
	}}
	store := newNativeDoltStoreForTest(guardedMetadataMissingRevisionStorage{nativeDoltStorageSpy: spy})
	rows, err := requireGuardedMetadataBatchStore(t, store).SnapshotMetadata([]string{"gc-exact"})
	if err == nil || len(rows) != 0 {
		t.Fatalf("snapshot fabricated a revision absent from the reader wire: rows=%+v err=%v", rows, err)
	}
}

type guardedMetadataBatchRecorder struct {
	requests []issueops.ApplyBatchRequest
	apply    func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error)
}

func (r *guardedMetadataBatchRecorder) ApplyBatch(_ context.Context, request issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	r.requests = append(r.requests, request)
	if r.apply != nil {
		return r.apply(request)
	}
	result := issueops.ApplyBatchResult{Items: make([]issueops.ItemResult, len(request.Items))}
	for i, item := range request.Items {
		id := ""
		if item.Update != nil {
			id = item.Update.Target.ID
		} else if item.Close != nil {
			id = item.Close.Target.ID
		}
		result.Items[i] = issueops.ItemResult{Kind: item.Kind, IssueID: id, RowVersion: int64(18 + i), Changed: true}
		if item.Close != nil {
			result.Items[i].Issue = &issueops.Issue{
				ID: id, Status: issueops.StatusClosed, RowVersion: result.Items[i].RowVersion,
				CloseReason: item.Close.Reason, ClosedBySession: item.Close.Session,
			}
		}
	}
	return result, nil
}

type guardedMetadataBatchSpy struct {
	*nativeDoltStorageSpy
	recorder *guardedMetadataBatchRecorder
}

func (s *guardedMetadataBatchSpy) BatchApplier() (issueops.BatchApplier, error) {
	return s.recorder, nil
}

func newGuardedMetadataBatchSpy() *guardedMetadataBatchSpy {
	return &guardedMetadataBatchSpy{
		nativeDoltStorageSpy: &nativeDoltStorageSpy{getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID: id, Title: "batch row", Status: beadslib.StatusOpen,
				IssueType: beadslib.TypeTask, RowVersion: -17, Metadata: json.RawMessage(`{"nonce":"old","clear":"value"}`),
			}, nil
		}},
		recorder: &guardedMetadataBatchRecorder{},
	}
}

func TestNativeGuardedMetadataBatchComposesOneRevisionGuardedRequest(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	capability := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy))
	status, assignee := "open", ""
	request := GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "-17", ExpectedStatus: &status, ExpectedAssignee: &assignee, RequireChange: true, Set: map[string]string{"nonce": "next"}},
		{ID: "gc-payload", ExpectedRevision: "-17", Set: map[string]string{"payload": "accepted"}, Delete: []string{"clear"}},
	}}
	if _, err := capability.ApplyGuardedMetadataBatch(request); err != nil {
		t.Fatal(err)
	}
	if len(spy.recorder.requests) != 1 || len(spy.recorder.requests[0].Items) != 2 {
		t.Fatalf("native calls = %+v, want one two-row batch", spy.recorder.requests)
	}
	for i, item := range spy.recorder.requests[0].Items {
		if item.Kind != issueops.ItemUpdate || item.Update == nil || item.Update.Target.ID != request.Rows[i].ID ||
			item.Update.ExpectedVersion == nil || *item.Update.ExpectedVersion != -17 {
			t.Fatalf("item %d lost exact target/original revision: %+v", i, item)
		}
		if item.Update.Patch.Metadata.Replace.Set || item.Update.ForceAssigneeTransfer || item.Update.ForceClosePolicy {
			t.Fatalf("item %d gained replacement/force behavior: %+v", i, item.Update)
		}
	}
	if string(spy.recorder.requests[0].Items[0].Update.Patch.Metadata.Set["nonce"]) != `"next"` {
		t.Fatal("guard value was not encoded as a JSON string delta")
	}
	guard := spy.recorder.requests[0].Items[0].Update
	if guard.ExpectedStatus == nil || string(*guard.ExpectedStatus) != status || guard.ExpectedAssignee == nil || *guard.ExpectedAssignee != assignee {
		t.Fatalf("explicit status/empty-assignee guards were lost: %+v", guard)
	}
}

func TestNativeGuardedMetadataBatchRefusesBeforeApplying(t *testing.T) {
	valid := GuardedMetadataPatch{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}}
	tooMany := make([]GuardedMetadataPatch, issueops.MaxApplyBatchItems+1)
	for i := range tooMany {
		tooMany[i] = valid
		tooMany[i].ID = fmt.Sprintf("gc-%d", i)
	}
	for _, tc := range []struct {
		name string
		rows []GuardedMetadataPatch
	}{
		{"empty", nil},
		{"no guard", []GuardedMetadataPatch{{ID: "gc-row", ExpectedRevision: "-17", Set: map[string]string{"payload": "next"}}}},
		{"guard no-op", []GuardedMetadataPatch{{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "old"}}, {ID: "gc-payload", ExpectedRevision: "-17", Set: map[string]string{"payload": "next"}}}},
		{"guard absent deletion", []GuardedMetadataPatch{{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Delete: []string{"absent"}}}},
		{"second guard no-op", []GuardedMetadataPatch{valid, {ID: "gc-other-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "old"}}}},
		{"duplicate physical row", []GuardedMetadataPatch{valid, valid}},
		{"over cap", tooMany},
		{"invalid revision", []GuardedMetadataPatch{{ID: "gc-guard", ExpectedRevision: "9223372036854775808", RequireChange: true, Set: map[string]string{"nonce": "next"}}}},
		{"set/delete overlap", []GuardedMetadataPatch{{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}, Delete: []string{"nonce"}}}},
		{"invalid metadata key", []GuardedMetadataPatch{{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"wait_hold=true": "next"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			capability := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy))
			if _, err := capability.ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: tc.rows}); err == nil {
				t.Fatal("invalid/ineffective guarded batch was accepted")
			}
			if len(spy.recorder.requests) != 0 {
				t.Fatalf("refused request called ApplyBatch %d times", len(spy.recorder.requests))
			}
		})
	}
}

func TestNativeGuardedMetadataBatchAcceptsOneGuardAndExactCap(t *testing.T) {
	for _, count := range []int{1, issueops.MaxApplyBatchItems} {
		t.Run(fmt.Sprintf("rows-%d", count), func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			rows := make([]GuardedMetadataPatch, count)
			for i := range rows {
				rows[i] = GuardedMetadataPatch{ID: fmt.Sprintf("gc-%d", i), ExpectedRevision: "-17", Set: map[string]string{"nonce": "next"}, RequireChange: i == 0}
			}
			if _, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: rows}); err != nil {
				t.Fatalf("legal %d-row batch refused: %v", count, err)
			}
			if len(spy.recorder.requests) != 1 || len(spy.recorder.requests[0].Items) != count {
				t.Fatalf("native requests = %+v, want one %d-row batch", spy.recorder.requests, count)
			}
		})
	}
}

func TestNativeGuardedMetadataBatchUsesTypedValuesForGuardChange(t *testing.T) {
	for _, tc := range []struct {
		name, raw, next string
		wantApply       bool
	}{
		{"escaped same string", `{"nonce":"\u006fld"}`, "old", false},
		{"boolean to string", `{"nonce":true}`, "true", true},
		{"null to empty string", `{"nonce":null}`, "", true},
		{"absent to empty string", `{}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			spy.getIssue = func(_ context.Context, id string) (*beadslib.Issue, error) {
				return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, RowVersion: -17, Metadata: json.RawMessage(tc.raw)}, nil
			}
			_, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
				{ID: "gc-guard", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": tc.next}},
			}})
			if (err == nil) != tc.wantApply || (len(spy.recorder.requests) == 1) != tc.wantApply {
				t.Fatalf("typed guard change error=%v calls=%d, wantApply=%v", err, len(spy.recorder.requests), tc.wantApply)
			}
		})
	}
}

func TestNativeGuardedMetadataTerminalCloseKeepsAllGuardsAndFinalRevision(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	status, assignee, reason := "open", "", "accepted source"
	request := GuardedMetadataBatchRequest{CloseSession: "gc-admitted-session", Rows: []GuardedMetadataPatch{
		{
			ID: "gc-source", ExpectedRevision: "-17", ExpectedStatus: &status, ExpectedAssignee: &assignee,
			RequireChange: true, Set: map[string]string{"nonce": "next", "gc.outcome": "pass"}, CloseReason: &reason,
		},
		{ID: "gc-authority", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
	}}
	rows, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(request)
	if err != nil || len(rows) != 2 || len(spy.recorder.requests) != 1 {
		t.Fatalf("terminal batch = (%+v, %v), requests=%+v", rows, err, spy.recorder.requests)
	}
	items := spy.recorder.requests[0].Items
	if len(items) != 3 || items[2].Kind != issueops.ItemClose || items[2].Close == nil {
		t.Fatalf("terminal intent did not share the original batch: %+v", items)
	}
	for i := range request.Rows {
		update := items[i].Update
		if items[i].Kind != issueops.ItemUpdate || update == nil || update.Target.ID != request.Rows[i].ID ||
			update.ExpectedVersion == nil || *update.ExpectedVersion != -17 || update.Patch.Status.Set || update.ForceClosePolicy {
			t.Fatalf("row %d lost its metadata fence: %+v", i, items[i])
		}
	}
	update, closeItem := items[0].Update, items[2].Close
	if update.ExpectedStatus == nil || string(*update.ExpectedStatus) != status || update.ExpectedAssignee == nil || *update.ExpectedAssignee != assignee ||
		closeItem.Target.ID != "gc-source" || closeItem.Reason != reason || closeItem.Session != request.CloseSession || closeItem.Force || closeItem.ExpectedVersion != nil {
		t.Fatalf("terminal batch changed guards or close policy: update=%+v close=%+v", update, closeItem)
	}
	if rows[0].Revision != "20" || !rows[0].Changed || rows[1].Revision != "19" {
		t.Fatalf("row results do not report the final close revision: %+v", rows)
	}
}

func TestNativeGuardedMetadataTerminalCloseDoesNotReplaceMetadataNonce(t *testing.T) {
	for _, tc := range []struct{ name, reason, session, nonce string }{
		{"unchanged nonce", "accepted", "gc-session", "old"},
		{"empty reason", "", "gc-session", "next"},
		{"blank reason", " \t\n", "gc-session", "next"},
		{"missing admitted session", "accepted", "", "next"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			_, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{
				CloseSession: tc.session, Rows: []GuardedMetadataPatch{{
					ID: "gc-source", ExpectedRevision: "-17", RequireChange: true,
					Set: map[string]string{"nonce": tc.nonce}, CloseReason: &tc.reason,
				}},
			})
			if err == nil || len(spy.recorder.requests) != 0 {
				t.Fatalf("invalid terminal intent reached native mutation: err=%v requests=%+v", err, spy.recorder.requests)
			}
		})
	}
}

func TestNativeGuardedMetadataTerminalCloseCountsAllNativeItems(t *testing.T) {
	for _, count := range []int{99, 100} {
		t.Run(fmt.Sprintf("rows-%d-plus-close", count), func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			rows := make([]GuardedMetadataPatch, count)
			for i := range rows {
				rows[i] = GuardedMetadataPatch{ID: fmt.Sprintf("gc-%d", i), ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}}
			}
			reason := "accepted"
			rows[0].CloseReason = &reason
			result, err := requireGuardedMetadataBatchStore(t, newNativeDoltStoreForTest(spy)).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: rows, CloseSession: "gc-session"})
			if count == 100 {
				if err == nil || len(result) != 0 || len(spy.recorder.requests) != 0 {
					t.Fatalf("oversized terminal batch was not refused whole: result=%+v err=%v calls=%d", result, err, len(spy.recorder.requests))
				}
			} else if err != nil || len(result) != count || len(spy.recorder.requests) != 1 || len(spy.recorder.requests[0].Items) != 100 {
				t.Fatalf("exact native cap = (%+v, %v), calls=%+v", result, err, spy.recorder.requests)
			}
		})
	}
}

func TestNativeGuardedMetadataSnapshotUsesFirstClassClosureFields(t *testing.T) {
	spy := newGuardedMetadataBatchSpy()
	spy.getIssue = func(_ context.Context, id string) (*beadslib.Issue, error) {
		return &beadslib.Issue{
			ID: id, Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, RowVersion: 18,
			CloseReason: "native reason", ClosedBySession: "gc-native-session", Metadata: json.RawMessage(`{"close_reason":"forged","closed_by_session":"forged"}`),
		}, nil
	}
	rows, err := newNativeDoltStoreForTest(spy).SnapshotMetadata([]string{"gc-source"})
	if err != nil || len(rows) != 1 || rows[0].CloseReason != "native reason" || rows[0].ClosedBySession != "gc-native-session" {
		t.Fatalf("snapshot lost native closure attribution: rows=%+v err=%v", rows, err)
	}
}

func TestNativeGuardedMetadataTerminalCloseAggregatesUpdateAndCloseChanges(t *testing.T) {
	for _, metadataChanged := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadata-changed-%t", metadataChanged), func(t *testing.T) {
			spy := newGuardedMetadataBatchSpy()
			spy.recorder.apply = func(issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
				return issueops.ApplyBatchResult{Items: []issueops.ItemResult{
					{Kind: issueops.ItemUpdate, IssueID: "gc-source", RowVersion: 18, Changed: metadataChanged},
					{Kind: issueops.ItemUpdate, IssueID: "gc-authority", RowVersion: 19, Changed: true},
					{Kind: issueops.ItemClose, IssueID: "gc-source", RowVersion: 20, Changed: !metadataChanged},
				}}, nil
			}
			reason := "accepted"
			rows, err := newNativeDoltStoreForTest(spy).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{CloseSession: "gc-session", Rows: []GuardedMetadataPatch{
				{ID: "gc-source", ExpectedRevision: "-17", Set: map[string]string{"nonce": "next"}, CloseReason: &reason},
				{ID: "gc-authority", ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}},
			}})
			if err != nil || len(rows) != 2 || !rows[0].Changed || rows[0].Revision != "20" {
				t.Fatalf("combined change/final revision = (%+v, %v)", rows, err)
			}
		})
	}
}
