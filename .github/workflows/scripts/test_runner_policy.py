import tempfile
import unittest
from pathlib import Path

import runner_policy

NIGHTLY_WORKFLOW = Path(__file__).resolve().parents[1] / "nightly.yml"
MAKEFILE = Path(__file__).resolve().parents[3] / "Makefile"


def _job_block(workflow: str, job_name: str) -> str:
    marker = f"  {job_name}:\n"
    start = workflow.index(marker)
    lines = workflow[start:].splitlines(keepends=True)
    block = [lines[0]]
    for line in lines[1:]:
        if line.startswith("  ") and not line.startswith("    ") and line.strip().endswith(":"):
            break
        block.append(line)
    return "".join(block)


class RunnerPolicyTests(unittest.TestCase):
    def test_load_allowlist_ignores_comments_and_case_normalizes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "allowlist.txt"
            path.write_text(
                "julianknutsen\n"
                "  Csells  # maintainer\n"
                "\n"
                "# comment\n",
                encoding="utf-8",
            )

            self.assertEqual(runner_policy.load_allowlist(path), {"julianknutsen", "csells"})

    def test_pull_request_from_allowlisted_author_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "Quad341",
            {"quad341"},
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("allowlist", reason)
        self.assertEqual(runners["runner_32vcpu"], "blacksmith-32vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_push_uses_github_even_for_allowlisted_author(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "push",
            "julianknutsen",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("approved pull requests", reason)
        self.assertEqual(runners["runner_32vcpu"], "ubuntu-latest")

    def test_forced_workflow_call_uses_blacksmith(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "workflow_call",
            "",
            set(),
            force_blacksmith=True,
        )

        self.assertTrue(use_blacksmith)
        self.assertIn("forced", reason)
        self.assertEqual(runners["runner_16vcpu"], "blacksmith-16vcpu-ubuntu-2404")
        self.assertEqual(runners["runner_macos"], "blacksmith-12vcpu-macos-15")

    def test_unlisted_pull_request_author_uses_github(self) -> None:
        use_blacksmith, reason, runners = runner_policy.select_runners(
            "pull_request",
            "external-contributor",
            {"julianknutsen"},
            force_blacksmith=False,
        )

        self.assertFalse(use_blacksmith)
        self.assertIn("not on the Blacksmith allowlist", reason)
        self.assertEqual(runners["runner_macos"], "macos-15")

    def test_nightly_reusable_ci_forces_the_deterministic_full_union(self) -> None:
        workflow = NIGHTLY_WORKFLOW.read_text(encoding="utf-8")
        deterministic_full = _job_block(workflow, "deterministic-full")

        self.assertIn("uses: ./.github/workflows/ci.yml", deterministic_full)
        self.assertIn("force_full_suite: true", deterministic_full)
        self.assertIn("permissions:\n      contents: read", deterministic_full)
        self.assertIn("secrets: inherit", deterministic_full)

    def test_nightly_expensive_jobs_are_bounded_and_run_the_owned_suites(self) -> None:
        workflow = NIGHTLY_WORKFLOW.read_text(encoding="utf-8")
        expected_jobs = {
            "race": (
                "90",
                "actions/setup-go@4a3601121dd01d1626a1e23e37211e3254c1c06c",
                "make test-race",
            ),
            "dolt-chaos": (
                "60",
                "uses: ./.github/actions/setup-gascity-ubuntu",
                "make test-chaos-dolt",
            ),
            "formula-recovery": (
                "45",
                "uses: ./.github/actions/setup-gascity-ubuntu",
                "make test-integration-review-formulas-recovery",
            ),
        }

        for job_name, (timeout, setup_action, command) in expected_jobs.items():
            with self.subTest(job=job_name):
                job = _job_block(workflow, job_name)
                self.assertIn(f"timeout-minutes: {timeout}", job)
                self.assertIn("permissions:\n      contents: read", job)
                self.assertIn(
                    "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
                    job,
                )
                self.assertIn(setup_action, job)
                self.assertIn(f"run: {command}", job)

    def test_race_target_uses_the_scrubbed_fast_package_contract(self) -> None:
        makefile = MAKEFILE.read_text(encoding="utf-8")

        self.assertIn(
            '$(TEST_ENV) GOFLAGS="$(QUALITY_GATE_GOFLAGS)" GC_FAST_UNIT=1 '
            "scripts/go-test-observable test-race -- "
            "-race -p=4 -count=1 -timeout 30m ./...",
            makefile,
        )


if __name__ == "__main__":
    unittest.main()
