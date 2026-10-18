package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/steveyegge/beads/issueops"
)

// runRootBatchRace drives the native batch port with a real MemStore revision
// CAS. Native Dolt's transaction itself is covered by the beads integration
// suite; this double checks which original snapshot the producer submits.
type runRootBatchRace struct {
	*beads.MemStore
	beforeApply func()
	applyErr    error
	requests    []beads.GuardedMetadataBatchRequest
	reads       int
}

func (s *runRootBatchRace) SnapshotMetadata(ids []string) ([]beads.MetadataSnapshot, error) {
	s.reads++
	rows := make([]beads.MetadataSnapshot, 0, len(ids))
	for _, id := range ids {
		row, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		metadata, err := json.Marshal(row.Metadata)
		if err != nil {
			return nil, err
		}
		rows = append(rows, beads.MetadataSnapshot{ID: row.ID, Revision: strconv.FormatInt(row.Revision, 10), Status: row.Status, Assignee: row.Assignee, Metadata: metadata})
	}
	return rows, nil
}

func (s *runRootBatchRace) ApplyGuardedMetadataBatch(request beads.GuardedMetadataBatchRequest) ([]beads.GuardedMetadataRowResult, error) {
	s.requests = append(s.requests, request)
	if s.beforeApply != nil {
		s.beforeApply()
	}
	patch := request.Rows[0]
	revision, err := strconv.ParseInt(patch.ExpectedRevision, 10, 64)
	if err != nil {
		return nil, err
	}
	if err := s.UpdateIfMatch(patch.ID, revision, beads.UpdateOpts{Metadata: patch.Set}); err != nil {
		if beads.IsPreconditionFailed(err) {
			return nil, issueops.ErrVersionMismatch
		}
		return nil, err
	}
	if s.applyErr != nil {
		return nil, s.applyErr
	}
	row, err := s.Get(patch.ID)
	return []beads.GuardedMetadataRowResult{{ID: patch.ID, Revision: strconv.FormatInt(row.Revision, 10), Changed: true}}, err
}

