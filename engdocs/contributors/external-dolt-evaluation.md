# External Dolt evaluation and migration test plan

**Status:** operational recommendation, not proof that an external deployment is faster
**Incident examined:** bounded three-item Sprocket formulas v2 run, 2026-08-19
**Related work:** `gc-um4u04` (orchestration latency), `ga-45n` (this document)

## Decision

A nearby, dedicated external Dolt SQL server is the strongest operational mitigation for the local resource-contention failure observed during the run. It would move Dolt's CPU, memory, disk I/O, journal writes, garbage collection, and storage capacity away from the agent build machine.

It is not a complete fix by itself. The same query load, transaction conflicts, connection limits, storage growth, or server failure can produce timeouts against an external endpoint. Network round-trip time and another service dependency can also make a healthy remote server slower than a healthy local server.

Do not migrate on this conclusion alone. Provision a representative external endpoint, copy the complete ledger, and run the controlled A/B test in this document. Promote the endpoint only if it removes the storage failures without regressing normal bead and orchestration latency.

## What “remote Dolt” means

Gas City needs a live MySQL-compatible **Dolt SQL endpoint** for bead reads and writes.

These mechanisms are different:

| Mechanism | Purpose | Changes live Gas City traffic? |
|---|---|---:|
| External Dolt SQL server or hosted beads gateway | Serves the live `issues`, dependency, label, event, and metadata tables | Yes |
| `dolt remote` | Exchanges repository commits with another repository | No |
| `dolt backup` | Creates or synchronizes a recoverable copy | No |
| File copy of `.beads/dolt` | Offline migration or recovery input | No, until a SQL server serves it and Gas City points at that server |

Adding a Git-style Dolt remote or backup target does not address the incident. The city must change from its managed-local endpoint to a live external endpoint.

## Incident context

The proof ran three independent Sprocket implementation items through formulas v2 with a configured implementation-worker pool maximum of three. It completed naturally, but took **3 h 22 m 13 s** from convoy creation to finalization.

### Observed orchestration timing

| Measurement | Observed value |
|---|---:|
| Worker wait before claim/start | 20 m 26 s / 1 h 18 m 16 s / 1 h 35 m 25 s |
| Completed orchestrator cycles | 324 |
| Cycle duration p50 / p95 / p99 / max | 13.993 s / 115.019 s / 213.110 s / 272.152 s |
| Cycles over 30 s / 60 s / 120 s | 107 / 46 / 14 |
| `demand_snapshot.load` p95 / max | 23.886 s / 194.165 s |
| `bead_reconcile_tick` p95 / max | 50.879 s / 248.308 s |
| Artifact retry-tail critical-path cost | approximately 20 m 43 s |

The build workload was also expensive and poorly scheduled. Concurrent Cargo processes contended on target directories and locks, and some agents created cold targets on the root filesystem. That work explains part of the total duration independently of Dolt.

### Observed storage failures

Within the workflow window, the supervisor recorded:

- **865** MySQL I/O timeouts;
- **594** unexpected EOFs;
- **92** Dolt circuit-breaker-open failures.

Representative connections were loopback connections to the managed server:

```text
read tcp 127.0.0.1:<client-port>->127.0.0.1:15615: i/o timeout
[mysql] packets.go:58 unexpected EOF
dolt circuit breaker is open: server appears down, failing fast
```

This rules out Internet packet loss for those samples. The local Dolt process was not servicing or retaining connections reliably.

The same investigation found substantial local disk pressure: the managed Dolt directory was approximately 22 GB, build worktrees and targets were much larger, and the root filesystem reached 88–96% utilization while compilation was active. An earlier 2026-08-13 incident produced explicit `journal.idx: no space left on device` errors. That earlier ENOSPC event is supporting evidence that the local topology is vulnerable to disk pressure; it is not proof that every 2026-08-19 timeout was caused by ENOSPC.

### Consequence for formula execution

The artifact validator called `gc bd show` to resolve attempt state. Store failures were flattened into generic validation failures, so the formula consumed bounded semantic retries even though the artifact itself was not invalid. Those retries added approximately 51 m 34 s of aggregate agent time across item and aggregate stages.

Moving Dolt can prevent this failure chain only when the external server stays available:

```text
local build/storage pressure
  -> Dolt timeout or dropped connection
  -> client circuit breaker opens
  -> bead lookup fails
  -> infrastructure failure is misclassified as artifact failure
  -> bounded semantic retry is consumed
```

The artifact-failure classification remains a separate bug and must be fixed even if the database moves.

## What an external endpoint changes

