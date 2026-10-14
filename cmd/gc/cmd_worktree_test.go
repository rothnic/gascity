package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worktree"
)

func worktreeTestRepo(t *testing.T) (string, string) {
	t.Helper()
	return testutil.InitGitRepo(t)
}

func managedWorktreeCmdOpts(repo, root, path, base string) worktreeCmdOpts {
	return worktreeCmdOpts{
		Repo:       repo,
		Root:       root,
		Path:       path,
		Branch:     "work/gc-test",
		Base:       base,
		BeadID:     "gc-test",
		StoreRef:   "gascity",
		Creator:    "test",
		Owner:      "gc-sling",
		Generation: "1",
		Lifecycle:  worktree.LifecycleActive,
	}
}

func TestCmdWorktreeLockTimeoutFlagContract(t *testing.T) {
	for _, cmd := range newWorktreeCmd(&bytes.Buffer{}, &bytes.Buffer{}).Commands() {
		if cmd.Name() == "verify" {
			if cmd.Flags().Lookup("lock-timeout") != nil {
				t.Fatal("read-only Verify exposes a mutation lock wait")
			}
			continue
		}
		if timeout, err := cmd.Flags().GetDuration("lock-timeout"); err != nil || timeout != 30*time.Second {
			t.Errorf("%s lock timeout = %s, %v; want bounded 30s default", cmd.Name(), timeout, err)
		}
	}
}

func TestCmdWorktreeInvalidLockTimeoutIsInert(t *testing.T) {
	clearCityEnv(t)
	t.Setenv("GIT_OPTIONAL_LOCKS", "0")
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	before := testutil.RunGit(t, repo, "for-each-ref", "--format=%(refname):%(objectname)")
	for _, action := range []string{"ensure", "cleanup"} {
		for _, timeout := range []string{"0", "-1s", "invalid", "9223372036854775808ns"} {
			t.Run(action+"/"+timeout, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				cmd := newWorktreeCmd(&stdout, &stderr)
				args := []string{
					action, "--repo", repo, "--root", root, "--path", filepath.Join(root, "wt"),
					"--branch", "work/gc-test", "--base", base, "--bead", "gc-test", "--store-ref", "gascity",
					"--creator", "test", "--owner", "gc-sling", "--generation", "1", "--lock-timeout=" + timeout,
				}
				if action == "cleanup" {
					args = append(args, "--attempt-id", "unused")
				}
				cmd.SetArgs(args)
				if err := cmd.ExecuteContext(t.Context()); err == nil ||
					((timeout == "0" || timeout == "-1s") && !strings.Contains(err.Error(), "must be positive")) {
					t.Fatalf("invalid timeout returned %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
				}
				if after := testutil.RunGit(t, repo, "for-each-ref", "--format=%(refname):%(objectname)"); after != before {
					t.Fatal("invalid timeout changed repository refs")
				}
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
					t.Fatalf("invalid timeout changed workspace root: %v, %v", entries, err)
				}
				for _, dir := range []string{"gc-worktree-locks", "worktrees"} {
					if _, err := os.Stat(filepath.Join(repo, ".git", dir)); !os.IsNotExist(err) {
						t.Fatalf("invalid timeout prepared %s: %v", dir, err)
					}
				}
			})
		}
	}
}