func TestRunRootStampGuardedOriginalSnapshot(t *testing.T) {
	for _, change := range []string{"parallel fallback", "root assignment", "unknown committed result"} {
		t.Run(change, func(t *testing.T) {
			root := beads.Bead{ID: "root", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
			step := beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}
			store := &runRootBatchRace{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{root}, nil)}
			initial, err := store.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			store.beforeApply = func() {
				switch change {
				case "parallel fallback":
					if err := store.SetMetadataBatch(root.ID, map[string]string{beadmeta.SessionNameMetadataKey: "winner", beadmeta.WorkDirMetadataKey: "/winner"}); err != nil {
						t.Fatal(err)
					}
				case "root assignment":
					owner := "root-owner"
					if err := store.Update(root.ID, beads.UpdateOpts{Assignee: &owner, Metadata: map[string]string{beadmeta.SessionNameMetadataKey: "root-session"}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if change == "unknown committed result" {
				store.applyErr = errors.New("native acknowledgement lost")
			}
			err = settleRunRootFromStep(gaConfig(), store, step, "step-session", "/step", true, nil)
			if (err == nil) != (change == "parallel fallback") {
				t.Fatalf("settle error = %v for %s", err, change)
			}
			if len(store.requests) != 1 {
				t.Fatalf("writes = %d, want one original intent", len(store.requests))
			}
			request := store.requests[0].Rows[0]
			if request.ExpectedRevision != strconv.FormatInt(initial.Revision, 10) || request.ExpectedStatus == nil || *request.ExpectedStatus != initial.Status || request.ExpectedAssignee == nil || *request.ExpectedAssignee != initial.Assignee {
				t.Fatalf("write did not retain the decision snapshot: %+v, original=%+v", request, initial)
			}
			current, err := store.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "parallel fallback" && (store.reads != 2 || current.Metadata[beadmeta.SessionNameMetadataKey] != "winner" || current.Metadata[beadmeta.WorkDirMetadataKey] != "/winner") {
				t.Fatalf("lost CAS did not reread and preserve the complete winner: reads=%d root=%+v", store.reads, current)
			}
			if change == "root assignment" && (current.Assignee != "root-owner" || current.Metadata[beadmeta.SessionNameMetadataKey] != "root-session" || current.Metadata[beadmeta.WorkDirMetadataKey] != "") {
				t.Fatalf("stale step decision changed assigned-root evidence: %+v", current)
			}
			if change == "unknown committed result" && store.reads != 1 {
				t.Fatalf("uncertain commit was converted using %d reads", store.reads)
			}
		})
	}
}

func TestHookClaimParallelRootFallbackIsStable(t *testing.T) {
	for _, winner := range []string{"worker-a", "worker-b"} {
		t.Run(winner, func(t *testing.T) {
			root := beads.Bead{ID: "run-root", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
			rows := []beads.Bead{root}
			for _, name := range []string{"worker-a", "worker-b"} {
				rows = append(rows, beads.Bead{ID: "step-" + name, Status: "open", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID, beadmeta.RoutedToMetadataKey: "worker"}})
			}
			store := &rootStampRaceStore{MemStore: beads.NewMemStoreFrom(0, rows, nil), readBoth: make(chan struct{}), finish: make(chan struct{}), stop: make(chan struct{}), winner: winner}
			t.Cleanup(func() { close(store.stop) })
			type result struct {
				name               string
				code               int
				output, diagnostic string
			}
			finished := make(chan result, 2)
			for _, name := range []string{"worker-a", "worker-b"} {
				go func() {
					candidate, err := store.MemStore.Get("step-" + name)
					if err != nil {
						finished <- result{name: name, code: 1, diagnostic: err.Error()}
						return
					}
					query, err := json.Marshal([]beads.Bead{candidate})
					if err != nil {
						finished <- result{name: name, code: 1, diagnostic: err.Error()}
						return
					}
					opts := hookClaimOptions{Assignee: name, IdentityCandidates: []string{name}, RouteTargets: []string{"worker"}, SessionID: "session-" + name, Env: []string{"GC_SESSION_ID=session-" + name, "GC_SESSION_NAME=" + name}, VerifiedSession: &session.Info{ID: "session-" + name, InstanceToken: "token"}}
					ops := hookClaimOps{
						Runner: func(string, string) (string, error) { return string(query), nil },
						Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
							status := "in_progress"
							if err := store.MemStore.UpdateIfMatch(id, candidate.Revision, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
								return beads.Bead{}, false, err
							}
							claimed, err := store.MemStore.Get(id)
							return claimed, true, err
						},
						DrainPending:      func(string) (bool, error) { return false, nil },
						ResolveWorkBranch: func(hookClaimWorkTree) string { return "" }, ResolveSessionWorkDir: func(string) string { return "" },
						StampWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string, patch map[string]string) error {
							return store.MemStore.SetMetadataBatch(id, patch)
						},
						ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
							return store.MemStore.Get(id)
						},
						StampSessionClaim: noopStampSessionClaim, PublishRunMap: noopPublishRunMap,
						EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
						PrepareRunRoot: func(_ context.Context, _ string, _ []string, step beads.Bead) error {
							return settleRunRootFromStep(gaConfig(), store, step, name, "/"+name, true, nil)
						},
					}
					var stdout, stderr bytes.Buffer
					code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr)
					finished <- result{name, code, stdout.String(), stderr.String()}
				}()
			}
			select {
			case got := <-finished:
				if got.name != winner || got.code != 0 || got.output != "step-"+winner+"\n" {
					t.Fatalf("first claim did not settle/deliver: %+v", got)
				}
			case <-time.After(hangBudget):
				t.Fatal("first claim did not complete")
			}
			frozen, err := store.MemStore.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			close(store.finish)
			select {
			case got := <-finished:
				if got.code != 0 || got.output != "step-"+got.name+"\n" {
					t.Fatalf("parallel claim did not accept stable winner: %+v", got)
				}
			case <-time.After(hangBudget):
				t.Fatal("parallel claim did not complete")
			}
			current, err := store.MemStore.Get(root.ID)
			if err != nil || !reflect.DeepEqual(current, frozen) {
				t.Fatalf("parallel successful claim changed root fence: before=%+v after=%+v err=%v", frozen, current, err)
			}
		})
	}
}