A managed city runs one local Dolt SQL server with a logical database for the city and each inherited rig. An external city endpoint changes the endpoint origin to `city_canonical`; inherited rigs then use that endpoint.

When Gas City resolves an external endpoint, `startBeadsLifecycle` does not spawn, adopt, restart, or stop the local managed Dolt process. It connects to the external service instead. See:

- `cmd/gc/beads_provider_lifecycle.go`, `startBeadsLifecycle`;
- `cmd/gc/cmd_beads_city.go`, `cmdBeadsCityUseExternal` and endpoint validation;
- `cmd/gc/cmd_rig_endpoint.go`, `verifyExternalDoltEndpoint`;
- `internal/beads/native_dolt_store.go`, transient read reconnect handling;
- `docs/runbooks/managed-city-endpoints.md`, endpoint ownership model.

### Failure modes likely removed

A dedicated database host isolates the store from:

- local Cargo CPU and memory pressure;
- build-target and worktree disk usage;
- local filesystem ENOSPC;
- build I/O competing with Dolt journal, chunk, backup, and GC I/O;
- accidental cleanup of local database runtime files;
- a single machine failure taking down both agents and the ledger.

The circuit-breaker errors are downstream. They should disappear when the timeout and connection-closure errors disappear.

### Failure modes that remain

An external endpoint does not change:

- the number or shape of queries issued by demand snapshots and reconciliation;
- transaction conflict behavior;
- inefficient scans or missing indexes;
- Dolt history growth and garbage-collection requirements;
- connection-pool exhaustion or server connection limits;
- artifact validation's infrastructure-versus-semantic failure classification;
- duplicated Rust verification and Cargo lock contention;
- worker-pool occupancy by unrelated work.

The native store recognizes I/O timeout, unexpected EOF, broken pipe, failed dial, and closed-connection errors as transient reconnect cases. Those errors can occur against either topology. A remote outage can therefore recreate the same circuit-breaker and retry behavior.

### New risks

| Risk | Control |
|---|---|
| Network RTT on every transaction | Place the endpoint in the same region or LAN; measure p50, p95, and p99 before cutover |
| Network or DNS outage | Redundant network path, monitored DNS, bounded connection timeouts |
| TLS or credential failure | Validate using the same credential source and identity checks Gas City will use |
| Provider connection limits | Size for orchestrator, dashboard, CLI, agents, maintenance, and retry bursts |
| Split-brain writes during migration | Quiesce all writers and permit exactly one authoritative endpoint |
| Incorrect logical database mapping | Inventory and validate every city and rig database before cutover |
| Lost or mismatched project identity | Preserve `_project_id`; use verified cutover rather than bypassing validation |
| Remote storage growth | Monitor bytes, live rows, history, GC duration, and free space on the remote host |
| Service ownership gap | Assign backups, upgrades, GC, health alerts, and incident response to an operator or provider |

A remote endpoint over a high-latency WAN can be reliable and still make orchestration slower. For this workload, prefer a measured steady-state RTT below 2 ms. Treat that number as a deployment target, not a Gas City protocol requirement.

## Migration invariants

The cutover is safe only when all of these hold:

1. **One writer topology.** The managed and external servers are never both authoritative.
2. **Complete scope inventory.** Copy the city database and every inherited rig database. At the time of the investigation these were `hq`, `ga`, `smc`, `sp`, and `spp`; re-query the live catalog because the set can change.
3. **Stable database names.** Each scope's pinned `dolt_database` still names the database served by the target.
4. **Stable project identity.** Each target database preserves the expected `metadata._project_id` and each scope's canonical local identity.
5. **Complete history.** Transfer branches, commits, working-set state, and table data—not only an `issues` export.
6. **Verified credentials.** The orchestrator, CLI, dashboard, and agents can all obtain credentials without embedding passwords in `city.toml` or shell history.
7. **Recoverable source.** Keep an immutable pre-cutover backup and leave the old managed data directory untouched until the acceptance window ends.
8. **Measured rollback point.** Record whether any writes have reached the external endpoint. That determines whether rollback requires reverse synchronization.

`gc beads city use-external` changes and validates endpoint topology. It does **not** copy databases. Provisioning and data transfer must finish first.

## Controlled migration procedure

Replace angle-bracketed values before running commands. Keep credentials out of the transcript and use the configured credential store.

### 1. Record the baseline

From the city root:

```sh
gc version
gc status --json
gc doctor --json
gc dolt health --json
gc dolt list
```

Record:

