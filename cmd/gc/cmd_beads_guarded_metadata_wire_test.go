package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

const guardedMetadataTestRequest = `{"admission":{"session_id":"gc-session","instance_token":"token-A","claimed_bead_id":"gc-step"},"rows":[{"id":"gc-guard","expected_revision":"-17","require_change":true,"set":{"nonce":"next"}},{"id":"gc-step","expected_revision":"-17","expected_status":"in_progress","expected_assignee":"worker","require_change":true,"set":{"nonce":"next"}}]}`

// This is a command port double, not a transaction or native-claim fixture.
// The native integration tests own the backend transaction assertions.
type guardedMetadataCommandStore struct {
	beads.Store
	snapshots map[string]beads.MetadataSnapshot
	requests  []beads.GuardedMetadataBatchRequest
	applyErr  error
}

func (s *guardedMetadataCommandStore) SnapshotMetadata(ids []string) ([]beads.MetadataSnapshot, error) {
	rows := make([]beads.MetadataSnapshot, 0, len(ids))
	for _, id := range ids {
		row, ok := s.snapshots[id]
		if !ok {
			return nil, fmt.Errorf("%s: %w", id, beads.ErrNotFound)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *guardedMetadataCommandStore) ApplyGuardedMetadataBatch(request beads.GuardedMetadataBatchRequest) ([]beads.GuardedMetadataRowResult, error) {
	s.requests = append(s.requests, request)
	if s.applyErr != nil {
		return nil, s.applyErr
	}
	rows := make([]beads.GuardedMetadataRowResult, len(request.Rows))
	for i, patch := range request.Rows {
		rows[i] = beads.GuardedMetadataRowResult{ID: patch.ID, Revision: "18", Changed: true}
	}
	return rows, nil
}

func guardedMetadataCommandFixture(t *testing.T) *guardedMetadataCommandStore {
	t.Helper()
	return &guardedMetadataCommandStore{
		Store: beads.NewMemStoreFrom(1, []beads.Bead{
			{ID: "gc-session", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: beads.StringMap{
				"state": string(session.StateActive), "alias": "worker", "session_name": "worker", "instance_token": "token-A",
				beadmeta.CurrentClaimBeadIDMetadataKey: "gc-step",
			}},
			{ID: "gc-step", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: beads.StringMap{"nonce": "old"}},
			{ID: "gc-guard", Type: "task", Status: "closed", Metadata: beads.StringMap{"nonce": "old"}},
		}, nil),
		snapshots: map[string]beads.MetadataSnapshot{
			"gc-guard": {ID: "gc-guard", Revision: "-17", Type: "task", Status: "closed", Metadata: json.RawMessage(`{"nonce":"old","large":9007199254740993123456789,"bool":true,"null":null}`)},
			"gc-step":  {ID: "gc-step", Revision: "-17", Type: "task", Status: "in_progress", Assignee: "worker", Metadata: json.RawMessage(`{"nonce":"old"}`)},
		},
	}
}

func setGuardedMetadataCaller(t *testing.T) {
	t.Helper()
	t.Setenv("GC_SESSION_ID", "gc-session")
	t.Setenv("GC_INSTANCE_TOKEN", "token-A")
	t.Setenv("GC_ALIAS", "worker")
}

func runGuardedMetadataCommand(cityPath string, args ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	command := []string{"--city", cityPath, "beads", "guarded-metadata-batch"}
	command = append(command, args...)
	code := run(command, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func writeGuardedMetadataRequest(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBeadsGuardedMetadataBatchSnapshotUsesExactSelectedStoreAndRawWire(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	setGuardedMetadataCaller(t)
	cityPath, rigPath := writeMetadataCASScopedFileCity(t)
	city, rig := guardedMetadataCommandFixture(t), guardedMetadataCommandFixture(t)
	cityRow := city.snapshots["gc-guard"]
	cityRow.Metadata = json.RawMessage(`{"scope":"city"}`)
	city.snapshots["gc-guard"] = cityRow
	rigRow := rig.snapshots["gc-guard"]
	rigRow.Revision = "-9223372036854775808"
	rig.snapshots["gc-guard"] = rigRow
	installMetadataCASStoreSeams(t, func(root, _ string) (beads.Store, error) {
		switch root {
		case cityPath:
			return city, nil
		case rigPath:
			return rig, nil
		default:
			t.Fatalf("opened an unselected scope %q", root)
			return nil, beads.ErrNotFound
		}
	}, func(beads.Store) error { return nil })
	stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=rig:tributary", "--snapshot=gc-guard", "--json")
	if code != 0 || stderr != "" || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("snapshot result code=%d stderr=%q stdout=%q", code, stderr, stdout)
	}
	var result struct {
		SchemaVersion string                   `json:"schema_version"`
		OK            bool                     `json:"ok"`
		StoreRef      string                   `json:"store_ref"`
		Rows          []beads.MetadataSnapshot `json:"rows"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.SchemaVersion != "1" || result.StoreRef != "rig:tributary" || len(result.Rows) != 1 ||
		result.Rows[0].ID != "gc-guard" || result.Rows[0].Revision != "-9223372036854775808" {
		t.Fatalf("snapshot identity/result = %+v", result)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(result.Rows[0].Metadata, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["large"]) != "9007199254740993123456789" || string(fields["bool"]) != "true" || string(fields["null"]) != "null" {
		t.Fatalf("wire projected or replaced typed metadata: %s", result.Rows[0].Metadata)
	}
	if len(city.requests)+len(rig.requests) != 0 {
		t.Fatal("snapshot issued a mutation")
	}
}

func TestBeadsGuardedMetadataBatchMissingSelectedIDDoesNotSearchAnotherStore(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	setGuardedMetadataCaller(t)
	cityPath, rigPath := writeMetadataCASScopedFileCity(t)
	city := guardedMetadataCommandFixture(t)
	delete(city.snapshots, "gc-guard")
	installMetadataCASStoreSeams(t, func(root, _ string) (beads.Store, error) {
		if root == rigPath {
			t.Fatal("missing city ID searched the rig store")
		}
		return city, nil
	}, func(beads.Store) error { return nil })
	stdout, _, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--snapshot=gc-guard", "--json")
	if code == 0 {
		t.Fatalf("missing selected ID succeeded: %s", stdout)
	}
	assertMetadataCASSharedFailureJSON(t, stdout)
}

func TestBeadsGuardedMetadataBatchStrictWireRefusesBeforeOpeningAStore(t *testing.T) {
	for _, tc := range []struct{ name, raw, diagnostic string }{
		{"forged close session", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"close_session":"forged","rows":`, 1), "unknown"},
		{"forged row session", strings.Replace(guardedMetadataTestRequest, `"id":"gc-guard"`, `"session":"forged","id":"gc-guard"`, 1), "unknown"},
		{"null close reason", strings.Replace(guardedMetadataTestRequest, `"require_change":true`, `"close_reason":null,"require_change":true`, 1), "null"},
		{"numeric close reason", strings.Replace(guardedMetadataTestRequest, `"require_change":true`, `"close_reason":42,"require_change":true`, 1), "close_reason"},
		{"blank close reason", strings.Replace(guardedMetadataTestRequest, `"require_change":true`, `"close_reason":" ","require_change":true`, 1), "close_reason"},
		{"duplicate close reason", strings.Replace(guardedMetadataTestRequest, `"require_change":true`, `"close_reason":"one","close_reason":"two","require_change":true`, 1), "duplicate"},
		{"unknown envelope field", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"extra":true,"rows":`, 1), "unknown"},
		{"unknown row field", strings.Replace(guardedMetadataTestRequest, `"require_change":true`, `"extra":true,"require_change":true`, 1), "unknown"},
		{"case alias field", strings.Replace(guardedMetadataTestRequest, `"id":"gc-guard"`, `"ID":"gc-guard"`, 1), "unknown"},
		{"long s envelope alias", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"rowſ":`, 1), "unknown"},
		{"long s admission alias", strings.Replace(guardedMetadataTestRequest, `"session_id":`, `"ſession_id":`, 1), "unknown"},
		{"long s row alias", strings.Replace(guardedMetadataTestRequest, `"set":`, `"ſet":`, 1), "unknown"},
		{"canonical then envelope alias", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"rows":[],"rowſ":`, 1), "unknown"},
		{"envelope alias then canonical", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"rowſ":[],"rows":`, 1), "unknown"},
		{"canonical then row alias", strings.Replace(guardedMetadataTestRequest, `"set":`, `"set":{},"ſet":`, 1), "unknown"},
		{"row alias then canonical", strings.Replace(guardedMetadataTestRequest, `"set":`, `"ſet":{},"set":`, 1), "unknown"},
		{"aliased rows omit guard designation", strings.Replace(strings.Replace(guardedMetadataTestRequest, `"rows":`, `"rowſ":`, 1), `"require_change":true,`, "", 1), "unknown"},
		{"unknown admission field", strings.Replace(guardedMetadataTestRequest, `"session_id":`, `"extra":true,"session_id":`, 1), "unknown"},
		{"numeric revision", strings.Replace(guardedMetadataTestRequest, `"-17"`, `-17`, 1), "expected_revision"},
		{"typed set value", strings.Replace(guardedMetadataTestRequest, `"nonce":"next"`, `"nonce":true`, 1), "set"},
		{"null set value", strings.Replace(guardedMetadataTestRequest, `"nonce":"next"`, `"nonce":null`, 1), "set"},
		{"missing guard designation", strings.Replace(guardedMetadataTestRequest, `"require_change":true,`, "", 1), "require_change"},
		{"duplicate envelope property", strings.Replace(guardedMetadataTestRequest, `"rows":`, `"rows":[],"rows":`, 1), "duplicate"},
		{"duplicate row property", strings.Replace(guardedMetadataTestRequest, `"id":"gc-guard"`, `"id":"other","id":"gc-guard"`, 1), "duplicate"},
		{"escaped duplicate property", strings.Replace(guardedMetadataTestRequest, `"id":"gc-guard"`, `"i\u0064":"other","id":"gc-guard"`, 1), "duplicate"},
		{"trailing JSON", guardedMetadataTestRequest + `{}`, "trailing"},
		{"invalid UTF-8", strings.Replace(guardedMetadataTestRequest, "next", string([]byte{0xff}), 1), "utf"},
		{"missing admission", `{"rows":[{"id":"gc-guard","expected_revision":"-17","require_change":true,"set":{"nonce":"next"}}]}`, "admission"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
				t.Fatal("invalid wire request opened a store")
				return nil, beads.ErrNotFound
			}, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, tc.raw), "--json")
			if code == 0 || !strings.Contains(strings.ToLower(stdout+stderr), tc.diagnostic) {
				t.Fatalf("invalid request code=%d stderr=%q stdout=%q, want %q diagnostic", code, stderr, stdout, tc.diagnostic)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

func TestBeadsGuardedMetadataTerminalCloseWireAndProjectedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, enforce, edits string
		wantOK               bool
	}{
		{"projected no-op", "1", `,"gc.work_outcome":"no-op"`, true},
		{"projected missing outcome", "1", "", false},
		{"projected shipped missing commit", "1", `,"gc.work_outcome":"shipped"`, false},
		{"new kind cannot exempt original task", "1", `,"gc.kind":"workflow"`, false},
		{"new step cannot exempt original task", "1", `,"gc.step_id":"step"`, false},
		{"warn remains warn", "0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			t.Setenv(workRecordEnforceEnvVar, tc.enforce)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			original := string(fixture.snapshots["gc-step"].Metadata)
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
			raw := strings.TrimSuffix(guardedMetadataTestRequest, `"next"}}]}`) + `"next"` + tc.edits + `},"close_reason":"accepted native source"}]}`
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, raw), "--json")
			if (code == 0) != tc.wantOK || (len(fixture.requests) == 1) != tc.wantOK {
				t.Fatalf("projected close policy: code=%d requests=%+v stderr=%q stdout=%q", code, fixture.requests, stderr, stdout)
			}
			if string(fixture.snapshots["gc-step"].Metadata) != original {
				t.Fatal("preflight edited the original snapshot")
			}
			if !tc.wantOK {
				assertMetadataCASSharedFailureJSON(t, stdout)
				return
			}
			request := fixture.requests[0]
			row := request.Rows[1]
			if request.CloseSession != "gc-session" || row.CloseReason == nil || *row.CloseReason != "accepted native source" ||
				row.ExpectedRevision != "-17" || row.ExpectedStatus == nil || *row.ExpectedStatus != "in_progress" ||
				row.ExpectedAssignee == nil || *row.ExpectedAssignee != "worker" || !row.RequireChange {
				t.Fatalf("terminal wire lost admitted identity or original guards: %+v", request)
			}
			if tc.enforce == "0" && !strings.Contains(stderr, "warn-only") {
				t.Fatalf("warning was swallowed: %q", stderr)
			}
		})
	}
}

