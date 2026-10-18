package beads

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

const (
	guardedVersionJSON = `{"capabilities":["issues.batchApply.cli.json.v1","issues.getExact.cli.json.v1"]}`
	guardedRowsJSON    = `[{"id":"gc-guard","revision":"9223372036854775807","status":"in_progress","assignee":"worker","issue_type":"task","metadata":{"nonce":"old","large":9007199254740993123456789,"bool":true,"object":{"x":1},"array":[1,false],"null":null}},{"id":"gc-payload","revision":"19","status":"open","issue_type":"task","metadata":{"payload":"old"}}]`
)

func guardedBDRequest() GuardedMetadataBatchRequest {
	status, assignee, reason := "in_progress", "worker", "finished"
	return GuardedMetadataBatchRequest{CloseSession: "admitted-session", Rows: []GuardedMetadataPatch{
		{ID: "gc-guard", ExpectedRevision: "9223372036854775807", ExpectedStatus: &status, ExpectedAssignee: &assignee, RequireChange: true, Set: map[string]string{"nonce": "new"}},
		{ID: "gc-payload", ExpectedRevision: "19", Set: map[string]string{"payload": "new"}, CloseReason: &reason},
	}}
}

func TestBdGuardedMetadataRefusesOlderCapabilityBeforeShowOrBatch(t *testing.T) {
	for _, c := range []struct {
		name, output string
		err          error
	}{
		{"older", `{"version":"1.3.1"}`, nil},
		{"batch-without-exact-reader", `{"capabilities":["issues.batchApply.cli.json.v1"]}`, nil},
		{"exact-reader-without-batch", `{"capabilities":["issues.getExact.cli.json.v1"]}`, nil},
		{"other-cli-grammar", `{"capabilities":["issues.batchApply.cli.v1"]}`, nil},
		{"http-only", `{"capabilities":["issues.batchApplyLarge"]}`, nil},
		{"malformed", `{"capabilities":`, nil},
		{"failed-probe", "", errors.New("unavailable")},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls [][]string
			store := NewBdStore("owned", func(dir, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string(nil), args...))
				if dir != "owned" || name != "bd" || !reflect.DeepEqual(args, []string{"version", "--json"}) {
					t.Fatalf("unsupported probe reached another command: %s %s %v", dir, name, args)
				}
				return []byte(c.output), c.err
			})
			if rows, err := store.ApplyGuardedMetadataBatch(guardedBDRequest()); err == nil || len(rows) != 0 {
				t.Fatalf("unsupported batch accepted: rows=%v err=%v", rows, err)
			}
			if len(calls) != 1 {
				t.Fatalf("got %d commands, want only store-free probe", len(calls))
			}
		})
	}
}

func TestBdGuardedMetadataKeepsRawTypedSnapshot(t *testing.T) {
	store := NewBdStore("owned", func(_ string, _ string, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte(`{"schema_version":1,"data":` + guardedVersionJSON + `}`), nil
		}
		if !reflect.DeepEqual(args, []string{"show", "--exact-store", "--json", "--", "gc-guard", "gc-payload"}) {
			t.Fatalf("unsafe exact-ID arguments: %v", args)
		}
		return []byte(guardedRowsJSON), nil
	})
	rows, err := store.SnapshotMetadata([]string{"gc-guard", "gc-payload"})
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Revision != "9223372036854775807" || !strings.Contains(string(rows[0].Metadata), "9007199254740993123456789") {
		t.Fatalf("raw snapshot lost precision: %+v", rows[0])
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(rows[0].Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"bool": "true", "object": `{"x":1}`, "array": "[1,false]", "null": "null"} {
		if string(metadata[key]) != want {
			t.Fatalf("typed sibling %s changed: %s", key, metadata[key])
		}
	}
}

