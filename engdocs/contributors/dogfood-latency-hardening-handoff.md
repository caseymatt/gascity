# Dogfood latency hardening handoff

**Status:** Implemented, published, installed, and proven on 2026-08-20.

## Problem

The Sprocket dogfood run exposed correctness and latency failures at the same boundary:

- controller ticks performed slow completion, order, store, and runtime work inline;
- capacity decisions lacked durable timestamps for separating queue delay, controller delay, and provider startup;
- malformed trace segments could make the evidence command fail instead of returning the valid prefix;
- a disconnected native Dolt store could be retried through a stale captured handle;
- worker hook claims could treat graph-v2 workflow roots as ordinary work; and
- parallel work could inherit a launcher directory instead of the selected source worktree.

The worst failure was semantic, not cosmetic: implementation workers claimed controller-owned workflow roots, skipped them as non-work, and left actual implementation steps stranded.

## Design

The controller remains the owner of graph workflow roots. Worker claim eligibility rejects graph roots at every claim tier: existing assignment, ready assignment, and fresh claim. Step and ordinary work beads remain claimable.

Slow convergence is split into bounded off-tick lanes. The fast tick computes desired state from a snapshot; completion and order work execute outside the tick with explicit time budgets. Capacity evaluation records queue readiness, capacity decision, provider-start request/completion, blockers, reason codes, outcome codes, session identity, workflow root, and step identity. Trace records are low-cardinality and omit prompt, source, and credential content.

Native Dolt recovery reopens the current store inside each retry instead of retrying a closure over the disconnected handle. Trace reads preserve valid records around malformed segments and report the malformed range.

Relevant commits on `latency-hardening`:

- `0db1c05e7` — move slow convergence off the controller tick;
- `e98787949` — bound work latency and expose queue/capacity/provider timestamps;
- `583a0d650` — keep graph roots controller-owned;
- `36aabfb3b`, `c7594bfd0`, `447eeff18` — discover, protect, and reap owned external worktrees.

The branch is rebased on current `origin/main` and published as `fork/latency-hardening`. The installed binary reports version `1.4.0`, commit `583a0d650`.

## Before and after

Both batches used three independent `thunderdome-work-item` roots in the same `sprocket-proof` rig.

| Measure | Before hardening | After hardening |
| --- | --- | --- |
| Roots | `spp-hai0`, `spp-upxu`, `spp-98gh` | `spp-thy8`, `spp-j3lg`, `spp-2d6x` |
| Batch window | 04:15:09–04:51:51 UTC: 36m42s | 05:01:02–05:22:41 UTC: 21m39s |
| Successful workflows | 0/3 | 3/3 |
| Worker behavior | implementation workers claimed workflow roots; roots closed skipped | implementation steps claimed by three distinct worker slots |
| Useful implementation commits | 0 | 3 |

The successful batch reduced observed wall time by 15m03s (41%) while changing the result from 0% to 100% successful. Its implementation attempts were claimed at 05:07:14, 05:08:14, and 05:09:45 UTC by pool slots 3, 2, and 1; they completed at 05:18:49, 05:19:20, and 05:22:21 UTC. The work overlapped instead of serializing or colliding.

This is a workflow-level comparison, not a microbenchmark. Agent reasoning and repository test duration remain in the measured wall time. Use `gc trace show --template <template> --json` for queue/capacity/provider attribution in future runs.

## End-to-end proof

Build workflow `spp-hvrt` produced candidate `spp-ih1d` from source beads `spp-85gi`, `spp-ef0e`, and `spp-x1fc`. Landing workflow `spp-r4xr` froze the candidate into epoch `spp-2oau`, merged aggregate PR `caseymatt/sprocket#45`, ran the canonical gate, and promoted release SHA `dede16857cc5429f6f6370e55c2835fcd937e942` to both:

- `refs/heads/dogfood/latency-proof-clean-20260820`
- `refs/heads/dogfood/latency-proof-release-20260820`

All three source beads closed with the verified epoch and release SHA. The landing root closed with `gc.outcome=pass`.

The proof also exercised:

- 232 aggregate Rust tests with zero failures;
- 16 focused relay terminal tests;
- TypeScript `tsc --noEmit`;
- the Chromium iPhone terminal smoke test; and
- GitHub's canonical PR gate, green in 7m34s.

Gas City verification completed with all `make test-fast-parallel` shards passing and focused `go vet` clean. The Gas City pack state-adapter regression suite contains 141 passing tests.

## Pack integration fixes discovered by the proof

The beads JSON wire returns structured metadata fields as JSON strings. Pack commit `3a2f0be` decodes and validates `candidate_ids`, `source_beads`, `repair_bead_ids`, and transition history at the adapter boundary. Pack commit `79afcce` allows a candidate recovered from a failed or cancelled epoch to point at its replacement epoch while preserving reverse-pointer checks for active and promoted epochs.

The dogfood city pins pack commit `79afcce99b08acba6b13b75d488f3cb2a6170316`.

## Residuals

- The existing Dolt database schema is v65 while the installed embedded beads schema is v59. The city intentionally sets `BD_IGNORE_SCHEMA_SKEW=1`; health and proof operations pass, but this remains explicit technical debt rather than a claim of schema equality.
- Cleanup removed all candidate/source worktrees. It preserved clean epoch worktrees named `epoch-spp-2oau` and `verify-spp-2oau` because those names do not match the pack's declared ownership forms `thunderdome-epoch-<epoch-id>` and `verify-<epoch-id>-r<N>`. Do not force-remove them; align creation names with the cleanup contract, then rerun guarded cleanup.
- No performance claim should be inferred for providers or models not exercised by this proof. The changes bound and expose controller latency; they do not promise model execution time.

## Resume commands

```bash
gc version --json
gc status --json
gc doctor --json
gc trace show --template sprocket-proof/gc.implementation-worker --since 24h --json
gc bd show spp-r4xr spp-2oau spp-ih1d --rig sprocket-proof --json
git -C /home/exedev/workspace/sprocket-gascity-proof ls-remote --heads origin \
  refs/heads/dogfood/latency-proof-clean-20260820 \
  refs/heads/dogfood/latency-proof-release-20260820
```