func TestBeadsGuardedMetadataTerminalCloseCountsBeforeAdmission(t *testing.T) {
	for _, count := range []int{99, 100} {
		t.Run(fmt.Sprintf("rows-%d-plus-close", count), func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			t.Setenv(workRecordEnforceEnvVar, "1")
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			status, assignee, reason := "in_progress", "worker", "accepted"
			request := beadsGuardedMetadataRequest{Admission: beadsGuardedMetadataAdmission{SessionID: "gc-session", InstanceToken: "token-A", ClaimedBeadID: "gc-step"}, Rows: make([]beads.GuardedMetadataPatch, count)}
			request.Rows[0] = beads.GuardedMetadataPatch{
				ID: "gc-step", ExpectedRevision: "-17", ExpectedStatus: &status, ExpectedAssignee: &assignee, RequireChange: true,
				Set: map[string]string{"nonce": "next", "gc.work_outcome": "no-op"}, CloseReason: &reason,
			}
			for i := 1; i < count; i++ {
				id := fmt.Sprintf("gc-cap-%d", i)
				request.Rows[i] = beads.GuardedMetadataPatch{ID: id, ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}}
				fixture.snapshots[id] = beads.MetadataSnapshot{ID: id, Revision: "-17", Type: "task", Status: "closed", Metadata: json.RawMessage(`{"nonce":"old"}`)}
			}
			opened := false
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) {
				opened = true
				if count == 100 {
					t.Fatal("oversized terminal request opened the store before refusal")
				}
				return fixture, nil
			}, func(beads.Store) error { return nil })
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, string(raw)), "--json")
			if count == 100 {
				if code == 0 || opened || len(fixture.requests) != 0 {
					t.Fatalf("oversized intent reached admission: code=%d opened=%v stdout=%q", code, opened, stdout)
				}
				assertMetadataCASSharedFailureJSON(t, stdout)
			} else if code != 0 || len(fixture.requests) != 1 || len(fixture.requests[0].Rows) != 99 {
				t.Fatalf("exact native cap refused: code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
		})
	}
}