func TestBdGuardedMetadataUsesOneGuardedNativeRequestAndNormalClose(t *testing.T) {
	var calls [][]string
	var requestPath string
	store := NewBdStore("owned", func(dir, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "version":
			return []byte(guardedVersionJSON), nil
		case "show":
			return []byte(guardedRowsJSON), nil
		case "--actor":
			if dir != "owned" || name != "bd" || len(args) != 7 || args[0] != "--actor" || args[1] != nativeDoltStoreActor || args[2] != "batch" || args[3] != "--input-format=json" || args[4] != "--file" || args[6] != "--json" {
				t.Fatalf("wrong native batch: %v", args)
			}
			requestPath = args[5]
			info, err := os.Stat(requestPath)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("request permissions: %v %v", info, err)
			}
			raw, err := os.ReadFile(requestPath)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				SchemaVersion string `json:"schema_version"`
				Request       struct {
					Actor json.RawMessage `json:"actor"`
					Items []struct {
						Kind   string `json:"kind"`
						Update struct {
							ExpectedVersion  string `json:"expected_version"`
							ExpectedStatus   string `json:"expected_status"`
							ExpectedAssignee string `json:"expected_assignee"`
							Patch            struct {
								Metadata struct {
									Set     map[string]json.RawMessage `json:"set"`
									Replace json.RawMessage            `json:"replace"`
								} `json:"metadata"`
							} `json:"patch"`
						} `json:"update"`
						Close struct {
							Reason, Session string
							Force           bool
						} `json:"close"`
					} `json:"items"`
				} `json:"request"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.SchemaVersion != "1" || len(wire.Request.Actor) != 0 || len(wire.Request.Items) != 3 || wire.Request.Items[0].Update.ExpectedVersion != "9223372036854775807" || wire.Request.Items[0].Update.ExpectedStatus != "in_progress" || wire.Request.Items[0].Update.ExpectedAssignee != "worker" {
				t.Fatalf("guard changed on transport: %s", raw)
			}
			if string(wire.Request.Items[0].Update.Patch.Metadata.Set["nonce"]) != `"new"` || len(wire.Request.Items[0].Update.Patch.Metadata.Replace) != 0 {
				t.Fatalf("patch replaced typed siblings: %s", raw)
			}
			if wire.Request.Items[2].Kind != "close" || wire.Request.Items[2].Close.Force || wire.Request.Items[2].Close.Session != "admitted-session" || wire.Request.Items[2].Close.Reason != "finished" {
				t.Fatalf("normal admitted close changed: %s", raw)
			}
			return []byte(`{"schema_version":1,"keys":{},"items":[{"kind":"update","issue_id":"gc-guard","changed":true,"revision":"-9223372036854775808"},{"kind":"update","issue_id":"gc-payload","changed":true,"revision":"20"},{"kind":"close","issue_id":"gc-payload","changed":true,"revision":"21"}]}`), nil
		default:
			t.Fatalf("ordinary write fallback: %v", args)
			return nil, nil
		}
	})
	rows, err := store.ApplyGuardedMetadataBatch(guardedBDRequest())
	if err != nil || len(rows) != 2 || rows[0].Revision != "-9223372036854775808" || rows[1].Revision != "21" {
		t.Fatalf("native result: %+v %v", rows, err)
	}
	if len(calls) != 3 {
		t.Fatalf("got %d commands, want probe/read/single apply", len(calls))
	}
	if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
		t.Fatalf("private request not removed: %v", err)
	}
}

func TestBdGuardedMetadataNoopGuardRefusesOtherRowChange(t *testing.T) {
	request := guardedBDRequest()
	request.Rows[0].Set["nonce"] = "old"
	store := NewBdStore("owned", func(_ string, _ string, args ...string) ([]byte, error) {
		if args[0] == "version" {
			return []byte(guardedVersionJSON), nil
		}
		if args[0] != "show" {
			t.Fatalf("no-op guard reached write: %v", args)
		}
		return []byte(guardedRowsJSON), nil
	})
	if rows, err := store.ApplyGuardedMetadataBatch(request); !errors.Is(err, issueops.ErrValidation) || len(rows) != 0 {
		t.Fatalf("no-op guard accepted: %v %v", rows, err)
	}
}

func TestBdGuardedMetadataNeverRetriesRefusalOrAmbiguousAcknowledgment(t *testing.T) {
	for _, c := range []struct {
		name, output string
		err          error
		refusal      error
	}{
		{"late-status-refusal", `{"code":"status_mismatch","error":"late guard"}`, errors.New("exit 1"), issueops.ErrStatusMismatch},
		{"native-stderr-refusal", "", errors.New("bd: {\"schema_version\":1,\"code\":\"version_mismatch\",\"error\":\"late guard\"}\nError: exit 13"), issueops.ErrVersionMismatch},
		{"assignee-refusal", `{"code":"assignee_mismatch","error":"owner moved"}`, errors.New("exit 13"), issueops.ErrAssigneeMismatch},
		{"validation-refusal", `{"code":"validation_failed","error":"invalid request"}`, errors.New("exit 1"), issueops.ErrValidation},
		{"close-refusal", `{"code":"close_blocked","error":"blocked"}`, errors.New("exit 1"), issueops.ErrCloseBlocked},
		{"lost-ack", "", errors.New("connection lost after commit"), nil},
		{"malformed-ack", `{"schema_version":1,"keys":`, nil, nil},
		{"numeric-revision", `{"schema_version":1,"keys":{},"items":[{"kind":"update","issue_id":"gc-guard","changed":true,"revision":19}]}`, nil, nil},
		{"missing-items", `{"schema_version":1,"keys":{},"items":[]}`, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			writes := 0
			var requestPath string
			store := NewBdStore("owned", func(_ string, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "version":
					return []byte(guardedVersionJSON), nil
				case "show":
					return []byte(guardedRowsJSON), nil
				case "--actor":
					requestPath = args[5]
					writes++
					return []byte(c.output), c.err
				default:
					t.Fatalf("ordinary write fallback: %v", args)
					return nil, nil
				}
			})
			rows, err := store.ApplyGuardedMetadataBatch(guardedBDRequest())
			if err == nil || len(rows) != 0 || writes != 1 {
				t.Fatalf("unsafe result/retry: rows=%v err=%v writes=%d", rows, err, writes)
			}
			if c.refusal != nil && !errors.Is(err, c.refusal) {
				t.Fatalf("lost typed native refusal: %v", err)
			}
			if _, err := os.Stat(requestPath); !os.IsNotExist(err) {
				t.Fatalf("private request not removed after failure: %v", err)
			}
		})
	}
}

func TestProxiedGuardedMetadataRefusesUnsupportedWriteLeaf(t *testing.T) {
	store := &ProxiedStore{bd: NewMemStore()}
	if _, err := store.SnapshotMetadata([]string{"gc-guard"}); !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("snapshot used native leaf: %v", err)
	}
	if _, err := store.ApplyGuardedMetadataBatch(guardedBDRequest()); !errors.Is(err, ErrConditionalWriteUnsupported) {
		t.Fatalf("batch used native leaf: %v", err)
	}
}

func TestBdGuardedMetadataPortKeepsDeleteOnlyAndEmptyAssignee(t *testing.T) {
	request := guardedBDRequest()
	empty := ""
	request.Rows[1].Set = nil
	request.Rows[1].Delete = []string{"payload"}
	request.Rows[1].ExpectedAssignee = &empty
	writes := 0
	store := NewBdStore("owned", func(_ string, _ string, args ...string) ([]byte, error) {
		switch args[0] {
		case "version":
			return []byte(guardedVersionJSON), nil
		case "show":
			return []byte(guardedRowsJSON), nil
		case "--actor":
			writes++
			raw, err := os.ReadFile(args[5])
			if err != nil {
				t.Fatal(err)
			}
			var wire bdGuardedMetadataWireRequest
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			update := wire.Request.Items[1].Update
			if update.ExpectedAssignee == nil || *update.ExpectedAssignee != "" ||
				len(update.Patch.Metadata.Set) != 0 || !reflect.DeepEqual(update.Patch.Metadata.Unset, []string{"payload"}) ||
				strings.Contains(string(raw), `"set":null`) {
				t.Fatalf("typed wire lost deletion or explicit empty-assignee guard: %s", raw)
			}
			return []byte(`{"schema_version":1,"keys":{},"items":[{"kind":"update","issue_id":"gc-guard","changed":true,"revision":"18"},{"kind":"update","issue_id":"gc-payload","changed":true,"revision":"20"},{"kind":"close","issue_id":"gc-payload","changed":true,"revision":"21"}]}`), nil
		default:
			t.Fatalf("ordinary write fallback: %v", args)
			return nil, nil
		}
	})
	if rows, err := store.ApplyGuardedMetadataBatch(request); err != nil || len(rows) != 2 || writes != 1 {
		t.Fatalf("delete-only transport = %+v, %v, %d writes", rows, err, writes)
	}
}

func TestBdGuardedMetadataRequiresVersionedAcknowledgment(t *testing.T) {
	plan, err := prepareGuardedMetadataBatch(guardedBDRequest(), nativeDoltStoreActor)
	if err != nil {
		t.Fatal(err)
	}
	const body = `"keys":{},"items":[{"kind":"update","issue_id":"gc-guard","changed":true,"revision":"0"},{"kind":"update","issue_id":"gc-payload","changed":true,"revision":"20"},{"kind":"close","issue_id":"gc-payload","changed":true,"revision":"21"}]`
	for _, c := range []struct {
		name, output string
		valid        bool
	}{
		{"flat", `{"schema_version":1,` + body + `}`, true},
		{"envelope", `{"schema_version":1,"data":{` + body + `}}`, true},
		{"missing", `{` + body + `}`, false},
		{"unsupported", `{"schema_version":2,` + body + `}`, false},
		{"string", `{"schema_version":"1",` + body + `}`, false},
		{"null", `{"schema_version":null,` + body + `}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			writes := 0
			store := NewBdStore("owned", func(_ string, _ string, _ ...string) ([]byte, error) {
				writes++
				return []byte(c.output), nil
			})
			result, err := store.applyNativeGuardedBatch(plan.batch)
			if writes != 1 {
				t.Fatalf("native acknowledgment triggered %d writes, want one", writes)
			}
			if c.valid {
				if err != nil || len(result.Items) != 3 || result.Items[0].RowVersion != 0 {
					t.Fatalf("valid versioned acknowledgment lost zero revision: %+v, %v", result, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "commit outcome unknown") || len(result.Items) != 0 {
				t.Fatalf("untrusted acknowledgment accepted: %+v, %v", result, err)
			}
		})
	}
}
