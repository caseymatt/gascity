import contextlib
import io
import json
import os
from pathlib import Path
import re
import tempfile
import unittest
from unittest import mock

import ci_suite_coverage as cov

CI_WORKFLOW = Path(__file__).resolve().parents[1] / "ci.yml"
CADENCE_MANIFEST = CI_WORKFLOW.parents[2] / ".github" / "test-cadence.json"
PATH_GATED_OUTPUTS = (
    "mail",
    "docker",
    "k8s",
    "beads",
    "packs",
    "worker",
    "worker_phase2",
    "cmd_gc_process",
    "credential_provider",
    "integration",
    "openclaw_bridge",
)

PATH_FILTER_BY_JOB = {
    "cmd-gc-process": "cmd_gc_process",
    "cmd-gc-productmetrics-testhook": "cmd_gc_process",
    "contract-acceptance-current": "beads",
    "contract-radar-bd-head": "beads",
    "credential-provider-windows": "credential_provider",
    "docker-session": "docker",
    "integration-shards": "integration",
    "k8s-session": "k8s",
    "mcp-mail": "mail",
    "openclaw-bridge": "openclaw_bridge",
    "pack-gate": "packs",
}
PATH_FILTER_BY_JOB_PREFIX = (
    ("worker-core-phase2-", "worker_phase2"),
    ("worker-core-", "worker"),
)


class ClassifyModeTests(unittest.TestCase):
    def test_shared_match_is_full(self) -> None:
        self.assertEqual(cov.classify_mode(True), cov.FULL)

    def test_no_shared_match_is_filtered(self) -> None:
        self.assertEqual(cov.classify_mode(False), cov.FILTERED)

    def test_main_push_is_full_with_reason(self) -> None:
        self.assertEqual(cov.classify_mode(False, "push", False), cov.FULL)
        self.assertEqual(
            cov.classify_reason(False, "push", False),
            cov.REASON_MAIN_PUSH,
        )

    def test_forced_reusable_call_is_full_with_reason(self) -> None:
        self.assertEqual(cov.classify_mode(False, "workflow_call", True), cov.FULL)
        self.assertEqual(
            cov.classify_reason(False, "workflow_call", True),
            cov.REASON_FORCED,
        )

    def test_shared_pull_request_has_shared_path_reason(self) -> None:
        self.assertEqual(
            cov.classify_reason(True, "pull_request", False),
            cov.REASON_SHARED_PATH,
        )

    def test_filtered_pull_request_has_filtered_reason(self) -> None:
        self.assertEqual(
            cov.classify_reason(False, "pull_request", False),
            cov.REASON_PATH_FILTERED,
        )

    def test_force_reason_takes_precedence(self) -> None:
        self.assertEqual(
            cov.classify_reason(True, "push", True),
            cov.REASON_FORCED,
        )


class SelectionPolicyTests(unittest.TestCase):
    def test_ordinary_pull_request_is_path_filtered(self) -> None:
        self.assertTrue(
            cov.path_suite_selected(True, False, "pull_request", False)
        )
        self.assertFalse(
            cov.path_suite_selected(False, False, "pull_request", False)
        )

    def test_shared_path_selects_full_pull_request_union(self) -> None:
        for path_matched in (False, True):
            self.assertTrue(
                cov.path_suite_selected(
                    path_matched,
                    True,
                    "pull_request",
                    False,
                )
            )

    def test_main_push_selects_full_union(self) -> None:
        self.assertTrue(cov.path_suite_selected(False, False, "push", False))

    def test_forced_reusable_call_selects_full_union(self) -> None:
        self.assertTrue(
            cov.path_suite_selected(False, False, "workflow_call", True)
        )

    def test_full_rest_is_limited_to_push_or_forced_union(self) -> None:
        self.assertFalse(cov.full_rest_selected(True, "pull_request", False))
        self.assertTrue(cov.full_rest_selected(True, "push", False))
        self.assertTrue(cov.full_rest_selected(True, "workflow_call", True))
        self.assertFalse(cov.full_rest_selected(False, "push", False))


class WorkflowWiringTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.workflow = CI_WORKFLOW.read_text(encoding="utf-8")
        cls.manifest = json.loads(CADENCE_MANIFEST.read_text(encoding="utf-8"))

    def filter_paths(self, name: str) -> set[str]:
        match = re.search(
            rf"^            {re.escape(name)}:\n"
            rf"(?P<body>(?:^              - '[^']+'\n)+)",
            self.workflow,
            flags=re.MULTILINE,
        )
        self.assertIsNotNone(match, f"missing path filter {name}")
        return set(re.findall(r"^              - '([^']+)'$", match.group("body"), re.MULTILINE))

    def test_force_full_suite_is_optional_false_boolean_input(self) -> None:
        input_match = re.search(
            r"^      force_full_suite:\n"
            r"(?:^        .*\n)*?"
            r"^        required: false\n"
            r"^        type: boolean\n"
            r"^        default: false$",
            self.workflow,
            flags=re.MULTILINE,
        )
        self.assertIsNotNone(input_match)

    def test_every_path_gated_output_has_all_union_sources(self) -> None:
        for output in PATH_GATED_OUTPUTS:
            match = re.search(
                rf"^      {re.escape(output)}: "
                rf"\$\{{\{{ (?P<expression>.+) \}}\}}$",
                self.workflow,
                flags=re.MULTILINE,
            )
            self.assertIsNotNone(match, f"missing changes output {output}")
            expression = match.group("expression")
            self.assertIn("github.event_name == 'push'", expression, output)
            self.assertIn("inputs.force_full_suite", expression, output)
            self.assertIn(
                f"steps.filter.outputs.{output} == 'true'",
                expression,
                output,
            )
            self.assertIn(
                "steps.filter.outputs.shared == 'true'",
                expression,
                output,
            )

    def test_shared_filter_exactly_matches_manifest_authority(self) -> None:
        self.assertEqual(
            self.filter_paths("shared"),
            set(self.manifest["shared_paths"]),
        )

    def test_path_gated_filters_cover_manifest_paths(self) -> None:
        shared = self.filter_paths("shared")
        for suite in self.manifest["suites"]:
            job = suite.get("workflow_jobs", {}).get("pull_request", "")
            filter_name = PATH_FILTER_BY_JOB.get(job)
            if filter_name is None:
                filter_name = next(
                    (
                        candidate
                        for prefix, candidate in PATH_FILTER_BY_JOB_PREFIX
                        if job.startswith(prefix)
                    ),
                    None,
                )
            if filter_name is None:
                continue
            with self.subTest(suite=suite["id"], filter=filter_name):
                self.assertLessEqual(
                    set(suite["paths"]),
                    self.filter_paths(filter_name) | shared,
                )

    def test_full_rest_job_accepts_only_push_or_forced_full_union(self) -> None:
        rest_job = re.search(
            r"^  integration-rest-full:\n(?P<body>.*?)(?=^  [a-zA-Z0-9_-]+:|\Z)",
            self.workflow,
            flags=re.MULTILINE | re.DOTALL,
        )
        self.assertIsNotNone(rest_job)
        self.assertIn(
            "if: (github.event_name == 'push' || inputs.force_full_suite) "
            "&& needs.changes.outputs.integration == 'true'",
            rest_job.group("body"),
        )

    def test_classification_receives_event_and_force_and_exports_reason(self) -> None:
        self.assertIn("suite_reason: ${{ steps.coverage.outputs.suite_reason }}", self.workflow)
        self.assertIn('"$SHARED_FILTER_RESULT" "$EVENT_NAME" "$FORCE_FULL_SUITE"', self.workflow)


