from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[1] / "rc-gate.yml"
CI_WORKFLOW = Path(__file__).resolve().parents[1] / "ci.yml"
MAC_WORKFLOW = Path(__file__).resolve().parents[1] / "mac-regression.yml"
RELEASE_WORKFLOW = Path(__file__).resolve().parents[1] / "release.yml"
RC_RELEASE_WORKFLOW = Path(__file__).resolve().parents[1] / "rc-release.yml"
GITHUB_SCRIPT_ACTION = (
    "actions/github-script@ed597411d8f924073f98dfc5c65a23a2325f34cd"
)


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


def _step_block(workflow: str, step_name: str) -> str:
    marker = f"      - name: {step_name}\n"
    start = workflow.index(marker)
    lines = workflow[start:].splitlines(keepends=True)
    block = [lines[0]]
    for line in lines[1:]:
        if line.startswith("      - "):
            break
        block.append(line)
    return "".join(block)


def _workflow_call_input_block(workflow: str, input_name: str) -> str:
    marker = f"      {input_name}:\n"
    start = workflow.index(marker)
    lines = workflow[start:].splitlines(keepends=True)
    block = [lines[0]]
    for line in lines[1:]:
        if line.strip() and not line.startswith("        "):
            break
        block.append(line)
    return "".join(block)


def _job_permissions(job: str) -> str:
    start = job.index("    permissions:\n")
    end = job.index("    steps:\n", start)
    return job[start:end]