func TestHookClaimRootBarrierAndUndeliveredFallback(t *testing.T) {
	for _, minted := range []bool{false, true} {
		for _, failure := range []string{"none", "root write", "result pipe", "claim window"} {
			if !minted && failure == "claim window" {
				continue
			}
			t.Run(strconv.FormatBool(minted)+"/"+failure, func(t *testing.T) {
				root := beads.Bead{ID: "root", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
				step := beads.Bead{ID: "step", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}
				store := beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)
				opts := hookClaimOptions{Assignee: "worker", Env: []string{"GC_SESSION_ID=session", "GC_SESSION_NAME=worker"}, VerifiedSession: &session.Info{ID: "session", InstanceToken: "verified"}}
				releases, starts, preparations := 0, 0, 0
				clock := time.Now()
				ops := hookClaimOps{
					Now: func() time.Time { return clock }, InvokedAt: clock, ClaimWindow: 40 * time.Second,
					ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
					ResolveSessionWorkDir: func(string) string { return "" },
					StampWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string, patch map[string]string) error {
						return store.SetMetadataBatch(id, patch)
					},
					ReadWorkMeta:      func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) },
					StampSessionClaim: noopStampSessionClaim, PublishRunMap: noopPublishRunMap,
					PrepareRunRoot: func(context.Context, string, []string, beads.Bead) error {
						preparations++
						if failure == "root write" {
							return errors.New("root CAS unavailable")
						}
						if failure == "claim window" {
							clock = clock.Add(40*time.Second + hookClaimMutationTimeout + time.Second)
						}
						return settleRunRootFromStep(gaConfig(), store, step, "worker", "/worker", true, nil)
					},
					EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {
						starts++
						current, err := store.Get(root.ID)
						if err != nil || current.Metadata[beadmeta.SessionNameMetadataKey] != "worker" || current.Metadata[beadmeta.WorkDirMetadataKey] != "/worker" {
							t.Errorf("start emitted before complete root correlation: %+v, %v", current, err)
						}
					},
					Release:           func(context.Context, string, []string, string, string) (bool, error) { releases++; return true, nil },
					EmitClaimReleased: func(hookClaimReleaseRecord) {},
				}
				ops.applyDefaults()
				var output, stderr bytes.Buffer
				var writer io.Writer = &output
				if failure == "result pipe" {
					writer = brokenPipeWriter{}
				}
				code := writeHookClaimWorkResultForBead(hookClaimJSONResult{BeadID: step.ID}, step, opts, ops, "/rig", minted, writer, &stderr)
				if (code == 0) != (failure == "none") || preparations != 1 {
					t.Fatalf("claim=%d preparations=%d output=%s stderr=%s", code, preparations, output.String(), stderr.String())
				}
				wantReleases := 0
				if minted && failure != "none" {
					wantReleases = 1
				}
				if releases != wantReleases || ((failure == "root write" || failure == "claim window") && (starts != 0 || output.Len() != 0)) {
					t.Fatalf("release/start/delivery boundary: releases=%d want=%d starts=%d output=%s", releases, wantReleases, starts, output.String())
				}
				if failure != "root write" {
					frozen, err := store.Get(root.ID)
					if err != nil {
						t.Fatal(err)
					}
					if err := settleRunRootFromStep(gaConfig(), store, step, "another-worker", "/another", true, nil); err != nil {
						t.Fatal(err)
					}
					current, err := store.Get(root.ID)
					if err != nil || !reflect.DeepEqual(frozen, current) {
						t.Fatalf("delivery/unwind allowed fallback replacement: before=%+v after=%+v err=%v", frozen, current, err)
					}
				}
			})
		}
	}
}

