#!/usr/bin/env python3
"""Contract tests for scripts/test-cadence."""

from __future__ import annotations

import contextlib
import copy
import importlib.machinery
import importlib.util
import io
import json
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
SCRIPT = REPO_ROOT / "scripts/test-cadence"
LOADER = importlib.machinery.SourceFileLoader("test_cadence_policy", str(SCRIPT))
SPEC = importlib.util.spec_from_loader(LOADER.name, LOADER)
assert SPEC is not None
CADENCE = importlib.util.module_from_spec(SPEC)
LOADER.exec_module(CADENCE)


class TestCadencePolicy(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        workflow_dir = self.root / ".github/workflows"
        workflow_dir.mkdir(parents=True)
        (workflow_dir / "ci.yml").write_text(
            """name: CI
jobs:
  small:
    steps:
      - run: make test-small
  medium:
    steps:
      - run: make test-medium
""",
            encoding="utf-8",
        )
        (workflow_dir / "nightly.yml").write_text(
            """name: Nightly
jobs:
  nightly:
    steps:
      - run: make test-nightly
""",
            encoding="utf-8",
        )
        (workflow_dir / "rc-gate.yml").write_text(
            """name: RC
jobs:
  rc:
    steps:
      - run: make test-rc
""",
            encoding="utf-8",
        )
        (workflow_dir / "release.yml").write_text(
            """name: Release
jobs:
  publish:
    steps:
      - run: make test-release
""",
            encoding="utf-8",
        )
        (workflow_dir / "rc-release.yml").write_text(
            """name: RC release
jobs:
  rc-publish:
    steps:
      - run: make test-rc-release
""",
            encoding="utf-8",
        )
        self.manifest = {
            "schema": 1,
            "shared_paths": [".github/workflows/**", "go.mod"],
            "suites": [
                self.suite(
                    "medium",
                    "medium",
                    "make test-medium",
                    ["pull_request", "push"],
                    "medium",
                    ["internal/worker/**"],
                ),
                self.suite(
                    "nightly",
                    "journey",
                    "make test-nightly",
                    ["nightly"],
                    "nightly",
                    ["test/acceptance/**"],
                ),
                self.suite(
                    "rc",
                    "journey",
                    "make test-rc",
                    ["rc"],
                    "rc",
                    ["**"],
                ),
                self.suite(
                    "release",
                    "journey",
                    "make test-release",
                    ["release"],
                    "publish",
                    ["**"],
                ),
                self.suite(
                    "small",
                    "small",
                    "make test-small",
                    ["pull_request", "push"],
                    "small",
                    ["cmd/**"],
                    tests=["TestSmall"],
                ),
            ],
        }
        self.manifest["suites"].sort(key=lambda suite: suite["id"])
        self.write_manifest()

    @staticmethod
    def suite(
        suite_id: str,
        suite_class: str,
        command: str,
        cadences: list[str],
        job: str,
        paths: list[str],
        *,
        tests: list[str] | None = None,
    ) -> dict[str, object]:
        row: dict[str, object] = {
            "id": suite_id,
            "class": suite_class,
            "command": command,
            "cadences": sorted(cadences),
            "required_on": sorted(cadences),
            "owner": "ga-test",
            "budget_minutes": 10,
            "paths": sorted(paths),
            "workflow_jobs": {event: job for event in sorted(cadences)},
        }
        if tests is not None:
            row["top_level_tests"] = tests
        return row

    def write_manifest(self) -> None:
        path = self.root / CADENCE.MANIFEST_PATH
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(self.manifest), encoding="utf-8")

    def validate(self, *, audit_orphans: bool = True):
        workflows = CADENCE.read_workflows(self.root)
        return CADENCE.validate_manifest(
            self.manifest, workflows, audit_orphans=audit_orphans
        )

    def assert_invalid(self, fragment: str, mutate) -> None:
        changed = copy.deepcopy(self.manifest)
        mutate(changed)
        workflows = CADENCE.read_workflows(self.root)
        with self.assertRaisesRegex(CADENCE.ManifestError, fragment):
            CADENCE.validate_manifest(changed, workflows)

    def test_positive_fixture_checks_workflow_bindings_and_commands(self) -> None:
        suites = self.validate()
        self.assertEqual(
            ["medium", "nightly", "rc", "release", "small"],
            [suite["id"] for suite in suites],
        )

    def test_rejects_schema_shape_and_suite_identity_errors(self) -> None:
        cases = [
            ("unknown field", lambda data: data.update({"extra": True})),
            ("schema must", lambda data: data.update({"schema": 2})),
            (
                "unknown field",
                lambda data: data["suites"][0].update({"extra": True}),
            ),
            (
                "class is unknown",
                lambda data: data["suites"][0].update({"class": "large"}),
            ),
            (
                "duplicate suite id",
                lambda data: data["suites"][1].update(
                    {"id": data["suites"][0]["id"]}
                ),
            ),
            (
                "positive integer",
                lambda data: data["suites"][0].update({"budget_minutes": 0}),
            ),
        ]
        for message, mutate in cases:
            with self.subTest(message=message):
                self.assert_invalid(message, mutate)

    def test_rejects_unknown_and_inconsistent_events(self) -> None:
        self.assert_invalid(
            "unknown event",
            lambda data: data["suites"][0].update(
                {
                    "cadences": ["hourly"],
                    "required_on": [],
                    "workflow_jobs": {"hourly": "medium"},
                }
            ),
        )
        self.assert_invalid(
            "outside cadences",
            lambda data: data["suites"][0].update(
                {"required_on": ["nightly", "pull_request"]}
            ),
        )
        self.assert_invalid(
            "map every cadence exactly",
            lambda data: data["suites"][0].update(
                {"workflow_jobs": {"pull_request": "medium"}}
            ),
        )

    def test_rejects_missing_jobs_command_drift_and_orphans(self) -> None:
        self.assert_invalid(
            "missing workflow job",
            lambda data: data["suites"][0]["workflow_jobs"].update(
                {"pull_request": "missing", "push": "missing"}
            ),
        )
        self.assert_invalid(
            "command is not present",
            lambda data: data["suites"][0].update(
                {"command": "make test-renamed"}
            ),
        )
        ci = self.root / ".github/workflows/ci.yml"
        ci.write_text(
            ci.read_text(encoding="utf-8")
            + "  orphan:\n    steps:\n      - run: make test-orphan\n",
            encoding="utf-8",
        )
        with self.assertRaisesRegex(CADENCE.ManifestError, "orphaned test command"):
            self.validate()

    def test_top_level_test_names_are_sorted_unique_and_go_shaped(self) -> None:
        self.assert_invalid(
            "must be sorted",
            lambda data: data["suites"][-1].update(
                {"top_level_tests": ["TestZulu", "TestAlpha"]}
            ),
        )
        self.assert_invalid(
            "must not contain duplicates",
            lambda data: data["suites"][-1].update(
                {"top_level_tests": ["TestAlpha", "TestAlpha"]}
            ),
        )
        self.assert_invalid(
            "invalid Go test name",
            lambda data: data["suites"][-1].update(
                {"top_level_tests": ["BenchmarkSmall"]}
            ),
        )

    def test_plan_event_truth_table(self) -> None:
        suites = self.validate()
        cases = [
            ("pull_request", [], ["small"], False),
            ("pull_request", ["docs/readme.txt"], ["small"], False),
            (
                "pull_request",
                ["internal/worker/run.go"],
                ["medium", "small"],
                False,
            ),
            ("pull_request", ["go.mod"], ["medium", "small"], True),
            ("push", [], ["medium", "small"], True),
            ("nightly", [], ["nightly"], True),
            ("rc", [], ["rc"], True),
            ("release", [], ["release"], True),
        ]
        for event, changed, expected, full in cases:
            with self.subTest(event=event, changed=changed):
                result = CADENCE.plan(self.manifest, suites, event, changed)
                self.assertEqual(expected, result["suite_ids"])
                self.assertEqual(full, result["full_suite"])
                self.assertEqual(sorted(result["workflow_jobs"]), result["workflow_jobs"])

    def test_plan_deduplicates_and_sorts_changed_files(self) -> None:
        suites = self.validate()
        result = CADENCE.plan(
            self.manifest,
            suites,
            "pull_request",
            ["internal/worker/z.go", "cmd/a.go", "internal/worker/z.go"],
        )
        self.assertEqual(["cmd/a.go", "internal/worker/z.go"], result["changed_files"])
        self.assertEqual(["medium", "small"], result["suite_ids"])

    def test_cli_parser_rejects_unknown_event(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            CADENCE.parse_args(["plan", "--event", "hourly"])

    def test_json_loader_rejects_duplicate_fields(self) -> None:
        path = self.root / CADENCE.MANIFEST_PATH
        path.write_text(
            '{"schema": 1, "schema": 1, "shared_paths": ["**"], "suites": []}',
            encoding="utf-8",
        )
        with self.assertRaisesRegex(CADENCE.ManifestError, "duplicate JSON field"):
            CADENCE.load_manifest(self.root)


if __name__ == "__main__":
    unittest.main()
