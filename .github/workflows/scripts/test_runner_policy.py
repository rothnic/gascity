import tempfile
import unittest
from pathlib import Path

import runner_policy


class RunnerPolicyTests(unittest.TestCase):
    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text("julianknutsen\n  Csells  # maintainer\n\n# comment\n", encoding="utf-8")
            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_fork_defaults_to_github_and_force_is_ignored(self) -> None:
        for force in (False, True):
            use_bs, _, runners = runner_policy.select_runners(
                "pull_request", "maintainer", {"maintainer"},
                repository="rothnic/gascity", force_blacksmith=force,
            )
            self.assertFalse(use_bs)
            self.assertEqual(runners["runner_2vcpu"], "ubuntu-latest")
            self.assertEqual(runners["runner_macos"], "macos-15")

    def test_unknown_repository_fails_toward_github(self) -> None:
        result = runner_policy.select_runners("push", "", set(), repository="")
        self.assertFalse(result[0])
        self.assertEqual(result[2], runner_policy.GITHUB_RUNNERS)

    def test_upstream_preserves_blacksmith_and_force_semantics(self) -> None:
        use_bs, _, runners = runner_policy.select_runners(
            "push", "", set(), repository="gastownhall/gascity"
        )
        self.assertTrue(use_bs)
        self.assertEqual(runners, runner_policy.BLACKSMITH_RUNNERS)
        forced = runner_policy.select_runners(
            "workflow_call", "", set(), repository="gastownhall/gascity", force_blacksmith=True
        )
        self.assertTrue(forced[0])

    def test_workflow_bootstraps_are_github_hosted(self) -> None:
        root = Path(__file__).parents[1]
        for name in ("ci.yml", "mac-regression.yml", "review-formulas.yml", "codeql.yml", "container-scan.yml", "govulncheck.yml"):
            text = (root / name).read_text(encoding="utf-8")
            self.assertNotIn("runs-on: blacksmith-2vcpu-ubuntu-2404", text)
            if name in ("ci.yml", "mac-regression.yml", "review-formulas.yml"):
                self.assertIn("RUNNER_REPOSITORY: ${{ github.repository }}", text)

    def test_bazel_fork_paths_do_not_use_blacksmith_or_rbe_mint(self) -> None:
        root = Path(__file__).parents[1]
        for name in ("bazel.yml", "bazel-test.yml"):
            text = (root / name).read_text(encoding="utf-8")
            self.assertNotIn("runs-on: blacksmith-", text)
        bazel = (root / "bazel.yml").read_text(encoding="utf-8")
        self.assertIn('if [ "$REPOSITORY" != "gastownhall/gascity" ]; then', bazel)
        self.assertIn("github.repository", bazel)
        self.assertIn('github.repository == \'gastownhall/gascity\' && (needs.rbe.outputs.mode', bazel)
        bazel_test = (root / "bazel-test.yml").read_text(encoding="utf-8")
        self.assertIn("github.repository == 'gastownhall/gascity'", bazel_test)
        self.assertIn("if: github.repository == 'gastownhall/gascity' &&", bazel_test)

    def test_every_runner_policy_caller_has_safe_bootstrap(self) -> None:
        root = Path(__file__).parents[1]
        for path in root.glob("*.yml"):
            text = path.read_text(encoding="utf-8")
            if "python3 .github/workflows/scripts/runner_policy.py" in text:
                self.assertIn("runs-on: ubuntu-latest", text, path.name)

    def test_review_formulas_codecov_is_upstream_only(self) -> None:
        text = (Path(__file__).parents[1] / "review-formulas.yml").read_text(encoding="utf-8")
        self.assertIn("github.repository == 'gastownhall/gascity'", text)
        self.assertIn("uses: codecov/codecov-action", text)

    def test_hostile_environment_cannot_enable_fork_bazel_rbe(self) -> None:
        result = runner_policy.select_runners(
            "pull_request", "", set(), repository="rothnic/gascity", force_blacksmith=True
        )
        self.assertFalse(result[0])
        self.assertEqual(result[2], runner_policy.GITHUB_RUNNERS)


if __name__ == "__main__":
    unittest.main()