func TestBeadsGuardedMetadataSnapshotCloseProofIsOptionalAndNative(t *testing.T) {
	for _, hasProof := range []bool{false, true} {
		t.Run(fmt.Sprintf("native-proof-%t", hasProof), func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			row := fixture.snapshots["gc-guard"]
			row.Metadata = json.RawMessage(`{"close_reason":"forged","closed_by_session":"forged"}`)
			if hasProof {
				row.CloseReason, row.ClosedBySession = "native reason", "gc-native-session"
			}
			fixture.snapshots[row.ID] = row
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--snapshot=gc-guard", "--json")
			var result struct {
				Rows []map[string]json.RawMessage `json:"rows"`
			}
			if code != 0 || stderr != "" {
				t.Fatalf("snapshot code=%d stderr=%q stdout=%q", code, stderr, stdout)
			}
			if err := json.Unmarshal([]byte(stdout), &result); err != nil || len(result.Rows) != 1 {
				t.Fatalf("snapshot wire = %+v, %v", result, err)
			}
			wire := result.Rows[0]
			if hasProof {
				if string(wire["close_reason"]) != `"native reason"` || string(wire["closed_by_session"]) != `"gc-native-session"` {
					t.Fatalf("native close proof projected from metadata: %+v", wire)
				}
			} else if _, found := wire["close_reason"]; found {
				t.Fatalf("absent native reason changed the old wire: %+v", wire)
			} else if _, found := wire["closed_by_session"]; found {
				t.Fatalf("absent native session changed the old wire: %+v", wire)
			}
			if len(fixture.requests) != 0 {
				t.Fatal("snapshot recovery issued a mutation")
			}
		})
	}
}