func TestHookClaimRootPreparationRetainsLandingWindow(t *testing.T) {
	originalTimeout := hookClaimMutationTimeout
	hookClaimMutationTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = originalTimeout })
	for _, minted := range []bool{false, true} {
		t.Run(strconv.FormatBool(minted), func(t *testing.T) {
			root := beads.Bead{ID: "root", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
			step := beads.Bead{ID: "step", Status: "in_progress", Assignee: "worker", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID, beadmeta.StepIDMetadataKey: "implementation"}}
			store := &runRootBatchRace{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{root, step}, nil)}
			opts := hookClaimOptions{Assignee: "worker", Env: []string{"GC_SESSION_ID=session", "GC_SESSION_NAME=worker"}, VerifiedSession: &session.Info{ID: "session", InstanceToken: "verified"}}
			releases, starts := 0, 0
			ops := hookClaimOps{
				InvokedAt: time.Now(), ClaimWindow: 3 * time.Second,
				ResolveWorkBranch:     func(hookClaimWorkTree) string { return "" },
				ResolveSessionWorkDir: func(string) string { return "" },
				StampWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string, patch map[string]string) error {
					return store.SetMetadataBatch(id, patch)
				},
				ReadWorkMeta:      func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) { return store.Get(id) },
				StampSessionClaim: noopStampSessionClaim, PublishRunMap: noopPublishRunMap,
				PrepareRunRoot: func(ctx context.Context, _ string, _ []string, step beads.Bead) error {
					// Live routing/session reads can exceed the write reserve while
					// the invoking turn still has time to receive its claim.
					timer := time.NewTimer(250 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
						return settleRunRootFromStep(gaConfig(), store, step, "worker", "/worker", true, nil)
					case <-ctx.Done():
						return ctx.Err()
					}
				},
				EmitExecutionStepStarted: func(beads.Bead, string, []string, string) { starts++ },
				Release:                  func(context.Context, string, []string, string, string) (bool, error) { releases++; return true, nil },
				EmitClaimReleased:        func(hookClaimReleaseRecord) {},
			}
			ops.applyDefaults()
			var output, stderr bytes.Buffer
			code := writeHookClaimWorkResultForBead(hookClaimJSONResult{BeadID: step.ID}, step, opts, ops, "/rig", minted, &output, &stderr)
			if code != 0 || output.String() != step.ID+"\n" || releases != 0 || starts != 1 || len(store.requests) != 1 {
				t.Fatalf("valid preparation consumed the write reserve: code=%d output=%q releases=%d starts=%d writes=%d stderr=%s", code, output.String(), releases, starts, len(store.requests), stderr.String())
			}
			current, err := store.Get(root.ID)
			if err != nil || current.Metadata[beadmeta.SessionNameMetadataKey] != "worker" || current.Metadata[beadmeta.WorkDirMetadataKey] != "/worker" {
				t.Fatalf("delivered claim lacks its complete root correlation: root=%+v err=%v", current, err)
			}
		})
	}
}

