# Remaining dogfood hardening handoff

## Objective

Finish the two remaining dogfood hardening beads:

1. `ga-7xr` — align Thunderdome worktree creation names with guarded cleanup.
2. `ga-d55` — upgrade embedded Beads from schema v59 to v65 and remove `BD_IGNORE_SCHEMA_SKEW`.

Both beads are currently `in_progress`, assigned to Casey Matt. Their claim leases may need renewal or reclaiming in the next session.

## Completed baseline

### Gas City

Repository: `/home/exedev/workspace/gascity`

Branch: `latency-hardening`

Remote branch: `fork/latency-hardening`

Published commits include:

- `0db1c05e7` — slow convergence moved off the controller tick.
- `e98787949` — bounded work latency and trace fields.
- `583a0d650` — graph roots remain controller-owned.
- `c4c3a65f2` — final dogfood and Dolt recovery handoff.

The installed runtime reports:

```json
{"version":"1.4.0","commit":"583a0d650"}
```

The completed proof is documented in `engdocs/contributors/dogfood-latency-hardening-handoff.md`.

### Successful dogfood proof

- Build workflow: `spp-hvrt`.
- Landing workflow: `spp-r4xr`, closed pass.
- Candidate: `spp-ih1d`.
- Epoch: `spp-2oau`, promoted.
- Aggregate PR: `caseymatt/sprocket#45`.
- Release SHA: `dede16857cc5429f6f6370e55c2835fcd937e942`.
- Closed source beads: `spp-85gi`, `spp-ef0e`, and `spp-x1fc`.

Both remote refs resolve to the release SHA:

```text
refs/heads/dogfood/latency-proof-clean-20260820
refs/heads/dogfood/latency-proof-release-20260820
```

### Current city health

Managed GC-only recovery completed:

- `hq/.dolt`: 18 GB to 15 GB.
- `sp/.dolt`: 2.6 GB to 2.5 GB.
- `gc doctor`: `blocking_failed=0`, `failed=0`, `passed=103`.
- Five databases queryable.
- Zero Dolt orphans.

There is no off-box Dolt backup. Do not run history-flattening recovery without first adding backup capacity.

## `ga-7xr`: worktree naming

Repository: `/home/exedev/workspace/gascity-packs`

Branch: `codestorage/publish-before-teardown`

The branch was clean and pushed before this work. The following changes are now uncommitted:

```text
gascity/assets/workflows/thunderdome-land/assemble-land.md
gascity/assets/workflows/thunderdome-land/verify-repair.md
gascity/tests/test_worktree_cleanup.py
```

### Implemented change

The creation prompts now specify the names already authorized by `cleanup-worktree.sh`:

```text
$GC_RIG_ROOT/worktrees/thunderdome-epoch-<epoch-id>
$GC_RIG_ROOT/worktrees/verify-<epoch-id>-r<N>
```

`cleanup-worktree.sh` already accepts these names; no script change was required.

Tests were added to prove:

- a clean `thunderdome-epoch-sp-epoch` worktree is removed;
- a clean `verify-sp-epoch-r1` worktree is removed; and
- assemble and verification prompts contain the cleanup-contract names.

### Verification state

The focused suite passes:

```bash
cd /home/exedev/workspace/gascity-packs
python3 -m unittest gascity.tests.test_worktree_cleanup
```

Result: eight tests passed.

A larger 149-test run hit two existing two-second subprocess timeouts:

```text
test_city_claim_command_bounds_ambiguous_hook_failures_without_drain_ack
test_city_claim_command_preserves_unreadable_claim_after_bounded_retries
```

Both timed out in `gascity/commands/claim/run.sh`, including when rerun alone. They are unrelated to the worktree edits, but the next session must determine whether this is machine load or a current regression before publishing.

### Next steps for `ga-7xr`

1. Inspect `git diff` in `gascity-packs`.
2. Resolve or conclusively diagnose the two claim-test timeouts.
3. Run:

   ```bash
   python3 -m unittest \
     gascity.tests.test_worktree_cleanup \
     gascity.tests.test_thunderdome \
     gascity.tests.test_formula_assets
   ```

4. Commit and push the pack branch.
5. Update `/home/exedev/workspace/gc-city/city.toml` and `packs.lock` to the new pack SHA.
6. Run `gc import install`.
7. Verify the activated cached prompts contain the contract names.
8. Close `ga-7xr`.

The currently installed city pack pin is:

```text
79afcce99b08acba6b13b75d488f3cb2a6170316
```

## `ga-d55`: schema alignment

No source edits have been made yet. Investigation established the exact mismatch.

### Current dependency

`go.mod` currently uses:

```text
github.com/steveyegge/beads v1.1.1-0.20260805093327-bf97b73749ac
```

That version contains migrations through schema v59.

### Candidate dependency

Current Beads `main` resolves to:

```text
github.com/steveyegge/beads v1.1.1-0.20260820124357-349e4b029fd8
```

Commit:

```text
349e4b029fd8a5bdb8a2a1f0c1ce2dfc62dfc6f3
```

It contains migrations `0060` through `0065`, exactly matching the live databases' schema v65.

The module is already downloaded at:

```text
/home/exedev/go/pkg/mod/github.com/steveyegge/beads@v1.1.1-0.20260820124357-349e4b029fd8
```

### Relevant test surface

Primary integration file:

