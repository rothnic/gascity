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
        result = runner_policy.select_runners("push", "", set())
        self.assertFalse(result[0])
        self.assertEqual(result[2], runner_policy.GITHUB_RUNNERS)

    def test_upstream_preserves_blacksmith_and_force_semantics(self) -> None:
        result = runner_policy.select_runners("push", "", set(), repository="gastownhall/gascity")
        self.assertTrue(result[0])
        self.assertEqual(result[2], runner_policy.BLACKSMITH_RUNNERS)
        forced = runner_policy.select_runners(
            "workflow_call", "", set(), repository="gastownhall/gascity", force_blacksmith=True
        )
        self.assertTrue(forced[0])

    def test_all_runner_policy_bootstraps_are_github_hosted(self) -> None:
        root = Path(__file__).parents[1]
        callers = []
        for path in root.glob("*.yml"):
            text = path.read_text(encoding="utf-8")
            if "python3 .github/workflows/scripts/runner_policy.py" in text:
                callers.append(path.name)
                self.assertRegex(text, r"(?m)^\s+runs-on: ubuntu-latest$")
        self.assertEqual(set(callers), {
            "ci.yml", "mac-regression.yml", "review-formulas.yml",
            "codeql.yml", "container-scan.yml", "govulncheck.yml",
        })

    def test_review_formulas_codecov_is_upstream_only(self) -> None:
        text = (Path(__file__).parents[1] / "review-formulas.yml").read_text(encoding="utf-8")
        start = text.index("      - name: Upload shard coverage to Codecov")
        end = text.index("  review-formulas:", start)
        step = text[start:end]
        self.assertIn("github.repository == 'gastownhall/gascity'", step)
        self.assertIn("steps.shard.outcome == 'success'", step)
        self.assertIn("uses: codecov/codecov-action", step)

    def test_bazel_workflows_gate_remote_features_on_upstream_identity(self) -> None:
        root = Path(__file__).parents[1]
        multi = (root / "bazel.yml").read_text(encoding="utf-8")
        side = (root / "bazel-test.yml").read_text(encoding="utf-8")
        self.assertIn('if [ "$REPOSITORY" != "gastownhall/gascity" ]; then\n            mode=local', multi)
        self.assertIn("github.repository == 'gastownhall/gascity'", multi)
        for item in ("BAZEL_REMOTE_EXECUTOR:", "BAZEL_FORK_CACHE:", "BAZEL_FORK_REMOTE:"):
            line = next(line for line in side.splitlines() if item in line)
            self.assertIn("github.repository == 'gastownhall/gascity'", line)
        self.assertIn("if: github.repository == 'gastownhall/gascity' &&", side)

    def test_blacksmith_permission_workarounds_are_upstream_only(self) -> None:
        text = (Path(__file__).parents[1] / "bazel-test.yml").read_text(encoding="utf-8")
        self.assertEqual(text.count('if [ "$GITHUB_REPOSITORY" = "gastownhall/gascity" ]; then'), 2)
        self.assertNotIn("runs-on: blacksmith-", text)


if __name__ == "__main__":
    unittest.main()