func TestCmdWorktreeEnsureCreatesAndVerifies(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer

	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.JSON = true
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ensure exit = %d, stderr: %s", code, stderr.String())
	}
	var rep struct {
		Path          string `json:"path"`
		Branch        string `json:"branch"`
		Created       bool   `json:"created"`
		BranchCreated bool   `json:"branch_created"`
		Provenance    *struct {
			BaseSHA   string `json:"base_sha"`
			AttemptID string `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal ensure output %q: %v", stdout.String(), err)
	}
	if !rep.Created || !rep.BranchCreated || rep.Branch != "work/gc-test" ||
		rep.Provenance == nil || rep.Provenance.BaseSHA == "" || rep.Provenance.AttemptID == "" {
		t.Errorf("report = %+v, want created managed worktree with publishable provenance", rep)
	}

	// verify must pass on the ensured worktree.
	stdout.Reset()
	stderr.Reset()
	verifyOpts := opts
	verifyOpts.BaseSHA = rep.Provenance.BaseSHA
	code = runWorktreeVerify(verifyOpts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("verify exit = %d, stderr: %s", code, stderr.String())
	}
}

// clearCityEnv forces resolveCity to fail deterministically via the
// ambient-upward-discovery refusal test binaries hit (main.go's
// isTestBinary branch), regardless of what the host process inherited.
func clearCityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GC_CITY", "GC_CITY_PATH", "GC_CITY_ROOT", "GC_DIR", "GC_RIG"} {
		t.Setenv(k, "")
	}
}

func TestCmdWorktreeEnsureAgentFlagAttemptsMaterialization(t *testing.T) {
	clearCityEnv(t)
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer

	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.Agent = "gc-test-agent"
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ensure exit = %d, want 0 even when materialization can't resolve a city; stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	if !strings.Contains(stderr.String(), "skill materialization skipped") {
		t.Errorf("stderr = %q, want evidence that --agent triggered a materialization attempt", stderr.String())
	}
}

func TestCmdWorktreeEnsureAgentMaterializesSkillsAndKeepsJSONPure(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeMaterializeTestCityFile(t, cityDir, "city.toml", "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n")
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeBuiltinImportsFixture(t, cityDir, "core")

	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer

	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.Agent = "mayor"
	opts.JSON = true
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ensure exit = %d, stderr: %s", code, stderr.String())
	}
	var v map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\nstdout: %q", err, stdout.String())
	}
	link := filepath.Join(wt, ".claude", "skills", "core.gc-work")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v; stderr: %s", link, err, stderr.String())
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
}

func TestCmdWorktreeEnsureNoAgentSkipsMaterialization(t *testing.T) {
	clearCityEnv(t)
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer

	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ensure exit = %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "skill materialization") {
		t.Errorf("stderr = %q, omitting --agent must not attempt materialization (regression guard for existing callers)", stderr.String())
	}
}

func TestCmdWorktreeVerifyFailsOnMissing(t *testing.T) {
	repo, _ := worktreeTestRepo(t)
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	opts := managedWorktreeCmdOpts(repo, root, filepath.Join(root, "nope"), "main")
	code := runWorktreeVerify(opts, &stdout, &stderr)
	if code == 0 {
		t.Fatal("verify on missing worktree returned 0, want nonzero")
	}
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Errorf("stderr %q does not explain the missing path", stderr.String())
	}
}

func TestCmdWorktreeEnsureDryRunIsPure(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	var stdout, stderr bytes.Buffer
	opts := managedWorktreeCmdOpts(repo, root, wt, base)
	opts.DryRun = true
	opts.JSON = true
	code := runWorktreeEnsure(opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run ensure exit = %d, stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("dry-run ensure created the worktree path")
	}
	var rep struct {
		Planned []string `json:"planned"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal dry-run output %q: %v", stdout.String(), err)
	}
	if len(rep.Planned) == 0 {
		t.Error("dry-run output has no planned actions")
	}
}

func TestCmdWorktreeManagedFlagsReachSpec(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	path := filepath.Join(root, "gc-test")
	opts := managedWorktreeCmdOpts(repo, root, path, base)
	opts.BaseSHA = strings.Repeat("a", 40)

	spec, err := opts.spec()
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	if spec.Root != root || spec.BeadID != opts.BeadID || spec.StoreRef != opts.StoreRef ||
		spec.BaseSHA != opts.BaseSHA || spec.Creator != opts.Creator || spec.Owner != opts.Owner ||
		spec.Generation != opts.Generation || spec.Lifecycle != opts.Lifecycle {
		t.Fatalf("spec = %+v, want all managed ownership fields from CLI", spec)
	}
}

func TestCmdWorktreeRegistered(t *testing.T) {
	root := newRootCmd(&bytes.Buffer{}, &bytes.Buffer{})
	for _, c := range root.Commands() {
		if c.Name() == "worktree" {
			return
		}
	}
	t.Fatal("gc worktree command is not registered on the root command")
}