- city path and config revision;
- managed endpoint host and port;
- every logical database and its scope;
- Dolt version;
- database size, commit count, open-bead count, and latest commit;
- root filesystem free space;
- worker pool configuration;
- active sessions and assigned work;
- the exact formula, convoy input, source revisions, and bead IDs used for the comparison run.

Save trace and supervisor-log boundaries before starting the benchmark. Trace extraction must fail visibly if a segment is malformed; do not silently omit the gap.

### 2. Quiesce writes and take a final backup

Stop formula launches, orders, agents, dashboard mutations, and direct `bd` clients. Then stop the city:

```sh
gc stop
```

Confirm that no Gas City process is writing to the managed endpoint. Take a final, provider-supported Dolt backup or repository transfer for **every** database. Record the backup URI, database name, branch, commit hash, byte size, and completion time.

Do not delete, rename, compact, or reuse the managed data directory during the acceptance window.

### 3. Provision and restore the external endpoint

Provision the target close to the Gas City host with:

- SSD-backed durable storage and explicit free-space alerts;
- sufficient CPU, memory, and connection capacity;
- authenticated MySQL-compatible Dolt access;
- TLS when traffic leaves a trusted private network;
- automated backups with a tested restore path;
- GC and storage-retention ownership;
- server logs and query/connection metrics.

Restore each logical database using the target provider's supported Dolt migration mechanism. The transfer procedure is provider-specific; it must preserve Dolt history and working-set state. Do not substitute JSONL export/import for a full database transfer unless loss of history and metadata has been explicitly accepted.

### 4. Validate the target out of band

Before changing Gas City, validate each database directly through the external SQL endpoint:

```sql
SELECT active_branch();
SHOW TABLES LIKE 'issues';
SELECT value FROM metadata WHERE `key` = '_project_id';
SELECT COUNT(*) FROM issues;
```

For each scope, compare the target with the quiesced source:

- expected database name;
- active branch;
- latest commit hash;
- `_project_id`;
- issue count;
- open-bead count;
- dependency and label counts where available.

A count match is necessary but not sufficient. Identity and commit mismatches block cutover.

### 5. Preview the topology change

```sh
gc beads city use-external \
  --host <external-host> \
  --port <external-port> \
  --user <external-user> \
  --dry-run
```

Review the city endpoint and every inherited rig mirror. Explicit rig endpoints should remain explicit.

Do not use `--adopt-unverified` for the production cutover. That flag records an endpoint without proving connectivity, tables, or project identity; it is appropriate only when intentionally staging unreachable infrastructure.

### 6. Cut over and restart

```sh
gc beads city use-external \
  --host <external-host> \
  --port <external-port> \
  --user <external-user>

gc start
```

The verified command checks connectivity, confirms that the database is Dolt, requires the `issues` table, and compares the local canonical project identity with the database `_project_id`. A failure is a blocked cutover, not a reason to bypass validation.

### 7. Run immediate smoke checks

```sh
gc dolt health --json
gc status --json
gc doctor --json
gc bd list --all --json
gc events --since 10m
```

Check that:

- health reports the intended external endpoint;
- all expected scopes are readable;
- no process starts or adopts the old managed-local server;
- no endpoint-drift or project-identity warning appears;
- no MySQL timeout, unexpected EOF, circuit-open, TLS, authentication, or unknown-database error appears.

From the city and each rig scope, exercise one controlled bead lifecycle using an ephemeral bead:

```sh
bd create --ephemeral --title "external Dolt cutover probe" --type task --json
bd show <probe-id> --json
bd close <probe-id> --reason "external Dolt cutover probe passed" --json
```

Also verify one dependency read and one metadata update through the normal Gas City command path. The purpose is to cover both reads and committed writes; direct SQL queries alone do not prove application behavior.

## A/B performance test

### Experimental controls

Use the same conditions for the managed-local baseline and external candidate:

- same Gas City binary and config revision;
- same formula and number of independent beads;
- same source commits and comparable worktree state;
- same agent providers and model configuration;
- same worker pool limits;
- no unrelated assigned work occupying the measured pool;
- warmed build cache in both runs;
- no concurrent Cargo commands sharing a target directory;
- equivalent build pressure when testing database isolation;
- a fresh trace/log window with exact start and end timestamps.

If the external run is performed with no build pressure while the baseline ran under heavy builds, it does not test the proposed isolation benefit. Run both a quiet control and a representative loaded run.

### Required measurements

Collect these for both variants:

