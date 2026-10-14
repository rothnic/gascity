package main

import (
	"encoding/json"
	"errors"
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