func TestCmdWorktreeEnsureStrictJSONContract(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	args := []string{
		"worktree", "ensure", "--json",
		"--repo", repo,
		"--root", root,
		"--path", wt,
		"--branch", "work/gc-test",
		"--base", base,
		"--bead", "gc-test",
		"--store-ref", "gascity",
		"--creator", "test",
		"--owner", "gc-sling",
		"--generation", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) failed: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "json_unsupported") {
		t.Fatalf("gc worktree ensure still lacks declared JSON support: %s", stdout.String())
	}
	var result struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Command       string `json:"command"`
		Action        string `json:"action"`
		Path          string `json:"path"`
		Provenance    *struct {
			SchemaVersion int     `json:"schema_version"`
			Mode          *string `json:"mode"`
			Placement     *string `json:"placement"`
			AttemptID     string  `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal JSON output %q: %v", stdout.String(), err)
	}
	if result.SchemaVersion != "1" || !result.OK || result.Command != "worktree ensure" ||
		result.Action != "ensure" || result.Path != wt ||
		result.Provenance == nil || result.Provenance.AttemptID == "" ||
		result.Provenance.SchemaVersion != 1 || result.Provenance.Mode != nil || result.Provenance.Placement != nil {
		t.Fatalf("JSON result = %+v, want declared structured ensure output", result)
	}
	if ref, err := git.New(wt).HeadSymbolicRef(); err != nil || ref != "refs/heads/work/gc-test" {
		t.Fatalf("omitted mode HEAD = %q, err = %v, want attached work/gc-test", ref, err)
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())
}

func TestCmdWorktreeEnsureDetachedExactBase(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	repo, _ := worktreeTestRepo(t)
	base := testutil.RunGit(t, repo, "rev-parse", "HEAD")
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	refsBefore := testutil.RunGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	args := []string{
		"worktree", "ensure", "--json", "--mode", "detached",
		"--repo", repo,
		"--root", root,
		"--path", wt,
		"--base", base,
		"--base-sha", base,
		"--bead", "gc-test",
		"--store-ref", "gascity",
		"--creator", "test",
		"--owner", "gc-sling",
		"--generation", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("detached ensure exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	var rep worktree.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshal detached ensure output %q: %v", stdout.String(), err)
	}
	if rep.Path != wt || rep.Mode != "detached" || rep.Branch != "" || rep.Head != base || !rep.Created || rep.BranchCreated {
		t.Fatalf("report = %+v, want detached creation at exact base without a branch", rep)
	}
	wtGit := git.New(wt)
	if ref, err := wtGit.HeadSymbolicRef(); err == nil {
		t.Fatalf("detached HEAD is symbolic: %q", ref)
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Fatalf("symbolic-ref error = %v, want detached exit 1", err)
		}
	}
	if got := testutil.RunGit(t, wt, "rev-parse", "HEAD"); got != base {
		t.Fatalf("actual HEAD = %s, want supplied base %s", got, base)
	}
	if got := testutil.RunGit(t, repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/"); got != refsBefore {
		t.Fatalf("detached creation changed branch refs: before = %q, after = %q", refsBefore, got)
	}
	wantPath, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatalf("canonical worktree path: %v", err)
	}
	common, err := git.New(repo).CommonDir()
	if err != nil {
		t.Fatalf("repository common dir: %v", err)
	}
	wantRepo, err := filepath.EvalSymlinks(common)
	if err != nil {
		t.Fatalf("canonical repository identity: %v", err)
	}
	p := rep.Provenance
	if p == nil || p.SchemaVersion != 2 || p.Mode != "detached" || p.Placement != "outside-worktrees" ||
		p.RepoIdentity != wantRepo || p.Path != wantPath || p.Branch != "" || p.BaseRef != base || p.BaseSHA != base ||
		p.BeadID != "gc-test" || p.StoreRef != "gascity" || p.Creator != "test" || p.Owner != "gc-sling" ||
		p.Generation != "1" || p.Lifecycle != worktree.LifecycleActive || p.AttemptID == "" || p.CreatedAt == nil {
		t.Fatalf("provenance = %+v, want complete detached owner/base/attempt identity", p)
	}
	gitDir, err := wtGit.GitDir()
	if err != nil {
		t.Fatalf("worktree private Git dir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(gitDir, "gascity-worktree-provenance.json"))
	if err != nil {
		t.Fatalf("read durable provenance: %v", err)
	}
	var durable worktree.Provenance
	if err := json.Unmarshal(body, &durable); err != nil {
		t.Fatalf("unmarshal durable provenance: %v", err)
	}
	if !reflect.DeepEqual(*p, durable) {
		t.Fatalf("reported provenance = %+v, durable provenance = %+v", *p, durable)
	}
	entries, err := git.New(repo).WorktreeList()
	if err != nil {
		t.Fatalf("list registered worktrees: %v", err)
	}
	matches := 0
	for _, entry := range entries {
		path, err := filepath.EvalSymlinks(entry.Path)
		if err != nil {
			t.Fatalf("canonical registered path: %v", err)
		}
		if path == wantPath {
			matches++
			if entry.Head != base || entry.Branch != "" {
				t.Fatalf("registration = %+v, want exact detached base", entry)
			}
		}
	}
	if matches != 1 {
		t.Fatalf("matching registrations = %d, want 1", matches)
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())
	createdJSON := append([]byte(nil), stdout.Bytes()...)
	for _, action := range []string{"ensure", "verify"} {
		args[1] = action
		stdout.Reset()
		stderr.Reset()
		if code := run(args, &stdout, &stderr); code != 0 {
			t.Fatalf("detached %s exit %d: %s", action, code, stderr.String())
		}
		var reused worktreeJSONResult
		if err := json.Unmarshal(stdout.Bytes(), &reused); err != nil || reused.Created || reused.BranchCreated ||
			reused.Mode != worktree.ModeDetached || reused.Head != base || !reflect.DeepEqual(reused.Provenance, rep.Provenance) {
			t.Fatalf("detached reuse/verify = %+v, %v", reused, err)
		}
		validateJSONAgainstResultSchema(t, []string{"worktree", action}, stdout.Bytes())
	}
	for _, change := range []func(map[string]any){
		func(v map[string]any) { v["mode"] = "unknown" },
		func(v map[string]any) { delete(v["provenance"].(map[string]any), "mode") },
		func(v map[string]any) { v["provenance"].(map[string]any)["schema_version"] = 1 },
		func(v map[string]any) { v["provenance"].(map[string]any)["placement"] = "within-repo-root" },
		func(v map[string]any) { v["branch"] = "unexpected-branch" },
		func(v map[string]any) { v["branch_created"] = true },
	} {
		var invalid map[string]any
		if err := json.Unmarshal(createdJSON, &invalid); err != nil {
			t.Fatal(err)
		}
		change(invalid)
		body, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateJSONAgainstResultSchemaE([]string{"worktree", "ensure"}, body); err == nil {
			t.Fatalf("schema accepted conflicting detached evidence: %s", body)
		}
	}
	for _, invalidMode := range []string{"", "unknown"} {
		bad := append([]string(nil), args...)
		bad[1], bad[4] = "ensure", invalidMode
		stdout.Reset()
		stderr.Reset()
		cmd := newWorktreeCmd(&stdout, &stderr)
		cmd.SetArgs(bad[1:])
		err := cmd.ExecuteContext(t.Context())
		if err == nil {
			t.Fatalf("raw Cobra accepted --mode %q", invalidMode)
		}
		if !strings.Contains(err.Error()+stderr.String(), "mode") {
			t.Fatalf("invalid mode failed for another reason: %v; stderr=%q", err, stderr.String())
		}
	}
	if got, err := os.ReadFile(filepath.Join(gitDir, "gascity-worktree-provenance.json")); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("reuse/refusal changed durable provenance: %v", err)
	}
	args[1] = "cleanup"
	args = append(args, "--attempt-id", p.AttemptID)
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("detached cleanup exit %d: %s", code, stderr.String())
	}
	var removed worktreeCleanupJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &removed); err != nil || !removed.Removed || removed.Mode != worktree.ModeDetached || removed.Branch != "" {
		t.Fatalf("detached cleanup = %+v, %v", removed, err)
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "cleanup"}, stdout.Bytes())
	t.Run("explicit attached schema 2", func(t *testing.T) {
		attached := append([]string(nil), args[:len(args)-2]...)
		attached[1], attached[4] = "ensure", worktree.ModeAttached
		attachedPath := filepath.Join(root, "attached")
		for i, arg := range attached {
			if arg == "--path" {
				attached[i+1] = attachedPath
			}
		}
		attached = append(attached, "--branch", "work/explicit-attached")
		var stdout, stderr bytes.Buffer
		if code := run(attached, &stdout, &stderr); code != 0 {
			t.Fatalf("explicit attached ensure exit %d: %s", code, stderr.String())
		}
		var result worktreeJSONResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || !result.Created || !result.BranchCreated ||
			result.Mode != worktree.ModeAttached || result.Provenance == nil || result.Provenance.SchemaVersion != 2 ||
			result.Provenance.Mode != worktree.ModeAttached || result.Provenance.Placement != worktree.PlacementOutsideWorktrees {
			t.Fatalf("explicit attached report = %+v, %v", result, err)
		}
		if ref, err := git.New(attachedPath).HeadSymbolicRef(); err != nil || ref != "refs/heads/work/explicit-attached" {
			t.Fatalf("explicit attached HEAD = %q, %v", ref, err)
		}
		validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())
	})
}

// ensureAttemptID provisions the worktree described by opts and returns the
// attempt id its ensure reported. Cleanup is authorized per provisioning
// attempt, so a caller that wants to clean up what it just created has to carry
// that id forward; routing the test through the CLI's own JSON output is also
// what proves the two commands agree on the field.
func ensureAttemptID(t *testing.T, opts *worktreeCmdOpts) string {
	t.Helper()
	jsonOpts := *opts
	jsonOpts.JSON = true
	var stdout, stderr bytes.Buffer
	if code := runWorktreeEnsure(jsonOpts, &stdout, &stderr); code != 0 {
		t.Fatalf("runWorktreeEnsure setup exit = %d (stderr: %s)", code, stderr.String())
	}
	var result struct {
		Provenance struct {
			AttemptID string `json:"attempt_id"`
		} `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal ensure output %q: %v", stdout.String(), err)
	}
	if result.Provenance.AttemptID == "" {
		t.Fatalf("ensure output %q carries no attempt id", stdout.String())
	}
	return result.Provenance.AttemptID
}

