package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

type beadsGuardedMetadataAdmission struct {
	SessionID     string `json:"session_id"`
	InstanceToken string `json:"instance_token"`
	ClaimedBeadID string `json:"claimed_bead_id"`
}

type beadsGuardedMetadataRequest struct {
	Admission beadsGuardedMetadataAdmission `json:"admission"`
	Rows      []beads.GuardedMetadataPatch  `json:"rows"`
}

type beadsGuardedMetadataResult struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	StoreRef      string `json:"store_ref"`
	Rows          any    `json:"rows"`
}

func newBeadsGuardedMetadataBatchCmd(stdout, stderr io.Writer) *cobra.Command {
	var storeRef, requestPath string
	var snapshotIDs []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "guarded-metadata-batch",
		Short: "Read exact metadata revisions or apply one guarded native batch",
		Long: `Read raw metadata with opaque revisions, or patch existing rows in one
native transaction. Select one local store with --store-ref=city:<name> or
--store-ref=rig:<name> and exactly one of --snapshot=<IDs> and --request=<file>.
Use --json for the canonical machine-output contract.

A request contains admission {session_id, instance_token, claimed_bead_id} and
rows with id, expected_revision, require_change, string-valued set and delete
keys. Optional expected_status and expected_assignee predicates are preserved.
The retained native claim must be a changing guard with its in-progress status
and current assignee. Session admission reads the authoritative city session
class; every transaction row remains in the selected work store.

An optional close_reason on a row closes it with normal dependency policy and
the admitted native session in the same transaction. Close reasons must be nonempty.
The complete request is limited to 100 native items: each unique row costs one,
and each close costs one more. Every designated metadata guard must change.
Unsupported providers, stale guards and uncertain commit results fail
without ordinary writes, chunking or a search in another work store. Session
admission is a preflight check; row revisions fence the native transaction.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, err := resolveContext()
			if err == nil && cmd.Flags().Changed("snapshot") == cmd.Flags().Changed("request") {
				err = fmt.Errorf("exactly one of --snapshot and --request is required")
			}
			if err == nil {
				_, _, err = parseBeadsMetadataCASStoreRef(storeRef)
			}
			var request *beadsGuardedMetadataRequest
			if err == nil && cmd.Flags().Changed("request") {
				request, err = readBeadsGuardedMetadataRequest(requestPath)
			} else if err == nil {
				err = validateBeadsGuardedMetadataIDs(snapshotIDs)
			}
			var result beadsGuardedMetadataResult
			if err == nil {
				var cfg *config.City
				cfg, err = loadCityConfig(ctx.CityPath, configWarnWriter(jsonOut, stderr))
				if err == nil {
					result, err = runBeadsGuardedMetadataBatch(ctx.CityPath, cfg, storeRef, snapshotIDs, request, stderr)
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc beads guarded-metadata-batch: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if writeCLIJSONLineOrExit(stdout, stderr, "gc beads guarded-metadata-batch", result) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "exact local store: city:<name> or rig:<name>")
	cmd.Flags().StringSliceVar(&snapshotIDs, "snapshot", nil, "exact physical IDs to read (comma-separated, at most 100)")
	cmd.Flags().StringVar(&requestPath, "request", "", "file containing the retained admission and guarded row patches")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the canonical JSON result")
	return cmd
}

func validateBeadsGuardedMetadataIDs(ids []string) error {
	if len(ids) == 0 || len(ids) > 100 {
		return fmt.Errorf("expected 1-100 complete rows, got %d", len(ids))
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !validMetadataCASToken(id, metadataCASMaxBeadIDBytes) {
			return fmt.Errorf("invalid exact bead ID %q", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate physical row %q: coalesce patches before submission", id)
		}
		seen[id] = true
	}
	return nil
}

func readBeadsGuardedMetadataRequest(path string) (*beadsGuardedMetadataRequest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading request: %w", err)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("request must be valid UTF-8")
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if err := checkBeadsGuardedMetadataJSON(check, "request"); err != nil {
		return nil, err
	}
	if _, err := check.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON after request")
	}
	var request beadsGuardedMetadataRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("decoding request: %w", err)
	}
	if err := (beads.GuardedMetadataBatchRequest{Rows: request.Rows}).Validate(); err != nil {
		return nil, err
	}
	a := request.Admission
	if !validMetadataCASToken(a.SessionID, metadataCASMaxBeadIDBytes) ||
		!validMetadataCASToken(a.ClaimedBeadID, metadataCASMaxBeadIDBytes) || strings.TrimSpace(a.InstanceToken) == "" {
		return nil, fmt.Errorf("admission requires an exact session_id, nonempty instance_token and retained claimed_bead_id")
	}
	if a.SessionID != os.Getenv("GC_SESSION_ID") || a.InstanceToken != os.Getenv("GC_INSTANCE_TOKEN") {
		return nil, fmt.Errorf("admission does not match this process's native session and instance")
	}
	ids := make([]string, len(request.Rows))
	guard := false
	for i, row := range request.Rows {
		ids[i] = row.ID
		if _, err := strconv.ParseInt(row.ExpectedRevision, 10, 64); err != nil {
			return nil, fmt.Errorf("row %q expected_revision: %w", row.ID, err)
		}
		guard = guard || row.RequireChange
	}
	if err := validateBeadsGuardedMetadataIDs(ids); err != nil {
		return nil, err
	}
	if !guard {
		return nil, fmt.Errorf("at least one row must require_change")
	}
	return &request, nil
}

// encoding/json otherwise accepts duplicate properties and null string values.
func checkBeadsGuardedMetadataJSON(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("%s: null request values are unsupported", path)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for decoder.More() {
		childPath := path + "[]"
		if delim == '{' {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key := keyToken.(string)
			if path == "request" || path == "request.admission" || path == "request.rows[]" {
				for _, c := range key {
					if (c < 'a' || c > 'z') && c != '_' {
						return fmt.Errorf("unknown JSON field %q", key)
					}
				}
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON property %q", key)
			}
			seen[key] = true
			childPath = path + "." + key
		}
		if err := checkBeadsGuardedMetadataJSON(decoder, childPath); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	if err == nil && path == "request.rows[]" && !seen["require_change"] {
		err = fmt.Errorf("%s requires an explicit require_change designation", path)
	}
	return err
}

func runBeadsGuardedMetadataBatch(cityPath string, cfg *config.City, storeRef string, snapshotIDs []string, request *beadsGuardedMetadataRequest, stderr io.Writer) (result beadsGuardedMetadataResult, err error) {
	if request != nil {
		if err := (beads.GuardedMetadataBatchRequest{Rows: request.Rows}).Validate(); err != nil {
			return result, err
		}
	}
	scopeRoot, canonicalRef, err := resolveBeadsMetadataCASStore(cfg, cityPath, storeRef)
	if err != nil {
		return result, err
	}
	store, err := openBeadsMetadataCASStore(scopeRoot, cityPath)
	if err != nil {
		return result, fmt.Errorf("opening %s: %w", canonicalRef, err)
	}
	defer func() { err = errors.Join(err, closeBeadsMetadataCASStore(store)) }()
	batch, ok := store.(beads.GuardedMetadataBatchStore)
	if !ok {
		return result, fmt.Errorf("store %s has no native guarded metadata capability", canonicalRef)
	}
	result = beadsGuardedMetadataResult{SchemaVersion: "1", OK: true, StoreRef: canonicalRef}
	if request == nil {
		result.Rows, err = batch.SnapshotMetadata(snapshotIDs)
		return result, err
	}

	// Session class routing starts at the city store, even for a rig work batch.
	// Close only handles opened here, not the shared class-route handle.
	cityStore := store
	if !scopeIsCity(cityPath, scopeRoot) {
		cityStore, err = openBeadsMetadataCASStore(cityPath, cityPath)
		if err != nil {
			return result, fmt.Errorf("opening city session authority: %w", err)
		}
		defer func() { err = errors.Join(err, closeBeadsMetadataCASStore(cityStore)) }()
	}
	info, err := cliSessionFrontDoor(cityStore, cfg, cityPath).GetExactFresh(request.Admission.SessionID)
	if err != nil {
		return result, err
	}
	ids := make([]string, len(request.Rows))
	for i, row := range request.Rows {
		ids[i] = row.ID
	}
	snapshots, err := batch.SnapshotMetadata(ids)
	if err != nil {
		return result, err
	}
	if err := admitBeadsGuardedMetadataBatch(info, *request, snapshots); err != nil {
		return result, err
	}
	for i, patch := range request.Rows {
		row := snapshots[i]
		if patch.CloseReason == nil || beadStatusIsClosed(row.Status) {
			continue
		}
		var metadata beads.StringMap
		if len(row.Metadata) > 0 {
			if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
				return result, fmt.Errorf("reading projected close metadata for %q: %w", row.ID, err)
			}
		}
		if metadata == nil {
			metadata = make(beads.StringMap)
		}
		original := beads.Bead{ID: row.ID, Type: row.Type, Status: row.Status, Assignee: row.Assignee, Labels: row.Labels, Metadata: metadata}
		// The existing gate classifies the original row before projecting edits.
		// A metadata patch cannot exempt an originally gated task from validation.
		closeArgs := []string{"update", row.ID, "--status=closed"}
		for key, value := range patch.Set {
			closeArgs = append(closeArgs, "--set-metadata="+key+"="+value)
		}
		for _, key := range patch.Delete {
			closeArgs = append(closeArgs, "--unset-metadata="+key)
		}
		if runWorkRecordCloseGate(closeArgs, scopeRoot, cityPath, cfg, store, map[string]beads.Bead{row.ID: original}, stderr) {
			return result, fmt.Errorf("work-record close gate refused guarded terminal close")
		}
	}
	result.Rows, err = batch.ApplyGuardedMetadataBatch(beads.GuardedMetadataBatchRequest{Rows: request.Rows, CloseSession: info.ID})
	return result, err
}

func admitBeadsGuardedMetadataBatch(info session.Info, request beadsGuardedMetadataRequest, snapshots []beads.MetadataSnapshot) error {
	a := request.Admission
	if info.ID != a.SessionID || info.InstanceToken != a.InstanceToken || info.CurrentClaimBeadID != a.ClaimedBeadID {
		return fmt.Errorf("retained native session, instance or claimed bead is no longer current")
	}
	switch session.ProjectLifecycle(session.LifecycleInputFromInfo(info)).BaseState {
	case session.BaseStateNone, session.BaseStateActive, session.BaseStateCreating, session.BaseStateStartPending:
	default:
		return fmt.Errorf("retained session is not eligible for guarded writes")
	}
	if len(snapshots) != len(request.Rows) {
		return fmt.Errorf("native snapshots did not return the complete selected request")
	}
	foundClaim := false
	for i, row := range snapshots {
		patch := request.Rows[i]
		if row.ID != patch.ID || row.Revision != patch.ExpectedRevision {
			return fmt.Errorf("row %q exact ID or revision changed", patch.ID)
		}
		if coordclass.ControllerOwned(beads.Bead{Type: row.Type, Labels: row.Labels}) {
			return fmt.Errorf("row %q is controller-owned", row.ID)
		}
		if row.ID != a.ClaimedBeadID {
			continue
		}
		foundClaim = true
		assignee := session.AssigneeIdentifier(info)
		if !patch.RequireChange || patch.ExpectedStatus == nil || *patch.ExpectedStatus != "in_progress" ||
			patch.ExpectedAssignee == nil || *patch.ExpectedAssignee != assignee ||
			row.Status != "in_progress" || row.Assignee != assignee {
			return fmt.Errorf("claimed bead %q requires its changing revision, in-progress status and native assignee guards", row.ID)
		}
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(row.Metadata, &metadata); err != nil {
			return fmt.Errorf("reading claimed bead %q metadata: %w", row.ID, err)
		}
		if raw, present := metadata[beadmeta.SessionIDMetadataKey]; present {
			var sessionID string
			if err := json.Unmarshal(raw, &sessionID); err != nil || sessionID != a.SessionID {
				return fmt.Errorf("claimed bead %q belongs to another native session", row.ID)
			}
		}
	}
	if !foundClaim {
		return fmt.Errorf("retained claimed bead %q must be a changing guard in this store's batch", a.ClaimedBeadID)
	}
	return nil
}
