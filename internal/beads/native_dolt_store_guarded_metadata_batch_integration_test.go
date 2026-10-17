//go:build integration

package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func TestCachingStoreNativeGuardedMetadataBatchTuple(t *testing.T) {
	scope := initServerScopeForMergeProof(t)
	a := openNativeDoltStoreHandleForMergeProof(t, scope)
	b := openNativeDoltStoreHandleForMergeProof(t, scope)
	root, err := a.Create(Bead{Title: "shared workflow root"})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	cache := NewCachingStoreForTest(a, func(kind, id string, _ json.RawMessage) { events = append(events, kind+":"+id) })
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, ok := GuardedMetadataBatchFor(cache)
	if !ok {
		t.Fatal("native batch capability hidden by cache")
	}
	before, err := batch.SnapshotMetadata([]string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	winner := map[string]string{"gc.session_name": "first-worker", "gc.work_dir": "/first-worker"}
	request := GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{{ID: root.ID, ExpectedRevision: before[0].Revision, RequireChange: true, Set: winner}}}
	if _, err := b.ApplyGuardedMetadataBatch(request); err != nil {
		t.Fatal(err)
	}
	request.Rows[0].Set = map[string]string{"gc.session_name": "loser", "gc.work_dir": "/loser"}
	if _, err := batch.ApplyGuardedMetadataBatch(request); !errors.Is(err, issueops.ErrVersionMismatch) {
		t.Fatalf("stale native root tuple = %v, want version mismatch", err)
	}
	current, err := cache.Get(root.ID)
	if err != nil || current.Metadata["gc.session_name"] != winner["gc.session_name"] || current.Metadata["gc.work_dir"] != winner["gc.work_dir"] || len(events) != 0 {
		t.Fatalf("cache lost native winner or emitted refused write: root=%+v events=%v err=%v", current, events, err)
	}
	fresh, err := batch.SnapshotMetadata([]string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	request.Rows[0].ExpectedRevision = fresh[0].Revision
	request.Rows[0].Set = map[string]string{"test.payload": "next"}
	committed, err := batch.ApplyGuardedMetadataBatch(request)
	if err != nil {
		t.Fatal(err)
	}
	current, err = cache.Get(root.ID)
	if err != nil || len(committed) != 1 || strconv.FormatInt(current.Revision, 10) != committed[0].Revision || current.Metadata["test.payload"] != "next" || current.Metadata["gc.session_name"] != "first-worker" || !reflect.DeepEqual(events, []string{"bead.updated:" + root.ID}) {
		t.Fatalf("cache did not install/emit exact guarded write: root=%+v committed=%+v events=%v err=%v", current, committed, events, err)
	}
	// The handle exposes the existing batch contract, including native close.
	// Retain normal close notification ownership rather than inventing a second
	// event or leaving its close intent live.
	fresh, err = batch.SnapshotMetadata([]string{root.ID})
	if err != nil {
		t.Fatal(err)
	}
	reason := "guarded cache proof complete"
	request.Rows[0].ExpectedRevision = fresh[0].Revision
	request.Rows[0].Set = map[string]string{"test.payload": "closed"}
	request.Rows[0].CloseReason = &reason
	request.CloseSession = "gc-cache-proof-session"
	if _, err := batch.ApplyGuardedMetadataBatch(request); err != nil {
		t.Fatal(err)
	}
	current, err = cache.Get(root.ID)
	if err != nil || current.Status != "closed" || current.CloseReason != reason || !reflect.DeepEqual(events, []string{"bead.updated:" + root.ID, "bead.closed:" + root.ID}) {
		t.Fatalf("cache native terminal forwarding: root=%+v events=%v err=%v", current, events, err)
	}
}

// TestNativeGuardedMetadataBatchStaleWriterAgainstRealDolt owns the real
// transaction boundary: a replacement's committed guard and payload exclude a
// retained old revision, while unrelated typed metadata survives a legal write.
// This is a component store proof, not native session admission or pack proof.
func TestNativeGuardedMetadataBatchStaleWriterAgainstRealDolt(t *testing.T) {
	for _, plane := range []struct {
		name             string
		guardEphemeral   bool
		payloadEphemeral bool
	}{{"issues", false, false}, {"wisps", true, true}, {"mixed-planes", false, true}} {
		t.Run(plane.name, func(t *testing.T) {
			scope := initServerScopeForMergeProof(t)
			a := openNativeDoltStoreHandleForMergeProof(t, scope)
			b := openNativeDoltStoreHandleForMergeProof(t, scope)
			guard, err := a.Create(Bead{Title: "guard", Ephemeral: plane.guardEphemeral, Metadata: map[string]string{"nonce": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := a.Create(Bead{Title: "payload", Ephemeral: plane.payloadEphemeral})
			if err != nil {
				t.Fatal(err)
			}
			storage, release, err := a.acquireStorage()
			if err != nil {
				t.Fatal(err)
			}
			seed := json.RawMessage(`{"payload":"initial","clear":"delete-me","bool":true,"large":9007199254740993123456789,"null":null,"object":{"x":1},"array":[1,false]}`)
			err = storage.UpdateIssue(t.Context(), payload.ID, map[string]interface{}{"metadata": seed}, "guarded-batch-fixture")
			release()
			if err != nil {
				t.Fatal(err)
			}
			ca := requireGuardedMetadataBatchStore(t, a)
			cb := requireGuardedMetadataBatchStore(t, b)
			ids := []string{guard.ID, payload.ID}
			before, err := ca.SnapshotMetadata(ids)
			if err != nil || len(before) != 2 {
				t.Fatalf("initial snapshots = (%+v, %v)", before, err)
			}
			request := GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
				{ID: guard.ID, ExpectedRevision: before[0].Revision, RequireChange: true, Set: map[string]string{"nonce": "B"}},
				{ID: payload.ID, ExpectedRevision: before[1].Revision, Set: map[string]string{"payload": "B"}, Delete: []string{"clear"}},
			}}
			committed, err := cb.ApplyGuardedMetadataBatch(request)
			if err != nil {
				t.Fatalf("legal replacement batch: %v", err)
			}
			accepted, err := cb.SnapshotMetadata(ids)
			if err != nil || len(accepted) != 2 {
				t.Fatalf("accepted snapshots = (%+v, %v)", accepted, err)
			}
			if len(committed) != len(accepted) {
				t.Fatalf("commit returned %d rows, want %d", len(committed), len(accepted))
			}
			for i, row := range committed {
				if row.ID != accepted[i].ID || row.Revision != accepted[i].Revision || !row.Changed {
					t.Fatalf("commit row %d = %+v, fresh native row = %+v", i, row, accepted[i])
				}
			}
			var seedFields, acceptedFields, guardFields map[string]json.RawMessage
			if err := json.Unmarshal(before[1].Metadata, &seedFields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(accepted[1].Metadata, &acceptedFields); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(accepted[0].Metadata, &guardFields); err != nil {
				t.Fatal(err)
			}
			if string(guardFields["nonce"]) != `"B"` || string(acceptedFields["payload"]) != `"B"` {
				t.Fatalf("legal replacement did not publish both rows: %+v", accepted)
			}
			if accepted[0].Revision == before[0].Revision || accepted[1].Revision == before[1].Revision {
				t.Fatalf("legal replacement did not advance both revisions: before=%+v after=%+v", before, accepted)
			}
			if _, exists := acceptedFields["clear"]; exists {
				t.Fatalf("deleted key remains in accepted metadata: %s", accepted[1].Metadata)
			}
			for key, want := range seedFields {
				if key != "payload" && key != "clear" && !reflect.DeepEqual(acceptedFields[key], want) {
					t.Fatalf("typed sibling %q changed: %s -> %s", key, want, acceptedFields[key])
				}
			}
			request.Rows[0].Set["nonce"] = "A-resumed"
			request.Rows[1].Set["payload"] = "A-resumed"
			_, err = ca.ApplyGuardedMetadataBatch(request)
			assertGuardedMetadataRevisionRefusal(t, err)
			after, err := cb.SnapshotMetadata(ids)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(accepted, after) {
				t.Fatalf("stale writer changed accepted rows: before=%+v after=%+v", accepted, after)
			}
		})
	}
}

type guardedMetadataInterleaveStorage struct {
	beadslib.Storage
	beforeApply func()
	loseACK     bool
	applyCalls  int
}

func (s *guardedMetadataInterleaveStorage) BatchApplier() (issueops.BatchApplier, error) {
	applier, err := s.Storage.BatchApplier()
	if err != nil {
		return nil, err
	}
	return guardedMetadataInterleaveApplier{applier: applier, storage: s}, nil
}

type guardedMetadataInterleaveApplier struct {
	applier issueops.BatchApplier
	storage *guardedMetadataInterleaveStorage
}

func (a guardedMetadataInterleaveApplier) ApplyBatch(ctx context.Context, request issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	a.storage.applyCalls++
	if before := a.storage.beforeApply; before != nil {
		a.storage.beforeApply = nil
		before()
	}
	result, err := a.applier.ApplyBatch(ctx, request)
	if err == nil && a.storage.loseACK {
		a.storage.loseACK = false
		return issueops.ApplyBatchResult{}, errors.New("connection lost after native commit")
	}
	return result, err
}

// TestNativeGuardedMetadataBatchLateItemRollsBackAgainstRealDolt forces the
// second item's revision to move after adapter preflight. The native applier
// must refuse that item and roll back the first guard's attempted mutation.
func TestNativeGuardedMetadataBatchLateItemRollsBackAgainstRealDolt(t *testing.T) {
	for _, plane := range []struct {
		name      string
		ephemeral bool
	}{{"issues", false}, {"mixed-planes", true}} {
		t.Run(plane.name, func(t *testing.T) {
			scope := initServerScopeForMergeProof(t)
			a := openNativeDoltStoreHandleForMergeProof(t, scope)
			b := openNativeDoltStoreHandleForMergeProof(t, scope)
			guard, err := a.Create(Bead{Title: "first guard", Metadata: map[string]string{"nonce": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := a.Create(Bead{Title: "late item", Ephemeral: plane.ephemeral, Metadata: map[string]string{"payload": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			ids := []string{guard.ID, payload.ID}
			before, err := requireGuardedMetadataBatchStore(t, a).SnapshotMetadata(ids)
			if err != nil || len(before) != 2 {
				t.Fatalf("initial snapshots = (%+v, %v)", before, err)
			}
			storage, release, err := a.acquireStorage()
			if err != nil {
				t.Fatal(err)
			}
			release()
			interleaved := false
			var winning []MetadataSnapshot
			wrapped := &guardedMetadataInterleaveStorage{Storage: storage, beforeApply: func() {
				interleaved = true
				if err := b.SetMetadata(payload.ID, "payload", "B"); err != nil {
					t.Fatalf("independent late-item mutation: %v", err)
				}
				var err error
				winning, err = b.SnapshotMetadata(ids)
				if err != nil || len(winning) != len(ids) {
					t.Fatalf("winning snapshots = (%+v, %v)", winning, err)
				}
			}}
			caller := newNativeDoltStoreWithStorageAndPrefix(wrapped, "late-item-caller", "gc")
			_, err = requireGuardedMetadataBatchStore(t, caller).ApplyGuardedMetadataBatch(GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
				{ID: guard.ID, ExpectedRevision: before[0].Revision, RequireChange: true, Set: map[string]string{"nonce": "A"}},
				{ID: payload.ID, ExpectedRevision: before[1].Revision, Set: map[string]string{"payload": "A"}},
			}})
			assertGuardedMetadataRevisionRefusal(t, err)
			var itemErr *issueops.ItemError
			if !errors.As(err, &itemErr) || itemErr.Index != 1 || itemErr.IssueID != payload.ID {
				t.Fatalf("late refusal = %v, want the native second item", err)
			}
			if !interleaved {
				t.Fatal("test never reached the native batch boundary")
			}
			after, err := requireGuardedMetadataBatchStore(t, b).SnapshotMetadata(ids)
			if err != nil || len(after) != 2 {
				t.Fatalf("rollback snapshots = (%+v, %v)", after, err)
			}
			if !reflect.DeepEqual(before[0], after[0]) {
				t.Fatalf("first guard escaped rollback: before=%+v after=%+v", before[0], after[0])
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(after[1].Metadata, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["payload"]) != `"B"` {
				t.Fatalf("late payload = %s, want B", fields["payload"])
			}
			if !reflect.DeepEqual(winning, after) {
				t.Fatalf("refused batch changed winning rows: before=%+v after=%+v", winning, after)
			}
		})
	}
}

func assertGuardedMetadataRevisionRefusal(t *testing.T, err error) {
	t.Helper()
	var precondition *PreconditionFailedError
	if err == nil || (!errors.Is(err, issueops.ErrVersionMismatch) && !errors.As(err, &precondition)) {
		t.Fatalf("stale batch error = %v, want a typed revision refusal", err)
	}
}

// The claim is won through NativeDoltStore.Claim on the real issue plane. The
// claimant here is fixture input, not a Gas City session admission receipt.
func TestNativeGuardedMetadataBatchClaimGuardsAgainstRealDolt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantErr   error
		wantApply bool
	}{
		{"matching-claim", nil, true},
		{"authority-noop", issueops.ErrValidation, false},
		{"claim-noop", issueops.ErrValidation, false},
		{"status-mismatch", issueops.ErrStatusMismatch, true},
		{"assignee-mismatch", issueops.ErrAssigneeMismatch, true},
		{"late-same-assignee-reclaim", issueops.ErrVersionMismatch, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := initServerScopeForMergeProof(t)
			a := openNativeDoltStoreHandleForMergeProof(t, scope)
			b := openNativeDoltStoreHandleForMergeProof(t, scope)
			guard, err := a.Create(Bead{Title: "authority guard", Metadata: map[string]string{"nonce": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := a.Create(Bead{Title: "work payload", Ephemeral: true, Metadata: map[string]string{"nonce": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			step, err := a.Create(Bead{Title: "claimed work step", Metadata: map[string]string{"nonce": "initial"}})
			if err != nil {
				t.Fatal(err)
			}
			const claimant = "same-worker"
			claimed, won, err := a.Claim(step.ID, claimant)
			if err != nil || !won || claimed.Status != "in_progress" || claimed.Assignee != claimant {
				t.Fatalf("native claim = (%+v, %v, %v)", claimed, won, err)
			}
			ids := []string{guard.ID, payload.ID, step.ID}
			before, err := a.SnapshotMetadata(ids)
			if err != nil || len(before) != len(ids) {
				t.Fatalf("claimed snapshots = (%+v, %v)", before, err)
			}
			status, assignee := "in_progress", claimant
			request := GuardedMetadataBatchRequest{Rows: []GuardedMetadataPatch{
				{ID: guard.ID, ExpectedRevision: before[0].Revision, RequireChange: true, Set: map[string]string{"nonce": "next"}},
				{ID: payload.ID, ExpectedRevision: before[1].Revision, Set: map[string]string{"nonce": "next"}},
				{ID: step.ID, ExpectedRevision: before[2].Revision, ExpectedStatus: &status, ExpectedAssignee: &assignee, RequireChange: true, Set: map[string]string{"nonce": "next"}},
			}}
			switch tc.name {
			case "authority-noop":
				request.Rows[0].Set["nonce"] = "initial"
			case "claim-noop":
				request.Rows[2].Set["nonce"] = "initial"
			case "status-mismatch":
				status = "open"
			case "assignee-mismatch":
				assignee = "other-worker"
			}
			storage, release, err := a.acquireStorage()
			if err != nil {
				t.Fatal(err)
			}
			release()
			winning, applied := before, false
			wrapped := &guardedMetadataInterleaveStorage{Storage: storage, beforeApply: func() {
				applied = true
				if tc.name != "late-same-assignee-reclaim" {
					return
				}
				open, unassigned := "open", ""
				if err := b.Update(step.ID, UpdateOpts{Status: &open, Assignee: &unassigned}); err != nil {
					t.Fatalf("release native claim: %v", err)
				}
				claimed, won, err := b.Claim(step.ID, claimant)
				if err != nil || !won || claimed.Status != "in_progress" || claimed.Assignee != claimant {
					t.Fatalf("same-assignee native reclaim = (%+v, %v, %v)", claimed, won, err)
				}
				winning, err = b.SnapshotMetadata(ids)
				if err != nil || len(winning) != len(ids) || winning[2].Revision == before[2].Revision {
					t.Fatalf("reclaimed snapshots = (%+v, %v), original=%+v", winning, err, before)
				}
			}}
			caller := newNativeDoltStoreWithStorageAndPrefix(wrapped, "claim-guard-caller", "gc")
			result, err := caller.ApplyGuardedMetadataBatch(request)
			if applied != tc.wantApply {
				t.Fatalf("reached native batch = %v, want %v", applied, tc.wantApply)
			}
			after, readErr := b.SnapshotMetadata(ids)
			if readErr != nil || len(after) != len(ids) {
				t.Fatalf("final snapshots = (%+v, %v)", after, readErr)
			}
			t.Logf("native batch outcome: error=%v returned_rows=%+v", err, result)
			for i, row := range after {
				t.Logf("native row %s: original_revision=%s winning_revision=%s final_revision=%s status=%s assignee=%s metadata=%s", row.ID, before[i].Revision, winning[i].Revision, row.Revision, row.Status, row.Assignee, row.Metadata)
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || len(result) != 0 {
					t.Fatalf("refusal = (%+v, %v), want no accepted rows and %v", result, err, tc.wantErr)
				}
				if tc.wantApply {
					var itemErr *issueops.ItemError
					if !errors.As(err, &itemErr) || itemErr.Index != 2 || itemErr.IssueID != step.ID {
						t.Fatalf("refusal = %v, want the native final claim item", err)
					}
				}
				if !reflect.DeepEqual(winning, after) {
					t.Fatalf("refused batch changed rows: before=%+v after=%+v", winning, after)
				}
				return
			}
			if err != nil || len(result) != len(ids) {
				t.Fatalf("matching claim batch = (%+v, %v)", result, err)
			}
			for i, row := range result {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(after[i].Metadata, &fields); err != nil {
					t.Fatal(err)
				}
				if row.ID != ids[i] || row.Revision != after[i].Revision || !row.Changed || row.Revision == before[i].Revision || string(fields["nonce"]) != `"next"` {
					t.Fatalf("committed row %d = %+v, native snapshot = %+v", i, row, after[i])
				}
			}
			if after[2].Status != "in_progress" || after[2].Assignee != claimant {
				t.Fatalf("metadata batch changed native claim fields: %+v", after[2])
			}
		})
	}
}

// This owns native close policy, transaction rollback and first-class readback.
// Session admission is owned by the command tests; the session here is fixture input.
func TestNativeGuardedMetadataTerminalCloseAgainstRealDolt(t *testing.T) {
	for _, name := range []string{"committed", "wisp-committed", "lost-ack", "blocked", "open-child", "stale-authority", "stale-source", "stale-claim", "status-mismatch", "assignee-mismatch", "readonly"} {
		t.Run(name, func(t *testing.T) {
			scope := initServerScopeForMergeProof(t)
			a := openNativeDoltStoreHandleForMergeProof(t, scope)
			b := openNativeDoltStoreHandleForMergeProof(t, scope)
			authority, err := a.Create(Bead{Title: "source authority", Metadata: StringMap{"nonce": "old"}})
			if err != nil {
				t.Fatal(err)
			}
			source, err := a.Create(Bead{Title: "source to close", Ephemeral: name == "wisp-committed", Metadata: StringMap{"nonce": "old", "close_reason": "metadata is not native proof"}})
			if err != nil {
				t.Fatal(err)
			}
			step, err := a.Create(Bead{Title: "retained native claim", Metadata: StringMap{"nonce": "old"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, won, err := a.Claim(step.ID, "retained-worker"); err != nil || !won {
				t.Fatalf("claim = (%v, %v)", won, err)
			}
			if name == "blocked" || name == "open-child" {
				other, err := a.Create(Bead{Title: "live close policy dependency"})
				if err != nil {
					t.Fatal(err)
				}
				from, to, kind := source.ID, other.ID, "blocks"
				if name == "open-child" {
					from, to, kind = other.ID, source.ID, "parent-child"
				}
				if err := a.DepAdd(from, to, kind); err != nil {
					t.Fatal(err)
				}
			}
			ids := []string{authority.ID, source.ID, step.ID}
			before, err := a.SnapshotMetadata(ids)
			if err != nil || len(before) != len(ids) {
				t.Fatalf("initial rows = (%+v, %v)", before, err)
			}
			sourceStatus, sourceAssignee := before[1].Status, before[1].Assignee
			claimStatus, claimAssignee, reason := "in_progress", "retained-worker", "qualified source and checkpoint"
			request := GuardedMetadataBatchRequest{CloseSession: "gc-admitted-session", Rows: []GuardedMetadataPatch{
				{ID: ids[0], ExpectedRevision: before[0].Revision, RequireChange: true, Set: StringMap{"nonce": "next"}},
				{
					ID: ids[1], ExpectedRevision: before[1].Revision, ExpectedStatus: &sourceStatus, ExpectedAssignee: &sourceAssignee,
					RequireChange: true, Set: StringMap{"nonce": "next", "gc.outcome": "pass"}, CloseReason: &reason,
				},
				{
					ID: ids[2], ExpectedRevision: before[2].Revision, ExpectedStatus: &claimStatus, ExpectedAssignee: &claimAssignee,
					RequireChange: true, Set: StringMap{"nonce": "next"},
				},
			}}
			if name == "status-mismatch" {
				sourceStatus = "in_progress"
			}
			if name == "assignee-mismatch" {
				sourceAssignee = "other-worker"
			}
			storage, release, err := a.acquireStorage()
			if err != nil {
				t.Fatal(err)
			}
			release()
			winning := before
			wrapped := &guardedMetadataInterleaveStorage{Storage: storage, loseACK: name == "lost-ack", beforeApply: func() {
				index := -1
				switch name {
				case "stale-authority":
					index = 0
				case "stale-source":
					index = 1
				case "stale-claim":
					index = 2
				}
				if index < 0 {
					return
				}
				if err := b.SetMetadata(ids[index], "nonce", "replacement"); err != nil {
					t.Fatal(err)
				}
				var err error
				winning, err = b.SnapshotMetadata(ids)
				if err != nil {
					t.Fatal(err)
				}
			}}
			caller := newNativeDoltStoreWithStorageAndPrefix(wrapped, "terminal-close-caller", "gc")
			if name == "readonly" {
				WithProxiedReadOnly()(caller)
			}
			result, err := caller.ApplyGuardedMetadataBatch(request)
			after, readErr := b.SnapshotMetadata(ids)
			if readErr != nil || len(after) != len(ids) {
				t.Fatalf("native readback = (%+v, %v)", after, readErr)
			}
			success := name == "committed" || name == "wisp-committed" || name == "lost-ack"
			if !success {
				if err == nil || len(result) != 0 || !reflect.DeepEqual(winning, after) {
					t.Fatalf("refused close escaped rollback: result=%+v err=%v before=%+v after=%+v", result, err, winning, after)
				}
				want := map[string]error{
					"blocked": issueops.ErrCloseBlocked, "open-child": issueops.ErrCloseOpenChildren,
					"status-mismatch": issueops.ErrStatusMismatch, "assignee-mismatch": issueops.ErrAssigneeMismatch, "readonly": ErrProxiedNativeReadOnly,
				}[name]
				if want != nil && !errors.Is(err, want) {
					t.Fatalf("refusal = %v, want %v", err, want)
				}
				if name == "stale-authority" || name == "stale-source" || name == "stale-claim" {
					assertGuardedMetadataRevisionRefusal(t, err)
				}
				if name == "readonly" && wrapped.applyCalls != 0 {
					t.Fatalf("read-only handle reached native apply %d times", wrapped.applyCalls)
				}
				return
			}
			if name == "lost-ack" {
				if err == nil || len(result) != 0 || wrapped.applyCalls != 1 {
					t.Fatalf("lost ACK certified or replayed: result=%+v err=%v calls=%d", result, err, wrapped.applyCalls)
				}
			} else {
				if err != nil || len(result) != len(ids) {
					t.Fatalf("terminal commit = (%+v, %v)", result, err)
				}
				for i, row := range result {
					if row.ID != after[i].ID || row.Revision != after[i].Revision || !row.Changed {
						t.Fatalf("final commit row = %+v, native row = %+v", row, after[i])
					}
				}
			}
			if after[1].Status != "closed" || after[1].CloseReason != reason || after[1].ClosedBySession != request.CloseSession {
				t.Fatalf("native terminal proof is incomplete: %+v", after[1])
			}
			if after[2].Status != claimStatus || after[2].Assignee != claimAssignee {
				t.Fatalf("close changed the retained claim: %+v", after[2])
			}
			for i, row := range after {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(row.Metadata, &fields); err != nil {
					t.Fatal(err)
				}
				if row.Revision == before[i].Revision || string(fields["nonce"]) != `"next"` {
					t.Fatalf("row did not commit with the source: %+v", row)
				}
				if i == 1 && string(fields["gc.outcome"]) != `"pass"` {
					t.Fatalf("closed source lost its outcome: %s", row.Metadata)
				}
			}
		})
	}
}

func TestNativeGuardedMetadataTerminalCloseNativeCapAgainstRealDolt(t *testing.T) {
	scope := initServerScopeForMergeProof(t)
	store := openNativeDoltStoreHandleForMergeProof(t, scope)
	storage, release, err := store.acquireStorage()
	if err != nil {
		t.Fatal(err)
	}
	applier, err := storage.BatchApplier()
	if err != nil {
		release()
		t.Fatal(err)
	}
	seed := issueops.ApplyBatchRequest{Actor: "terminal-cap-fixture", Items: make([]issueops.ApplyItem, 100)}
	for i := range seed.Items {
		seed.Items[i] = issueops.ApplyItem{Kind: issueops.ItemCreate, Create: &issueops.CreateItem{Issue: &issueops.Issue{
			Title: "terminal cap row", IssueType: beadslib.TypeTask, Status: issueops.StatusOpen, Metadata: json.RawMessage(`{"nonce":"old"}`),
		}}}
	}
	created, err := applier.ApplyBatch(t.Context(), seed)
	release()
	if err != nil || len(created.Items) != 100 {
		t.Fatalf("native cap fixture = (%+v, %v)", created, err)
	}
	ids := make([]string, 100)
	for i, item := range created.Items {
		ids[i] = item.IssueID
	}
	before, err := store.SnapshotMetadata(ids)
	if err != nil {
		t.Fatal(err)
	}
	request := GuardedMetadataBatchRequest{CloseSession: "gc-admitted-session", Rows: make([]GuardedMetadataPatch, 100)}
	for i, row := range before {
		request.Rows[i] = GuardedMetadataPatch{ID: row.ID, ExpectedRevision: row.Revision, RequireChange: true, Set: StringMap{"nonce": "next"}}
	}
	reason := "accepted at native cap"
	request.Rows[0].CloseReason = &reason
	if result, err := store.ApplyGuardedMetadataBatch(request); !errors.Is(err, issueops.ErrValidation) || len(result) != 0 {
		t.Fatalf("100 rows + close = (%+v, %v), want whole refusal", result, err)
	}
	unchanged, err := store.SnapshotMetadata(ids)
	if err != nil || !reflect.DeepEqual(before, unchanged) {
		t.Fatalf("oversized batch changed native rows: err=%v", err)
	}
	request.Rows = request.Rows[:99]
	result, err := store.ApplyGuardedMetadataBatch(request)
	if err != nil || len(result) != 99 {
		t.Fatalf("99 rows + close = (%+v, %v)", result, err)
	}
	after, err := store.SnapshotMetadata(ids)
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range result {
		if row.ID != after[i].ID || row.Revision != after[i].Revision || !row.Changed {
			t.Fatalf("native cap result = %+v, row=%+v", row, after[i])
		}
	}
	if after[0].Status != "closed" || after[0].CloseReason != reason || after[0].ClosedBySession != request.CloseSession || !reflect.DeepEqual(before[99], after[99]) {
		t.Fatalf("native cap terminal result or excluded row changed: first=%+v excluded=%+v", after[0], after[99])
	}
}
