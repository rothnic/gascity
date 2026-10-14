package main

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// These cases seed native session-shaped records to test command admission.
// They do not prove that gc hook --claim or a runtime produced the retained tuple.
func TestBeadsGuardedMetadataBatchRetainedNativeAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *guardedMetadataCommandStore) string
	}{
		{"matching retained tuple", nil},
		{"missing process session", func(t *testing.T, _ *guardedMetadataCommandStore) string {
			t.Setenv("GC_SESSION_ID", "")
			return guardedMetadataTestRequest
		}},
		{"missing process instance", func(t *testing.T, _ *guardedMetadataCommandStore) string {
			t.Setenv("GC_INSTANCE_TOKEN", "")
			return guardedMetadataTestRequest
		}},
		{"process session alias is not exact id", func(t *testing.T, _ *guardedMetadataCommandStore) string {
			t.Setenv("GC_SESSION_ID", "worker")
			return guardedMetadataTestRequest
		}},
		{"retained instance is stale", func(t *testing.T, s *guardedMetadataCommandStore) string {
			if err := s.SetMetadata("gc-session", "instance_token", "token-B"); err != nil {
				t.Fatal(err)
			}
			return guardedMetadataTestRequest
		}},
		{"unreadable authoritative session", func(_ *testing.T, s *guardedMetadataCommandStore) string {
			s.Store = guardedMetadataSessionReadFailureStore{Store: s.Store}
			return guardedMetadataTestRequest
		}},
		{"missing live claim", func(t *testing.T, s *guardedMetadataCommandStore) string {
			if err := s.SetMetadata("gc-session", beadmeta.CurrentClaimBeadIDMetadataKey, ""); err != nil {
				t.Fatal(err)
			}
			return guardedMetadataTestRequest
		}},
		{"live claim cannot replace retained step", func(t *testing.T, s *guardedMetadataCommandStore) string {
			if err := s.SetMetadata("gc-session", beadmeta.CurrentClaimBeadIDMetadataKey, "gc-replacement-step"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_BEAD_ID", "gc-replacement-step")
			return guardedMetadataTestRequest
		}},
		{"caller supplied another step", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"claimed_bead_id":"gc-step"`, `"claimed_bead_id":"gc-other"`, 1)
		}},
		{"claimed step is absent from batch", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `,{"id":"gc-step","expected_revision":"-17","expected_status":"in_progress","expected_assignee":"worker","require_change":true,"set":{"nonce":"next"}}`, "", 1)
		}},
		{"claimed step is not a changing guard", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"expected_assignee":"worker","require_change":true`, `"expected_assignee":"worker","require_change":false`, 1)
		}},
		{"claimed step has no status predicate", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"expected_status":"in_progress",`, "", 1)
		}},
		{"claimed step has no assignee predicate", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"expected_assignee":"worker",`, "", 1)
		}},
		{"claimed step requests another status", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"expected_status":"in_progress"`, `"expected_status":"open"`, 1)
		}},
		{"claimed step requests another assignee", func(_ *testing.T, _ *guardedMetadataCommandStore) string {
			return strings.Replace(guardedMetadataTestRequest, `"expected_assignee":"worker"`, `"expected_assignee":"other-worker"`, 1)
		}},
		{"closed exact session cannot adopt its alias successor", func(t *testing.T, s *guardedMetadataCommandStore) string {
			closed := "closed"
			if err := s.Update("gc-session", beads.UpdateOpts{Status: &closed}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Create(beads.Bead{Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: beads.StringMap{
				"alias": "worker", "session_name": "worker", "state": string(session.StateActive), "instance_token": "token-A",
				beadmeta.CurrentClaimBeadIDMetadataKey: "gc-step",
			}}); err != nil {
				t.Fatal(err)
			}
			return guardedMetadataTestRequest
		}},
		{"draining session", func(t *testing.T, s *guardedMetadataCommandStore) string {
			if err := s.SetMetadata("gc-session", "state", string(session.StateDraining)); err != nil {
				t.Fatal(err)
			}
			return guardedMetadataTestRequest
		}},
		{"claimed step was closed", func(t *testing.T, s *guardedMetadataCommandStore) string {
			closed := "closed"
			if err := s.Update("gc-step", beads.UpdateOpts{Status: &closed}); err != nil {
				t.Fatal(err)
			}
			row := s.snapshots["gc-step"]
			row.Status = closed
			s.snapshots["gc-step"] = row
			return guardedMetadataTestRequest
		}},
		{"claimed step changed assignee", func(t *testing.T, s *guardedMetadataCommandStore) string {
			other := "other-worker"
			if err := s.Update("gc-step", beads.UpdateOpts{Assignee: &other}); err != nil {
				t.Fatal(err)
			}
			row := s.snapshots["gc-step"]
			row.Assignee = other
			s.snapshots["gc-step"] = row
			return guardedMetadataTestRequest
		}},
		{"claimed step names another session", func(t *testing.T, s *guardedMetadataCommandStore) string {
			if err := s.SetMetadata("gc-step", beadmeta.SessionIDMetadataKey, "gc-other-session"); err != nil {
				t.Fatal(err)
			}
			row := s.snapshots["gc-step"]
			row.Metadata = json.RawMessage(`{"nonce":"old","gc.session_id":"gc-other-session"}`)
			s.snapshots["gc-step"] = row
			return guardedMetadataTestRequest
		}},
		{"claimed step metadata is absent", func(_ *testing.T, s *guardedMetadataCommandStore) string {
			row := s.snapshots["gc-step"]
			row.Metadata = nil
			s.snapshots["gc-step"] = row
			return guardedMetadataTestRequest
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			raw := guardedMetadataTestRequest
			if tc.mutate != nil {
				raw = tc.mutate(t, fixture)
			}
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, raw), "--json")
			if tc.mutate != nil {
				if code == 0 || len(fixture.requests) != 0 {
					t.Fatalf("stale/invalid admission reached the batch: code=%d requests=%+v stdout=%q", code, fixture.requests, stdout)
				}
				assertMetadataCASSharedFailureJSON(t, stdout)
				return
			}
			if code != 0 || stderr != "" || len(fixture.requests) != 1 {
				t.Fatalf("matching admission code=%d requests=%+v stderr=%q stdout=%q", code, fixture.requests, stderr, stdout)
			}
			rows := fixture.requests[0].Rows
			if len(rows) != 2 || rows[1].ID != "gc-step" || rows[1].ExpectedRevision != "-17" ||
				rows[1].ExpectedStatus == nil || *rows[1].ExpectedStatus != "in_progress" ||
				rows[1].ExpectedAssignee == nil || *rows[1].ExpectedAssignee != "worker" || !rows[1].RequireChange {
				t.Fatalf("command changed the retained step guards: %+v", rows)
			}
		})
	}
}

func TestBeadsGuardedMetadataBatchReadsTheAuthoritativeSessionClass(t *testing.T) {
	for _, tc := range []struct {
		name, localToken, authoritativeToken string
		readFails, missing, wantAccepted     bool
	}{
		{"binding authorizes despite stale retained copy", "token-B", "token-A", false, false, true},
		{"stale retained copy cannot override binding", "token-A", "token-B", false, false, false},
		{"binding read error cannot fall back", "token-A", "token-A", true, false, false},
		{"missing binding session cannot fall back", "token-A", "token-A", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			selected, binding := guardedMetadataCommandFixture(t), guardedMetadataCommandFixture(t)
			if err := selected.SetMetadata("gc-session", "instance_token", tc.localToken); err != nil {
				t.Fatal(err)
			}
			if err := binding.SetMetadata("gc-session", "instance_token", tc.authoritativeToken); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				binding.Store = beads.NewMemStore()
			}
			if tc.readFails {
				binding.Store = guardedMetadataSessionReadFailureStore{Store: binding.Store}
			}
			// Reuse the existing whole-split route memo seam. It describes opened
			// routes without launching a provider or proving native migration.
			reads := &ownershipCountingStore{Store: binding.Store}
			seedSplitRoutes(t, cityPath, reads)
			installMetadataCASStoreSeams(t, func(root, _ string) (beads.Store, error) {
				if root != cityPath {
					t.Fatalf("batch opened another scope %q", root)
				}
				return selected, nil
			}, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
			if reads.count() == 0 {
				t.Fatalf("command never read the authoritative session binding: code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			if tc.wantAccepted {
				if code != 0 || stderr != "" || len(selected.requests) != 1 {
					t.Fatalf("binding-authorized batch code=%d requests=%+v stderr=%q stdout=%q", code, selected.requests, stderr, stdout)
				}
				return
			}
			if code == 0 || len(selected.requests) != 0 {
				t.Fatalf("retained work-store copy bypassed binding: code=%d requests=%+v stdout=%q", code, selected.requests, stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

func TestBeadsGuardedMetadataBatchForeignClaimedStepRefusesWithoutSearch(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	setGuardedMetadataCaller(t)
	cityPath, rigPath := writeMetadataCASScopedFileCity(t)
	selected, foreign := guardedMetadataCommandFixture(t), guardedMetadataCommandFixture(t)
	delete(selected.snapshots, "gc-step")
	rows, err := selected.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	kept := rows[:0]
	for _, row := range rows {
		if row.ID != "gc-step" {
			kept = append(kept, row)
		}
	}
	selected.Store = beads.NewMemStoreFrom(100, kept, nil)
	installMetadataCASStoreSeams(t, func(root, _ string) (beads.Store, error) {
		if root == rigPath {
			t.Fatal("claimed step outside the selected store triggered a rig search")
			return foreign, nil
		}
		if root != cityPath {
			t.Fatalf("opened an unselected scope %q", root)
		}
		return selected, nil
	}, func(beads.Store) error { return nil })
	stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
	if code == 0 || len(selected.requests)+len(foreign.requests) != 0 || !strings.Contains(stdout+stderr, "gc-step") {
		t.Fatalf("foreign claimed-step control code=%d selected=%+v foreign=%+v stderr=%q stdout=%q", code, selected.requests, foreign.requests, stderr, stdout)
	}
	assertMetadataCASSharedFailureJSON(t, stdout)
}

func TestBeadsGuardedMetadataBatchRigUsesCitySessionAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, cityToken, rigToken                    string
		readFails, missing, movedClaim, wantAccepted bool
	}{
		{"city authorizes despite stale rig copy", "token-A", "token-B", false, false, false, true},
		{"stale rig cannot override city token", "token-B", "token-A", false, false, false, false},
		{"city read error cannot fall back", "token-A", "token-A", true, false, false, false},
		{"missing city session cannot fall back", "token-A", "token-A", false, true, false, false},
		{"city moved claim cannot fall back", "token-A", "token-A", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath, rigPath := writeMetadataCASScopedFileCity(t)
			city, rig := guardedMetadataCommandFixture(t), guardedMetadataCommandFixture(t)
			if err := city.SetMetadata("gc-session", "instance_token", tc.cityToken); err != nil {
				t.Fatal(err)
			}
			if err := rig.SetMetadata("gc-session", "instance_token", tc.rigToken); err != nil {
				t.Fatal(err)
			}
			if tc.movedClaim {
				if err := city.SetMetadata("gc-session", beadmeta.CurrentClaimBeadIDMetadataKey, "gc-successor-step"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing {
				city.Store = beads.NewMemStore()
			}
			if tc.readFails {
				city.Store = guardedMetadataSessionReadFailureStore{Store: city.Store}
			}
			delete(city.snapshots, "gc-step")
			delete(city.snapshots, "gc-guard")
			opened := map[string]int{}
			installMetadataCASStoreSeams(t, func(root, _ string) (beads.Store, error) {
				opened[root]++
				switch root {
				case cityPath:
					return city, nil
				case rigPath:
					return rig, nil
				default:
					t.Fatalf("opened an unrelated store %q", root)
					return nil, beads.ErrNotFound
				}
			}, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=rig:tributary", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
			if len(city.requests) != 0 || opened[cityPath] != 1 || opened[rigPath] != 1 {
				t.Fatalf("wrong authority or batch route: opened=%v city=%+v", opened, city.requests)
			}
			if tc.wantAccepted {
				if code != 0 || stderr != "" || len(rig.requests) != 1 {
					t.Fatalf("city-authorized rig batch code=%d rig=%+v stderr=%q stdout=%q", code, rig.requests, stderr, stdout)
				}
			} else {
				if code == 0 || len(rig.requests) != 0 {
					t.Fatalf("stale city admission reached rig batch: code=%d rig=%+v stdout=%q", code, rig.requests, stdout)
				}
				assertMetadataCASSharedFailureJSON(t, stdout)
			}
		})
	}
}

func TestBeadsGuardedMetadataBatchKeepsControllerOwnedRowsExcluded(t *testing.T) {
	for _, row := range []beads.MetadataSnapshot{
		{ID: "gc-owned", Type: session.BeadType},
		{ID: "gc-owned", Type: " SESSION "},
		{ID: "gc-owned", Type: "task", Labels: []string{session.LabelSession}},
		{ID: "gc-owned", Type: "task", Labels: []string{" GC:SESSION "}},
		{ID: "gc-owned", Type: "startup-health-episode"},
		{ID: "gc-owned", Type: "convergence"},
	} {
		t.Run(row.Type+strings.Join(row.Labels, ","), func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			row.Revision, row.Status, row.Metadata = "-17", "open", json.RawMessage(`{"nonce":"old"}`)
			fixture.snapshots[row.ID] = row
			backing, err := fixture.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
			if err != nil {
				t.Fatal(err)
			}
			backing = append(backing, beads.Bead{ID: row.ID, Type: row.Type, Status: row.Status, Labels: row.Labels, Metadata: beads.StringMap{"nonce": "old"}})
			fixture.Store = beads.NewMemStoreFrom(100, backing, nil)
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
			raw := strings.Replace(guardedMetadataTestRequest, `"id":"gc-guard"`, `"id":"gc-owned"`, 1)
			stdout, _, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, raw), "--json")
			if code == 0 || len(fixture.requests) != 0 {
				t.Fatalf("controller-owned row reached the generic batch: code=%d requests=%+v stdout=%q", code, fixture.requests, stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

func TestBeadsGuardedMetadataBatchCoordinationOwnershipAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, typ, raw, status string
		labels                 []string
		set                    map[string]string
		remove                 []string
		allow                  bool
		readOnly               bool
	}{
		{name: "mail", typ: "message", raw: `{}`},
		{name: "mail case and space", typ: " Message ", raw: `{}`},
		{name: "wait label", typ: "task", labels: []string{"gc:wait"}, raw: `{}`},
		{name: "wait label case and space", typ: "task", labels: []string{" GC:WAIT "}, raw: `{}`},
		{name: "typed wait", typ: "gate", labels: []string{"session:s-1"}, raw: `{"session_id":"s-1"}`},
		{name: "legacy typed wait", typ: " Wait ", labels: []string{" Session:S-1 "}, raw: `{"session_id":" S-1 "}`},
		{name: "nudge shadow", typ: "chore", labels: []string{" GC:NUDGE "}, raw: `{}`},
		{name: "nudge queue", typ: "task", labels: []string{" GC:NUDGE-QUEUE "}, raw: `{}`},
		{name: "order tracking", typ: "task", labels: []string{" ORDER-TRACKING "}, raw: `{}`},
		{name: "order run", typ: "task", labels: []string{"note", " ORDER-RUN:digest "}, raw: `{}`},
		{name: "extmsg binding", typ: "task", labels: []string{" GC:EXTMSG-BINDING "}, raw: `{}`},
		{name: "extmsg transcript", typ: "task", labels: []string{"gc:extmsg-transcript-state"}, raw: `{}`},
		{name: "projected wait", typ: "gate", labels: []string{"session:s-1"}, raw: `{}`, set: map[string]string{"session_id": "s-1"}},
		{name: "projected legacy wait", typ: " WAIT ", labels: []string{" SESSION:S-1 "}, raw: `{"session_id":"other"}`, set: map[string]string{"session_id": " s-1 "}},
		{name: "removing original wait ownership", typ: "gate", labels: []string{"session:s-1"}, raw: `{"session_id":"s-1"}`, remove: []string{"session_id"}},
		{name: "replacing original wait ownership", typ: "wait", labels: []string{"session:s-1"}, raw: `{"session_id":"s-1"}`, set: map[string]string{"session_id": "other"}},
		{name: "read-only coordination guard", typ: "task", labels: []string{"gc:nudge-queue"}, raw: `{}`, readOnly: true},
		{name: "ordinary task", typ: "task", raw: `{"nonce":"old"}`, allow: true},
		{name: "unrelated typed metadata", typ: "task", raw: `{"nonce":"old","large":9007199254740993123456789,"bool":true,"null":null,"object":{"x":1},"array":[1,false]}`, allow: true},
		{name: "empty allocation object", typ: "task", status: "closed", raw: `{}`, allow: true},
		{name: "empty allocation absent metadata", typ: "task", status: "closed", allow: true},
		{name: "unmarked gate", typ: "gate", raw: `{}`, allow: true},
		{name: "gate for another session", typ: "gate", labels: []string{"session:s-2"}, raw: `{"session_id":"s-1"}`, allow: true},
		{name: "projected gate for another session", typ: "gate", labels: []string{"session:s-2"}, raw: `{}`, set: map[string]string{"session_id": "s-1"}, allow: true},
		{name: "task with a session label", typ: "task", labels: []string{"session:s-1"}, raw: `{"session_id":"s-1"}`, allow: true},
		{name: "ordinary wisp", typ: "task", labels: []string{"gc:wisp"}, raw: `{}`, allow: true},
		{name: "label near matches", typ: "task", labels: []string{"gc:waitish", "order-runner:x", "extmsg:binding:conv:v1:x"}, raw: `{}`, allow: true},
	} {
		modes := []string{"metadata", "terminal"}
		if tc.name == "ordinary task" {
			modes = append(modes, "uncertain reply")
		}
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				clearGCEnv(t)
				configureIsolatedRuntimeEnv(t)
				setGuardedMetadataCaller(t)
				cityPath := writeMetadataCASTestCity(t)
				fixture := guardedMetadataCommandFixture(t)
				row := fixture.snapshots["gc-guard"]
				row.Type, row.Labels, row.Metadata, row.Status = tc.typ, tc.labels, json.RawMessage(tc.raw), tc.status
				if len(row.Metadata) == 0 {
					row.Metadata = nil
				}
				if row.Status == "" {
					row.Status = "open"
				}
				fixture.snapshots[row.ID] = row
				beforeRow := row
				beforeRow.Metadata = append(json.RawMessage(nil), row.Metadata...)
				beforeRow.Labels = append([]string(nil), row.Labels...)
				before, err := fixture.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
				if err != nil {
					t.Fatal(err)
				}
				beforeBytes, err := json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
				var request beadsGuardedMetadataRequest
				if err := json.Unmarshal([]byte(guardedMetadataTestRequest), &request); err != nil {
					t.Fatal(err)
				}
				for key, value := range tc.set {
					request.Rows[0].Set[key] = value
				}
				request.Rows[0].Delete = tc.remove
				if tc.readOnly {
					request.Rows[0].RequireChange = false
					request.Rows[0].Set = nil
				}
				if mode != "metadata" {
					reason := "accepted source"
					request.Rows[0].CloseReason = &reason
					if request.Rows[0].Set == nil {
						request.Rows[0].Set = make(map[string]string)
					}
					request.Rows[0].Set[beadmeta.WorkOutcomeMetadataKey] = "no-op"
				}
				if mode == "uncertain reply" {
					fixture.applyErr = errors.New("native commit result is uncertain")
				}
				// A valid changing claim precedes the protected row. Refusal must
				// still reach neither a partial batch nor a terminal close.
				request.Rows[0], request.Rows[1] = request.Rows[1], request.Rows[0]
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
				stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, string(raw)), "--json")
				if !tc.allow {
					if code == 0 || len(fixture.requests) != 0 || !strings.Contains(stdout+stderr, "coordination-owned") {
						t.Fatalf("owned mixed batch reached an effect: code=%d requests=%+v stderr=%q stdout=%q", code, fixture.requests, stderr, stdout)
					}
					assertMetadataCASSharedFailureJSON(t, stdout)
				} else {
					if len(fixture.requests) != 1 || !reflect.DeepEqual(fixture.requests[0].Rows, request.Rows) || fixture.requests[0].CloseSession != "gc-session" {
						t.Fatalf("ordinary batch changed its guards or terminal intent: requests=%+v stderr=%q stdout=%q", fixture.requests, stderr, stdout)
					}
					if mode == "uncertain reply" {
						if code == 0 || !strings.Contains(stdout+stderr, "uncertain") {
							t.Fatalf("uncertain native effect became success: code=%d stderr=%q stdout=%q", code, stderr, stdout)
						}
						assertMetadataCASSharedFailureJSON(t, stdout)
					} else if code != 0 || stderr != "" {
						t.Fatalf("ordinary batch refused: code=%d stderr=%q stdout=%q", code, stderr, stdout)
					}
				}
				after, err := fixture.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
				if err != nil {
					t.Fatal(err)
				}
				afterBytes, err := json.Marshal(after)
				if err != nil {
					t.Fatal(err)
				}
				if string(beforeBytes) != string(afterBytes) || !reflect.DeepEqual(beforeRow, fixture.snapshots[row.ID]) {
					t.Fatal("command bypassed the native batch with an ordinary write or changed its snapshot")
				}
			})
		}
	}
}

func TestBeadsGuardedMetadataBatchMalformedOriginalMetadataRefuses(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `false`, `"metadata"`, `{"session_id":`, `{} {}`,
		`{"session_id":"s-1","session_id":"other"}`, `{"nonce":"old","nonce":"new"}`,
		`{"session_id":null}`, `{"session_id":1}`, `{"session_id":true}`, `{"session_id":[]}`, `{"session_id":{}}`,
		`{"gc.session_id":null}`, `{"gc.session_id":1}`, "{\"session_id\":\"s-\xff\"}",
	} {
		t.Run(raw, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			row := fixture.snapshots["gc-guard"]
			row.Type, row.Labels, row.Metadata = "gate", []string{"session:s-1"}, json.RawMessage(raw)
			fixture.snapshots[row.ID] = row
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
			if code == 0 || len(fixture.requests) != 0 || !strings.Contains(stdout+stderr, "metadata") {
				t.Fatalf("malformed original metadata reached a batch: raw=%q code=%d requests=%+v stderr=%q stdout=%q", raw, code, fixture.requests, stderr, stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

type guardedMetadataSessionReadFailureStore struct{ beads.Store }

type guardedMetadataReadOnlyCommandStore struct{ *guardedMetadataCommandStore }

func (s guardedMetadataReadOnlyCommandStore) ReadOnly() bool { return true }

func TestBeadsGuardedMetadataTerminalCloseKeepsAdmissionAndReadOnlyRefusals(t *testing.T) {
	for _, name := range []string{"stale-session", "controller-owned", "readonly"} {
		t.Run(name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			var store beads.Store = fixture
			switch name {
			case "stale-session":
				if err := fixture.SetMetadata("gc-session", "instance_token", "replacement"); err != nil {
					t.Fatal(err)
				}
			case "controller-owned":
				row := fixture.snapshots["gc-step"]
				row.Type = session.BeadType
				fixture.snapshots["gc-step"] = row
			case "readonly":
				store = wrapStoreWithBeadPolicies(guardedMetadataReadOnlyCommandStore{fixture}, nil)
			}
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return store, nil }, func(beads.Store) error { return nil })
			raw := strings.TrimSuffix(guardedMetadataTestRequest, `"next"}}]}`) + `"next","gc.work_outcome":"no-op"},"close_reason":"accepted"}]}`
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, raw), "--json")
			if code == 0 || len(fixture.requests) != 0 {
				t.Fatalf("terminal intent bypassed %s: code=%d requests=%+v stderr=%q stdout=%q", name, code, fixture.requests, stderr, stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

func (s guardedMetadataSessionReadFailureStore) Get(id string) (beads.Bead, error) {
	if id == "gc-session" {
		return beads.Bead{}, errors.New("authoritative session read failed")
	}
	return s.Store.Get(id)
}