```text
internal/beads/native_dolt_store_integration_test.go
```

Notable existing test:

```text
TestNativeDoltStoreOpenPreservesMissingIDDefaults
```

Its comment currently says it verifies the v59 contract. Update that wording if the dependency upgrade changes the current schema contract.

Other required regression coverage includes:

```text
internal/beads/native_dolt_store_reconnect_test.go
internal/beads/native_dolt_store_conformance_test.go
internal/beads/native_dolt_store_metadata_cas_integration_test.go
```

### Required approach

1. Add a failing disposable-store integration test proving a v65 database opens without `BD_IGNORE_SCHEMA_SKEW`.
2. Upgrade the Beads dependency to `v1.1.1-0.20260820124357-349e4b029fd8`.
3. Resolve API changes without compatibility shims.
4. Exercise migration and opening against a disposable copy before touching live `hq`, `sp`, or `spp`.
5. Run native-store reconnect, schema, metadata-CAS, and conformance tests.
6. Run the repository's fast parallel test shards and `go vet`.
7. Build and install Gas City from the resulting clean code commit.
8. Restart the supervisor.
9. Remove the schema-skew override only after the new binary opens the disposable v65 store successfully.

The override is currently supplied through:

```text
/home/exedev/.config/systemd/user/gascity-supervisor.service.d/schema-skew.conf
```

10. Verify live commands without setting `BD_IGNORE_SCHEMA_SKEW`:

    ```bash
    gc doctor --json
    gc dolt health --json
    gc bd list --rig sprocket-proof --limit 1 --json
    gc bd show spp-r4xr --rig sprocket-proof --json
    ```

11. Require no schema-skew warning, no doctor error, all databases queryable, and no Dolt orphans.
12. Close `ga-d55`.

Do not modify the live schema manually. The live stores are already v65; the binary must be brought up to their supported schema version.

## Repository publication state

- `gascity`: clean and pushed before the new `ga-7xr` work.
- `gascity-packs`: contains the uncommitted `ga-7xr` changes listed above.
- `gc-city`: local commit `50fae48`; this repository has no Git remote and cannot be pushed.
- The generated `.beads.gate.lock` may reappear after `bd` commands. Remove only that generated lock before commits.

## Completion - 2026-08-21

Both original beads are closed:

- `ga-7xr` - Thunderdome worktree names now match the registered cleanup
  contract. The final pack regression covered 186 tests after publication.
- `ga-d55` - embedded Beads is at schema v65, the native-store regression
  surface passes, and the live city runs without `BD_IGNORE_SCHEMA_SKEW`.

Published revisions:

- Gas City `04cddf3` - hardened workflow recovery and native-store contracts.
- Gas City `b2e7093` - corrected the generated command-catalog regression
  assertion after the four new managed-worktree commands expanded the catalog.
- Gas City pack series:
  - `73bf9ba` - recovery, queue serialization, promotion, and lifecycle
    hardening.
  - `3e5bf8d` - one-time legacy source-provenance migration.
  - `f3f217f` - execute the sealed aggregate preflight command exactly.
  - `f17bb71` - bridge durable Code Storage publication to a guarded GitHub
    integration ref.
  - `2e77823` - merge the sealed candidates before running their aggregate
    preflight.
  - `c4ee06f` - checked private-repository promotion fallback when GitHub cannot
    expose branch protection.
  - `7aac697` - release reservations held by verified candidates whose epochs
    later fail or are cancelled.

The live autonomous proof completed through the production scheduler:

- source bead: `spp-donh`
- candidate: `spp-tdc-c91743764bf8`
- epoch: `spp-tde-b44237ae0db3`
- workflow root: `spp-55z2`
- aggregate PR: `https://github.com/caseymatt/sprocket/pull/48`
- landed, verified, and promoted SHA:
  `3901e43d623769aa16e2f3b56767cb22fd811957`
- release ref: `refs/heads/release/stable`
- configured GitHub check:
  `Canonical gate (core and clients)` passed in 8m51s
- source closure provenance:
  `Verified in Thunderdome epoch spp-tde-b44237ae0db3 at
  3901e43d623769aa16e2f3b56767cb22fd811957`

Final scheduler projection reports zero invariant violations, zero active
epochs, zero queued candidates, zero stale candidates, and
`action=none reason=no_ready_candidates` for a dry-run reconcile at the promoted
SHA. Every dogfood-created aggregate and verification worktree was published
through `gc worktree publish` before `gc worktree reclaim`; the two clean direct
source checkouts used by the synthetic proof were then removed.

The failed intermediate epochs were retained as evidence until their failure
classes exposed and drove the four prompt/state fixes above. Their worktrees
were not force-removed or discarded: each was first durably published through
Code Storage, then reclaimed through the central managed-worktree API.

Final verification after publication:

- `python3 -m unittest gascity.tests.test_worktree_cleanup
  gascity.tests.test_thunderdome gascity.tests.test_formula_assets`: 186 tests
  passed.
- The affected Gas City dispatch, events, graph, sling, and resource-census
  packages passed. Focused command regressions for metadata compare-and-set,
  formula fingerprints, and JSON schemas passed.
- The generated production command-catalog assertion passes at 202 entries;
  the publishing commit hook also passed affected lint and vet checks.
- Final `gc doctor`: 102 checks passed, 13 advisory warnings, zero failures.