func TestBeadsGuardedMetadataTerminalCloseEmission(t *testing.T) {
	for _, tc := range []struct {
		name, id, want string
		refuse         bool
	}{
		{"new terminal row", "gc-step", events.BeadClosed, false},
		{"metadata on closed row", "gc-guard", events.BeadUpdated, false},
		{"refused close", "gc-step", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			fixture := guardedMetadataCommandFixture(t)
			if tc.refuse {
				fixture.applyErr = errors.New("native close refused")
			}
			emitter := &emittingClassStore{Store: fixture, cityPath: cityPath}
			reason := "accepted"
			_, err := emitter.ApplyGuardedMetadataBatch(beads.GuardedMetadataBatchRequest{CloseSession: "gc-session", Rows: []beads.GuardedMetadataPatch{
				{ID: tc.id, ExpectedRevision: "-17", RequireChange: true, Set: map[string]string{"nonce": "next"}, CloseReason: &reason},
			}})
			got := beadEvents(readCityJournal(t, cityPath))
			if tc.refuse {
				if err == nil || len(got) != 0 {
					t.Fatalf("refused close emitted: err=%v events=%+v", err, got)
				}
			} else if err != nil || len(got) != 1 || got[0].Type != tc.want || got[0].Subject != tc.id {
				t.Fatalf("terminal event = %+v, err=%v", got, err)
			}
		})
	}
}