func TestHookPrepareRunRootUsesRelocatedClass(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "token")
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	class := newClaimRouteClassStore(t)
	root := mintClaimRouteBead(t, class, "gcg-root", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow})
	step := beads.Bead{ID: "gcg-step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}
	route, err := newHookClaimClassRoute(class)
	if err != nil {
		t.Fatal(err)
	}
	route.resident[root.ID] = true // A completed native residence proof.
	opts := hookClaimOptions{Assignee: "worker", VerifiedSession: &session.Info{ID: sessionID, InstanceToken: "token", SessionNameMetadata: "worker", WorkDir: "/worker"}}
	if err := hookPrepareRunRoot(cfg, cityDir, opts, route)(t.Context(), "/nonexistent-work-leg", nil, step); err != nil {
		t.Fatal(err)
	}
	current, err := class.Get(root.ID)
	if err != nil || current.Metadata[beadmeta.SessionNameMetadataKey] != "worker" || current.Metadata[beadmeta.WorkDirMetadataKey] != "/worker" {
		t.Fatalf("root did not use its class store: %+v, %v", current, err)
	}
	// Session-store fail-open admission supplies no verified identity. It must
	// not manufacture a root correlation or try any native store access.
	if err := hookPrepareRunRoot(cfg, "/absent-city", hookClaimOptions{}, nil)(t.Context(), "/absent-work", nil, step); err != nil {
		t.Fatalf("unknown session changed admission policy: %v", err)
	}
	unnamed := hookClaimOptions{VerifiedSession: &session.Info{ID: sessionID, InstanceToken: "token"}}
	if err := hookPrepareRunRoot(cfg, "/absent-city", unnamed, nil)(t.Context(), "/absent-work", nil, step); err == nil {
		t.Fatal("verified unnamed session acknowledged an unsettled root")
	}
}

func TestRunRootStampPreservesWrapperCapabilitiesAndEmission(t *testing.T) {
	for _, path := range []string{"policy/emitter", "blind/cache/policy"} {
		t.Run(path, func(t *testing.T) {
			root := beads.Bead{ID: "root", Status: "in_progress", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}}
			step := beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}}
			backing := &runRootBatchRace{MemStore: beads.NewMemStoreFrom(0, []beads.Bead{root}, nil)}
			cityPath := t.TempDir()
			var notifications []string
			var store beads.Store
			if path == "policy/emitter" {
				store = &beadPolicyStore{Store: &emittingClassStore{Store: backing, cityPath: cityPath}, cfg: gaConfig()}
			} else {
				cache := beads.NewCachingStoreForTest(&beadPolicyStore{Store: backing, cfg: gaConfig()}, func(kind, id string, _ json.RawMessage) { notifications = append(notifications, kind+":"+id) })
				if err := cache.Prime(t.Context()); err != nil {
					t.Fatal(err)
				}
				store = blindWriteRefusingStore{inner: cache}
			}
			if err := settleRunRootFromStep(gaConfig(), store, step, "worker", "/worker", true, nil); err != nil {
				t.Fatal(err)
			}
			frozen, err := store.Get(root.ID)
			if err != nil {
				t.Fatal(err)
			}
			if frozen.Metadata[beadmeta.SessionNameMetadataKey] != "worker" || frozen.Metadata[beadmeta.WorkDirMetadataKey] != "/worker" {
				t.Fatalf("wrapped root tuple missing: %+v", frozen)
			}
			if err := settleRunRootFromStep(gaConfig(), store, step, "other-worker", "/other", true, nil); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(root.ID)
			if err != nil || !reflect.DeepEqual(current, frozen) || len(backing.requests) != 1 {
				t.Fatalf("wrapped steady state wrote root: frozen=%+v current=%+v requests=%d err=%v", frozen, current, len(backing.requests), err)
			}
			if path == "policy/emitter" {
				rows := beadEvents(readCityJournal(t, cityPath))
				if len(rows) != 1 || rows[0].Type != "bead.updated" || rows[0].Subject != root.ID {
					t.Fatalf("CLI root write notifications: %+v", rows)
				}
			} else if !reflect.DeepEqual(notifications, []string{"bead.updated:" + root.ID}) {
				t.Fatalf("controller root write notifications: %v", notifications)
			}
		})
	}
}