func TestCmdWorktreeCleanupJSONReportsPendingSafetyRefusal(t *testing.T) {
	for _, mode := range []string{"", worktree.ModeDetached} {
		t.Run("mode="+mode, func(t *testing.T) {
			repo, base := worktreeTestRepo(t)
			root := t.TempDir()
			wt := filepath.Join(root, "wt")
			opts := managedWorktreeCmdOpts(repo, root, wt, base)
			opts.Mode = mode
			if mode == worktree.ModeDetached {
				opts.Branch = ""
			}
			opts.AttemptID = ensureAttemptID(t, &opts)
			if err := os.WriteFile(filepath.Join(wt, "keep.txt"), []byte("keep"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}

			opts.JSON = true
			var stdout, stderr bytes.Buffer
			code := runWorktreeCleanup(opts, &stdout, &stderr)
			if code == 0 {
				t.Fatal("runWorktreeCleanup dirty worktree returned 0, want nonzero")
			}
			var result struct {
				SchemaVersion  string `json:"schema_version"`
				OK             bool   `json:"ok"`
				Command        string `json:"command"`
				Action         string `json:"action"`
				CleanupPending bool   `json:"cleanup_pending"`
				Error          *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatalf("unmarshal cleanup output %q: %v", stdout.String(), err)
			}
			if result.SchemaVersion != "1" || result.OK || result.Command != "worktree cleanup" ||
				result.Action != "cleanup" || !result.CleanupPending || result.Error == nil ||
				result.Error.Code != worktree.CleanupErrorDirty || result.Error.Message == "" {
				t.Fatalf("cleanup result = %+v, want structured cleanup_pending dirty refusal", result)
			}
			if _, err := os.Stat(filepath.Join(wt, "keep.txt")); err != nil {
				t.Fatalf("cleanup safety refusal removed WIP: %v", err)
			}
		})
	}
}

// TestCmdWorktreeEnsureDryRunJSONContract schema-validates the DRY-RUN output.
// Only the real-creation path was validated before, which is how planned
// provenance came to emit a zero timestamp and an empty attempt id while the
// doc comment promised both were absent. A consumer publishing that evidence
// onto a bead would have stored 0001-01-01T00:00:00Z as a creation time.
func TestCmdWorktreeEnsureDryRunJSONContract(t *testing.T) {
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	args := []string{
		"worktree", "ensure", "--json", "--dry-run",
		"--repo", repo,
		"--root", root,
		"--path", wt,
		"--branch", "work/gc-test",
		"--base", base,
		"--bead", "gc-test",
		"--store-ref", "gascity",
		"--creator", "test",
		"--owner", "gc-sling",
		"--generation", "1",
	}
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("run(%v) failed: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
	}
	validateJSONAgainstResultSchema(t, []string{"worktree", "ensure"}, stdout.Bytes())

	if body := stdout.String(); strings.Contains(body, "0001-01-01") {
		t.Errorf("dry-run emitted a zero creation time: %s", body)
	}
	var result struct {
		Provenance map[string]any `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, absent := range []string{"created_at", "attempt_id"} {
		if _, present := result.Provenance[absent]; present {
			t.Errorf("dry-run provenance carries %q = %v, want it omitted until the worktree exists",
				absent, result.Provenance[absent])
		}
	}
}