| Layer | Measurements |
|---|---|
| Workflow | creation-to-finalizer wall time; each step's ready, claim, start, and close times |
| Capacity | pool occupancy and explicit blocker for each queued bead |
| Orchestrator | cycle count; duration p50/p95/p99/max; cycles over 30/60/120 s |
| Store operations | `demand_snapshot.load` and `bead_reconcile_tick` p50/p95/max |
| Reliability | MySQL timeouts, unexpected EOFs, circuit-open failures, reconnects |
| Formula retries | semantic artifact retries versus infrastructure retries |
| Database | connection utilization, query duration, CPU, memory, disk latency, free space, GC activity |
| Build host | CPU, memory, root free space, build-target bytes, Cargo lock wait |
| Network | endpoint RTT p50/p95/p99, connection failures, TLS handshake failures |

Capture command output under an on-disk temporary directory such as `/var/tmp/gc-external-dolt-test-<timestamp>`. Do not put build caches or large trace exports under `/tmp`.

### Pass criteria

The external candidate passes only if:

1. The full workflow and every source bead close naturally without manual adoption, metadata repair, or force-close.
2. There are **zero** MySQL I/O timeouts, unexpected EOFs, and Dolt circuit-breaker openings in the measured window.
3. No artifact-validity attempt is consumed by a store lookup or transport failure.
4. Ready work with pool capacity starts within the configured reconciliation and provider-startup contract; every longer wait records a concrete capacity blocker.
5. Orchestrator cycle p95 is below 30 s and max is below 90 s for the repeated three-item proof.
6. `demand_snapshot.load` and bead reconciliation no longer dominate the critical path.
7. Bead CRUD p95 does not regress materially from the healthy managed-local control.
8. Database and build-host free space remain above their operational alert thresholds.
9. A backup created from the external endpoint can be restored and queried in an isolated environment.

The 30 s/90 s cycle limits are acceptance targets for this proof, not universal platform defaults. If the workload legitimately requires different thresholds, record them before the run rather than changing them after seeing the result.

### Interpretation

| Result | Conclusion |
|---|---|
| Storage failures disappear and latency improves | Local Dolt resource contention was a material cause; external deployment is justified |
| Storage failures disappear but latency regresses | Reliability improved, but network/query latency needs tuning or a closer endpoint |
| Same failures recur with healthy network | Investigate query load, connection limits, Dolt server health, and storage maintenance |
| Workflow remains slow with healthy store metrics | Continue with worker capacity, Cargo scheduling, provider startup, and retry-classification fixes |
| Candidate fails only under representative build load | Isolation is incomplete; inspect shared network, storage, or host dependencies |

## Rollback

### Before any external write

If validation or startup fails before the external endpoint accepts writes:

```sh
gc stop
gc beads city use-managed --dry-run
gc beads city use-managed
gc start
```

Verify health, scope inventory, and project identities against the retained managed store.

### After any external write

Do **not** switch directly back to the stale managed store. That would discard or fork accepted work.

1. Quiesce all writers.
2. Record the external endpoint's latest commits and scope counts.
3. Back up the external endpoint.
4. Transfer the external authoritative state back into a replacement managed store, or repair the external service in place.
5. Validate database names, commits, tables, and project identities.
6. Run `gc beads city use-managed` only after the managed target contains the authoritative state.
7. Restart and repeat the smoke checks.

Any period in which both stores accepted writes requires explicit reconciliation. Gas City does not merge divergent endpoint histories as part of `use-managed` or `use-external`.

## Follow-up work independent of migration

The following changes remain necessary whether the city stays local or moves:

1. Classify store unavailability as infrastructure failure so it does not consume semantic artifact retries.
2. Preserve sanitized `gc bd show` stderr in attempt records.
3. Emit root/step/attempt/session-correlated queue and execution timestamps.
4. Record the exact pool-capacity blocker for delayed ready work.
5. Add structured command spans for Cargo lock wait, cache state, duration, exit class, and peak disk use.
6. Serialize aggregate Rust verification and prohibit concurrent Cargo processes from sharing a target directory.
7. Keep build targets and temporary caches off `/tmp` and enforce root free-space preflight.
8. Make trace reading report and quarantine malformed historical segments instead of failing the whole query or silently skipping data.
9. Monitor and maintain Dolt history, backups, GC, connection utilization, and disk headroom in either topology.

## Evidence limitations

The incident evidence establishes correlation and a credible mechanism, not a controlled causal proof. The run combined database instability, worker-pool occupancy, duplicated Rust verification, local disk pressure, and repeated artifact retries. It lacked a single trace joining workflow, queue, provider startup, claim, command, validation, retry, and finalization.

The A/B procedure above is therefore part of the recommendation. Without it, “remote Dolt fixes the run” remains an inference.