func TestBeadsGuardedMetadataBatchStrictWireKeepsUnicodeMetadataKeys(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	setGuardedMetadataCaller(t)
	cityPath := writeMetadataCASTestCity(t)
	fixture := guardedMetadataCommandFixture(t)
	installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error { return nil })
	raw := strings.Replace(guardedMetadataTestRequest, `"nonce":"next"`, `"nonce":"next","ſet":"unicode","KEYÉ":"uppercase"`, 1)
	stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, raw), "--json")
	if code != 0 || len(fixture.requests) != 1 {
		t.Fatalf("valid metadata keys code=%d attempts=%d stderr=%q stdout=%q", code, len(fixture.requests), stderr, stdout)
	}
	fields := fixture.requests[0].Rows[0].Set
	if fields["ſet"] != "unicode" || fields["KEYÉ"] != "uppercase" {
		t.Fatalf("Unicode metadata keys changed: %+v", fields)
	}
}

func TestBeadsGuardedMetadataBatchRefusesRemoteAndAmbiguousCommandShape(t *testing.T) {
	clearGCEnv(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city-url=https://example.invalid", "--city-name=demo", "beads", "guarded-metadata-batch", "--store-ref=city:demo", "--snapshot=gc-guard"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "remote") || !strings.Contains(stderr.String(), "does not support") {
		t.Fatalf("remote result code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	configureIsolatedRuntimeEnv(t)
	cityPath := writeMetadataCASTestCity(t)
	stdoutText, stderrText, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--snapshot=gc-guard", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
	if code == 0 || !strings.Contains(strings.ToLower(stdoutText+stderrText), "exactly one") {
		t.Fatalf("ambiguous command code=%d stderr=%q stdout=%q", code, stderrText, stdoutText)
	}
}

type guardedMetadataUnsupportedCommandStore struct {
	beads.Store
	writes int
}

func (s *guardedMetadataUnsupportedCommandStore) SetMetadata(_, _, _ string) error {
	s.writes++
	return nil
}

func (s *guardedMetadataUnsupportedCommandStore) SetMetadataBatch(_ string, _ map[string]string) error {
	s.writes++
	return nil
}

func (s *guardedMetadataUnsupportedCommandStore) Update(_ string, _ beads.UpdateOpts) error {
	s.writes++
	return nil
}

func TestBeadsGuardedMetadataBatchUnsupportedAndAmbiguousResultsNeverBecomeSuccess(t *testing.T) {
	for _, unsupported := range []bool{true, false} {
		t.Run(fmt.Sprintf("unsupported-%v", unsupported), func(t *testing.T) {
			clearGCEnv(t)
			configureIsolatedRuntimeEnv(t)
			setGuardedMetadataCaller(t)
			cityPath := writeMetadataCASTestCity(t)
			fixture := guardedMetadataCommandFixture(t)
			fixture.applyErr = errors.New("connection lost while receiving commit acknowledgement")
			noCapability := &guardedMetadataUnsupportedCommandStore{Store: fixture.Store}
			var selected beads.Store = fixture
			if unsupported {
				selected = noCapability
			}
			installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return selected, nil }, func(beads.Store) error { return nil })
			stdout, stderr, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
			if code == 0 || noCapability.writes != 0 || len(fixture.requests) > 1 {
				t.Fatalf("failed/unsupported batch code=%d writes=%d native attempts=%d stdout=%q", code, noCapability.writes, len(fixture.requests), stdout)
			}
			if !unsupported && len(fixture.requests) != 1 {
				t.Fatal("ambiguous-control case never reached the batch door")
			}
			if unsupported && !strings.Contains(strings.ToLower(stdout+stderr), "support") && !strings.Contains(strings.ToLower(stdout+stderr), "capability") {
				t.Fatalf("unsupported control refused for the wrong reason: stderr=%q stdout=%q", stderr, stdout)
			}
			assertMetadataCASSharedFailureJSON(t, stdout)
		})
	}
}

func TestBeadsGuardedMetadataBatchCloseFailureDoesNotCertifyAnAcceptedCommit(t *testing.T) {
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	setGuardedMetadataCaller(t)
	cityPath := writeMetadataCASTestCity(t)
	fixture := guardedMetadataCommandFixture(t)
	installMetadataCASStoreSeams(t, func(_, _ string) (beads.Store, error) { return fixture, nil }, func(beads.Store) error {
		return errors.New("store close failed after successful batch")
	})
	stdout, _, code := runGuardedMetadataCommand(cityPath, "--store-ref=city:demo", "--request="+writeGuardedMetadataRequest(t, guardedMetadataTestRequest), "--json")
	if code == 0 || len(fixture.requests) != 1 {
		t.Fatalf("close-failure control code=%d attempts=%d stdout=%q", code, len(fixture.requests), stdout)
	}
	assertMetadataCASSharedFailureJSON(t, stdout)
}