class RCGatePolicyTests(unittest.TestCase):
    def assert_exact_sha_guard(self, workflow: str, sha_expression: str) -> None:
        guard = _step_block(workflow, "Require exact-SHA RC Gate evidence")

        self.assertIn(f"uses: {GITHUB_SCRIPT_ACTION}", guard)
        self.assertIn(f"PUBLISH_SHA: {sha_expression}", guard)
        self.assertIn("workflow_id: 'rc-gate.yml'", guard)
        self.assertIn("head_sha: publishSha", guard)
        self.assertIn("event: 'workflow_dispatch'", guard)
        self.assertIn("status: 'success'", guard)
        self.assertIn("candidate.head_sha === publishSha", guard)
        self.assertIn("candidate.event === 'workflow_dispatch'", guard)
        self.assertIn("candidate.conclusion === 'success'", guard)
        self.assertIn("core.setFailed(", guard)
        self.assertIn("exact publish SHA", guard)
        self.assertIn("await core.summary", guard)
        self.assertIn("${run.id}", guard)
        self.assertIn("${run.html_url}", guard)

    def test_ci_parity_forces_the_full_deterministic_suite(self) -> None:
        rc_workflow = WORKFLOW.read_text()
        ci_workflow = CI_WORKFLOW.read_text()

        ci_parity = _job_block(rc_workflow, "ci_parity")
        self.assertIn("force_full_suite: true", ci_parity)

        force_input = _workflow_call_input_block(ci_workflow, "force_full_suite")
        self.assertIn("required: false", force_input)
        self.assertIn("type: boolean", force_input)
        self.assertIn("default: false", force_input)

        changes = _job_block(ci_workflow, "changes")
        for output in (
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
        ):
            self.assertIn(
                f"{output}: ${{{{ github.event_name == 'push' || "
                "inputs.force_full_suite ||",
                changes,
            )

        rest_full = _job_block(ci_workflow, "integration-rest-full")
        self.assertIn(
            "if: (github.event_name == 'push' || inputs.force_full_suite) "
            "&& needs.changes.outputs.integration == 'true'",
            rest_full,
        )

    def test_rc_summary_requires_every_direct_need_to_succeed(self) -> None:
        workflow = WORKFLOW.read_text()

        summary = _job_block(workflow, "rc_summary")
        self.assertIn("for job_id, meta in needs.items():", summary)
        self.assertIn('if result != "success":', summary)
        self.assertNotIn('{"success", "skipped"}', summary)
        self.assertIn("A skipped job fails this gate.", summary)

    def test_stable_release_requires_exact_sha_evidence_before_publish(self) -> None:
        workflow = RELEASE_WORKFLOW.read_text()
        release = _job_block(workflow, "release")
        resolver = _step_block(release, "Resolve publish commit")

        self.assertEqual(
            "    permissions:\n      contents: write\n      actions: read\n",
            _job_permissions(release),
        )
        self.assertIn('git rev-parse "${GITHUB_REF_NAME}^{commit}"', resolver)
        resolve_index = workflow.index("- name: Resolve publish commit")
        guard_index = workflow.index("- name: Require exact-SHA RC Gate evidence")
        self.assertLess(resolve_index, guard_index)
        self.assert_exact_sha_guard(workflow, "${{ steps.publish.outputs.sha }}")
        self.assertLess(
            workflow.index("- name: Require exact-SHA RC Gate evidence"),
            workflow.index("- name: Run GoReleaser"),
        )

    def test_rc_release_requires_exact_sha_evidence_before_tag_or_publish(
        self,
    ) -> None:
        workflow = RC_RELEASE_WORKFLOW.read_text()
        release = _job_block(workflow, "rc-release")
        resolver = _step_block(release, "Resolve RC release target")
        create_tag = _step_block(release, "Create RC tag")

        self.assertEqual(
            "    permissions:\n      contents: write\n      actions: read\n",
            _job_permissions(release),
        )
        self.assertIn("git rev-parse 'HEAD^{commit}'", resolver)
        self.assertIn('git rev-parse "refs/tags/$tag^{commit}"', resolver)
        self.assertNotIn("git tag -a", resolver)
        self.assertNotIn("git push", resolver)
        self.assertIn("git tag -a", create_tag)
        self.assertIn('git push origin "refs/tags/$TAG_NAME"', create_tag)
        self.assert_exact_sha_guard(workflow, "${{ steps.rc.outputs.commit }}")

        resolve_index = workflow.index("- name: Resolve RC release target")
        guard_index = workflow.index("- name: Require exact-SHA RC Gate evidence")
        create_index = workflow.index("- name: Create RC tag")
        publish_index = workflow.index("- name: Run GoReleaser draft prerelease")
        self.assertLess(resolve_index, guard_index)
        self.assertLess(guard_index, create_index)
        self.assertLess(guard_index, publish_index)

    def test_real_inference_jobs_are_throttled_after_ci_parity(self) -> None:
        workflow = WORKFLOW.read_text()

        acceptance_a = _job_block(workflow, "ubuntu_acceptance_a")
        self.assertIn("needs: ci_parity", acceptance_a)
        self.assertIn("max-parallel: 2", acceptance_a)

        acceptance_c = _job_block(workflow, "ubuntu_acceptance_c")
        self.assertIn("needs: ubuntu_acceptance_a", acceptance_c)
        self.assertIn("max-parallel: 2", acceptance_c)

        integration = _job_block(workflow, "ubuntu_integration_shards")
        self.assertIn("needs: ubuntu_acceptance_c", integration)
        self.assertIn("max-parallel: 8", integration)
        self.assertIn("shard_name: review-formulas-basic-2-of-2", integration)
        self.assertIn("timeout_minutes: 35", integration)

        tutorial = _job_block(workflow, "ubuntu_tutorial")
        self.assertIn("needs: ubuntu_integration_shards", tutorial)
        self.assertIn("max-parallel: 2", tutorial)

    def test_rc_gate_runs_full_mac_regression_workflow(self) -> None:
        workflow = WORKFLOW.read_text()

        self.assertNotIn("macos_fast_tests:", workflow)

        mac_regression = _job_block(workflow, "mac_regression")
        self.assertIn("uses: ./.github/workflows/mac-regression.yml", mac_regression)
        self.assertIn("suite: full", mac_regression)
        self.assertIn("force_blacksmith: true", mac_regression)

        summary = _job_block(workflow, "rc_summary")
        self.assertIn("- mac_regression", summary)

    def test_mac_regression_is_reusable_by_rc_gate(self) -> None:
        workflow = MAC_WORKFLOW.read_text()

        self.assertIn("workflow_call:", workflow)
        self.assertIn("force_blacksmith:", workflow)
        self.assertIn("FORCE_BLACKSMITH: ${{ inputs.force_blacksmith }}", workflow)


if __name__ == "__main__":
    unittest.main()