class ClassificationEmissionTests(unittest.TestCase):
    def test_forced_reason_is_recorded_without_changing_mode_token(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_path = Path(directory) / "output"
            summary_path = Path(directory) / "summary"
            environment = {
                "GITHUB_OUTPUT": os.fspath(output_path),
                "GITHUB_STEP_SUMMARY": os.fspath(summary_path),
            }
            notice = io.StringIO()
            with mock.patch.dict(os.environ, environment), contextlib.redirect_stdout(notice):
                cov._emit_classification("false", "workflow_call", "true")

            self.assertEqual(
                output_path.read_text(encoding="utf-8"),
                "suite_mode=full\nsuite_reason=forced\n",
            )
            summary = summary_path.read_text(encoding="utf-8")
            self.assertIn("- mode: `full`", summary)
            self.assertIn("- reason: `forced`", summary)
            self.assertIn("ci_suite_mode=full", notice.getvalue())
            self.assertIn("ci_suite_reason=forced", notice.getvalue())


class PathsMatchTests(unittest.TestCase):
    def test_directory_glob_matches_nested_file(self) -> None:
        self.assertTrue(cov.paths_match(["internal/beads/store.go"], ["internal/beads/**"]))

    def test_directory_glob_does_not_match_sibling(self) -> None:
        self.assertFalse(cov.paths_match(["internal/beadsx/store.go"], ["internal/beads/**"]))

    def test_suffix_glob_matches_any_go_file(self) -> None:
        self.assertTrue(cov.paths_match(["cmd/gc/main.go"], ["**/*.go"]))

    def test_literal_path_matches_exactly(self) -> None:
        self.assertTrue(cov.paths_match(["go.mod"], ["go.mod"]))
        self.assertFalse(cov.paths_match(["go.sum"], ["go.mod"]))

    def test_trailing_wildcard_matches_within_segment(self) -> None:
        # `cmd/gc/session_*` and `contrib/session-scripts/gc-session-k8s*`
        self.assertTrue(cov.paths_match(["cmd/gc/session_pool.go"], ["cmd/gc/session_*"]))
        self.assertTrue(
            cov.paths_match(
                ["contrib/session-scripts/gc-session-k8s-runner"],
                ["contrib/session-scripts/gc-session-k8s*"],
            )
        )

    def test_trailing_wildcard_does_not_cross_slash(self) -> None:
        # `*` must not match a path separator, mirroring picomatch/dorny.
        self.assertFalse(cov.paths_match(["cmd/gc/session_sub/extra.go"], ["cmd/gc/session_*"]))

    def test_mid_path_wildcard_with_suffix(self) -> None:
        # `cmd/gc/template_resolve*.go`
        self.assertTrue(
            cov.paths_match(
                ["cmd/gc/template_resolve_t3bridge.go"],
                ["cmd/gc/template_resolve*.go"],
            )
        )
        self.assertFalse(
            cov.paths_match(
                ["cmd/gc/template_resolve_t3bridge.txt"], ["cmd/gc/template_resolve*.go"]
            )
        )

    def test_embedded_globstar(self) -> None:
        # `test/**worker**` matches any test path containing "worker".
        self.assertTrue(
            cov.paths_match(["test/integration/session_worker_test.go"], ["test/**worker**"])
        )
        self.assertFalse(cov.paths_match(["test/integration/mail_test.go"], ["test/**worker**"]))

    def test_root_file_matches_leading_globstar_suffix(self) -> None:
        # `**/*.go` must match a repo-root file, not only nested ones.
        self.assertTrue(cov.paths_match(["main.go"], ["**/*.go"]))

    def test_matcher_handles_supported_single_star_shapes(self) -> None:
        samples = {
            "cmd/gc/template_resolve*.go": "cmd/gc/template_resolve_t3bridge.go",
            "cmd/gc/session_*": "cmd/gc/session_pool.go",
            "contrib/session-scripts/gc-session-k8s*": (
                "contrib/session-scripts/gc-session-k8s-runner"
            ),
            "test/**worker**": "test/integration/session_worker_test.go",
        }
        for glob, sample in samples.items():
            self.assertTrue(
                cov.paths_match([sample], [glob]),
                f"matcher fails to match {sample!r} against {glob!r}",
            )


class AggregateTests(unittest.TestCase):
    def test_percentages(self) -> None:
        result = cov.aggregate([cov.FULL, cov.FILTERED, cov.FILTERED, cov.FULL])
        self.assertEqual(result["total"], 4)
        self.assertEqual(result["full"], 2)
        self.assertEqual(result["filtered"], 2)
        self.assertEqual(result["full_pct"], 50.0)
        self.assertEqual(result["filtered_pct"], 50.0)

    def test_empty_is_zero_not_division_error(self) -> None:
        result = cov.aggregate([])
        self.assertEqual(result["total"], 0)
        self.assertEqual(result["full_pct"], 0.0)

    def test_unknown_tokens_counted_separately(self) -> None:
        result = cov.aggregate([cov.FULL, "weird"])
        self.assertEqual(result["unknown"], 1)


if __name__ == "__main__":
    unittest.main()
