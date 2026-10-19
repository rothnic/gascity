//go:build integration

package beads_test

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The ordinary-show control must reach the real foreign database first. The
// same exact ID then goes through production BdStore and must fail before any
// batch invocation; an injected reader error cannot establish that boundary.
func TestBdStoreGuardedMetadataExactStoreTransport(t *testing.T) {
	required := false
	if value := strings.TrimSpace(os.Getenv("GC_REQUIRE_BD_GUARDED_METADATA")); value != "" {
		parsed, err := strconv.ParseBool(value)
		required = err != nil || parsed
	}
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		if required {
			t.Fatalf("required guarded metadata contract has no bd: %v", err)
		}
		t.Skipf("bd not on PATH: %v", err)
	}
	local, localDir := newConditionalIntegrationBdStore(t)
	foreign, foreignDir := newConditionalIntegrationBdStore(t)
	localRow, err := local.Create(beads.Bead{Title: "local metadata target", Metadata: map[string]string{"nonce": "old"}})
	if err != nil {
		t.Fatal(err)
	}
	foreignRow, err := foreign.Create(beads.Bead{Title: "foreign metadata target", Metadata: map[string]string{"nonce": "foreign"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bd=%s; local scope=%s row=%s; foreign scope=%s row=%s", bdPath, localDir, localRow.ID, foreignDir, foreignRow.ID)
	routePath, err := filepath.Rel(localDir, foreignDir)
	if err != nil {
		t.Fatal(err)
	}
	route, err := json.Marshal(map[string]string{"prefix": "tst-", "path": filepath.ToSlash(routePath)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, ".beads", "routes.jsonl"), append(route, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newConditionalIntegrationRunner(t, localDir)
	ordinary, err := run(localDir, "bd", "show", "--json", "--", foreignRow.ID)
	var ordinaryRows []struct {
		ID string `json:"id"`
	}
	if err != nil || json.Unmarshal(ordinary, &ordinaryRows) != nil || len(ordinaryRows) != 1 || ordinaryRows[0].ID != foreignRow.ID {
		t.Fatalf("ordinary show must actually route to the foreign database: %v %s", err, ordinary)
	}

	// Stock-CLI shards skip an unavailable contract; its equipped native cell
	// sets GC_REQUIRE_BD_GUARDED_METADATA and must execute every assertion.
	localSnapshots, err := local.SnapshotMetadata([]string{localRow.ID})
	if errors.Is(err, beads.ErrConditionalWriteUnsupported) && !required {
		t.Skip("installed bd lacks the guarded metadata/exact-store contract")
	}
	if err != nil || len(localSnapshots) != 1 {
		t.Fatalf("new exact-reader/batch capabilities and local positive required: %v %+v", err, localSnapshots)
	}
	foreignBefore, err := foreign.SnapshotMetadata([]string{foreignRow.ID})
	if err != nil || len(foreignBefore) != 1 {
		t.Fatalf("foreign fixture positive: %v %+v", err, foreignBefore)
	}
	batchCalls := 0
	guarded := beads.NewBdStore(localDir, func(dir, name string, args ...string) ([]byte, error) {
		verb := args
		if len(verb) >= 2 && verb[0] == "--actor" {
			verb = verb[2:]
		}
		if len(verb) != 0 && verb[0] == "batch" {
			batchCalls++
		}
		return run(dir, name, args...)
	})
	for _, ids := range [][]string{{foreignRow.ID}, {localRow.ID, foreignRow.ID}} {
		if rows, err := guarded.SnapshotMetadata(ids); err == nil || len(rows) != 0 {
			t.Fatalf("foreign exact snapshot was attributed to local store: %v %+v", err, rows)
		}
	}
	request := beads.GuardedMetadataBatchRequest{Rows: []beads.GuardedMetadataPatch{
		{ID: foreignRow.ID, ExpectedRevision: foreignBefore[0].Revision, RequireChange: true, Set: map[string]string{"nonce": "must-not-land"}},
	}}
	if rows, err := guarded.ApplyGuardedMetadataBatch(request); err == nil || len(rows) != 0 || batchCalls != 0 {
		t.Fatalf("foreign snapshot reached native batch/fallback: %v %+v batch calls=%d", err, rows, batchCalls)
	}
	foreignAfter, err := foreign.SnapshotMetadata([]string{foreignRow.ID})
	if err != nil || len(foreignAfter) != 1 || foreignAfter[0].Revision != foreignBefore[0].Revision || string(foreignAfter[0].Metadata) != string(foreignBefore[0].Metadata) {
		t.Fatalf("refused read/apply changed foreign metadata: %v %+v", err, foreignAfter)
	}

	request.Rows[0] = beads.GuardedMetadataPatch{ID: localRow.ID, ExpectedRevision: localSnapshots[0].Revision, RequireChange: true, Set: map[string]string{"nonce": "new"}}
	if rows, err := guarded.ApplyGuardedMetadataBatch(request); err != nil || len(rows) != 1 || !rows[0].Changed || batchCalls != 1 {
		t.Fatalf("local existing-row transaction refused: %v %+v batch calls=%d", err, rows, batchCalls)
	}
	after, err := guarded.SnapshotMetadata([]string{localRow.ID})
	if err != nil || len(after) != 1 || !strings.Contains(string(after[0].Metadata), `"new"`) {
		t.Fatalf("local metadata did not change: %v %+v", err, after)
	}
}