func TestHookPrepareRunRootSeparatesReadAndWriteDeadlines(t *testing.T) {
	originalTimeout := hookClaimMutationTimeout
	hookClaimMutationTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hookClaimMutationTimeout = originalTimeout })
	for _, failure := range []string{"none", "preparation deadline", "write deadline", "landing deadline", "canceled parent", "canceled after snapshot", "unknown write result"} {
		t.Run(failure, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "token")
			cfg, err := loadCityConfig(cityDir, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			parentBudget := 3 * time.Second
			if failure == "preparation deadline" {
				parentBudget = hookClaimMutationTimeout
			}
			if failure == "landing deadline" {
				parentBudget = 300 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
			parentDeadline, _ := ctx.Deadline()
			defer cancel()
			if failure == "canceled parent" {
				cancel()
			}
			originalRunner := hookClaimCommandRunnerWithEnvContext
			t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
			reads, writes := 0, 0
			uncertain := errors.New("native write acknowledgement lost")
			hookClaimCommandRunnerWithEnvContext = func(commandCtx context.Context, env map[string]string) beads.CommandRunner {
				if env["BEADS_ACTOR"] != "worker" {
					t.Fatalf("root runner lost its actor: %q", env["BEADS_ACTOR"])
				}
				return func(dir, name string, args ...string) ([]byte, error) {
					if dir != "/selected-work" || name != "bd" {
						t.Fatalf("root runner changed selected store: %s %s", dir, name)
					}
					if err := commandCtx.Err(); err != nil {
						return nil, err
					}
					switch args[0] {
					case "version":
						reads++
						if reads == 1 {
							timer := time.NewTimer(250 * time.Millisecond)
							defer timer.Stop()
							select {
							case <-timer.C:
							case <-commandCtx.Done():
								return nil, commandCtx.Err()
							}
						}
						return []byte(`{"capabilities":["issues.batchApply.cli.json.v1","issues.getExact.cli.json.v1"]}`), nil
					case "show":
						if !reflect.DeepEqual(args, []string{"show", "--exact-store", "--json", "--", "root"}) {
							t.Fatalf("root read lost exact-store binding: %v", args)
						}
						if failure == "canceled after snapshot" && reads == 1 {
							cancel()
						}
						return []byte(`[{"id":"root","revision":"7","status":"in_progress","assignee":"","metadata":{"gc.kind":"workflow"}}]`), nil
					case "--actor":
						writes++
						deadline, bounded := commandCtx.Deadline()
						if !bounded || time.Until(deadline) > hookClaimMutationTimeout || deadline.After(parentDeadline) {
							t.Fatalf("root write lacks its separate bounded reserve: deadline=%s bounded=%v", deadline, bounded)
						}
						if failure == "write deadline" || failure == "landing deadline" {
							<-commandCtx.Done()
							return nil, commandCtx.Err()
						}
						if failure == "unknown write result" {
							return nil, uncertain
						}
						return []byte(`{"schema_version":1,"keys":{},"items":[{"kind":"update","issue_id":"root","changed":true,"revision":"8"}]}`), nil
					default:
						t.Fatalf("unexpected root command: %v", args)
						return nil, nil
					}
				}
			}
			opts := hookClaimOptions{Assignee: "worker", VerifiedSession: &session.Info{ID: sessionID, InstanceToken: "token", SessionNameMetadata: "worker", WorkDir: "/worker"}}
			step := beads.Bead{ID: "step", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}}
			err = hookPrepareRunRoot(cfg, cityDir, opts, nil)(ctx, "/selected-work", nil, step)
			wantWrites := 1
			if failure == "preparation deadline" || failure == "canceled parent" || failure == "canceled after snapshot" {
				wantWrites = 0
			}
			if (err == nil) != (failure == "none") || writes != wantWrites {
				t.Fatalf("root phase outcome: failure=%s writes=%d want=%d error=%v", failure, writes, wantWrites, err)
			}
			if failure == "unknown write result" && reads != 2 {
				t.Fatalf("uncertain root write reread/replayed: probes=%d writes=%d", reads, writes)
			}
		})
	}
}
