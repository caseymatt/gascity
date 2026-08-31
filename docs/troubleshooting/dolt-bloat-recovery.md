---
title: Recover from Dolt Bloat
description: Recover a Gas City beads store whose managed Dolt footprint has grown out of proportion.
---

## Overview

Gas City stores beads in a managed Dolt server. The complete per-database
footprint is `.beads/dolt/<database>/.dolt/`, not only its `noms/` directory.
The main components are:

- `noms/`: immutable chunks and the live Dolt commit graph.
- `git-remote-cache/`: rebuildable Git transport objects for `git+...` remotes.
- `temptf/`: temporary table and NBS scratch files.

`gc doctor` measures the complete footprint and reports this component
breakdown as `dolt-storage-size`. This runbook covers each component; running
chunk GC cannot reclaim remote-cache or temporary-file growth.

## Symptoms

1. `gc doctor` reports `dolt-storage-size` as **Warning** or **Error**.
2. `du -sh <cityPath>/.beads/dolt/<database>/.dolt/` exceeds the configured
   warning threshold.
3. `bd` writes feel slower than usual, or `bd ready` takes noticeable time.
4. Agents fail with Dolt connection or timeout errors, especially shortly
   after start.


## Identify the dominant component

Measure each path separately. Do not pass both a parent and its children to one
`du` invocation: GNU `du` de-duplicates blocks across arguments, which makes the
parent total look smaller than it is.

```bash
du -sh <cityPath>/.beads/dolt/<database>/.dolt
du -sh <cityPath>/.beads/dolt/<database>/.dolt/noms
du -sh <cityPath>/.beads/dolt/<database>/.dolt/git-remote-cache
du -sh <cityPath>/.beads/dolt/<database>/.dolt/temptf
```

Use the matching recovery below. A large `noms/` directory needs compaction or
GC; a large `git-remote-cache/` or `temptf/` directory does not.
## Preconditions

- **Stop all agents.** Run `gc stop` in the affected city so no session is
  writing to Dolt.
- **Ensure no external writers are connected.** If you have opened a
  `dolt sql` shell against the managed port, quit it.
- **Free disk space.** Dolt GC rewrites chunks into a new store before
  swapping; budget at least **2× the current `.dolt/` size** in free space
  on the same filesystem.
- **Dolt 2.1.2 or newer.** Dolt 2.1.2 restored Git-remote teardown and
  2.1.3 bounds the parented cache history, preventing the remote cache from
  retaining every force-pushed flattened history. Gas City's embedded Beads
  dependency includes these fixes. Check the external server binary with
  `dolt version`.

## Noms Recovery Procedure

```bash
# 1. Stop the supervisor (and with it, all agents and the managed Dolt server).
gc stop <cityPath>

# 2. Capture a safety backup before touching the store.
cd <cityPath>/.beads/dolt/<database>
cp -a .dolt .dolt.bak-$(date +%Y%m%d-%H%M%S)

# 3. Run a full GC with archive compression. This is the step that actually
#    reclaims space. On a 120 GB store expect this to take tens of minutes.
dolt gc --archive-level=1

# 4. Restart the city and verify.
cd <cityPath>
gc start
gc doctor          # dolt-storage-size should now report the remaining components accurately
du -sh .beads/dolt/<database>/.dolt
```

If `gc doctor` reports a clean `dolt-storage-size` and agents come back up
cleanly, the recovery is complete. You may delete the `.dolt.bak-*`
directory at your leisure once you are confident in the new store.

## Reclaiming stale temporary files

A hard-killed Dolt process can leave `buffered_file_byte_sink_*` scratch files
under `.dolt/temptf/`. Gas City's managed-server preflight removes regular files
there once they are older than 24 hours, after proving the data-directory lock
is free and before starting the next server.

```bash
gc stop <cityPath>
gc start <cityPath>
gc doctor
```

Do not delete `temptf/` while the server is running. Files newer than 24 hours
are preserved because they may belong to recent work.

## Reclaiming a legacy Git remote cache

Dolt 2.1.2+ cleans process-owned cache refs on graceful teardown, and 2.1.3+
bounds reachable cache history. Those fixes prevent future unbounded growth but
do not necessarily reclaim refs leaked by an older process. For a legacy cache
that still dominates `dolt-storage-size`:

1. Verify `dolt version` is 2.1.3 or newer and the configured remote is
   reachable.
2. Stop the city. Do not touch a cache held by the running SQL server.
3. Rename `.dolt/git-remote-cache` to a sibling rollback directory.
4. Restart the city and query the affected bead store. Dolt rebuilds the cache
   from the configured remote.
5. Keep the rollback directory until `gc bd list --rig <rig> --limit 1 --json`,
   `gc doctor`, and the next remote sync all succeed; then remove it.

The cache is transport state, not the local Dolt database. Never move or delete
`.dolt/noms`, `repo_state.json`, or working-set files as part of this procedure.

## Reclaiming a database stranded below the compaction threshold

`gc dolt compact` skips any database with fewer commits than the threshold
(default 2000, `GC_DOLT_COMPACT_THRESHOLD_COMMITS`). A database can fall *below*
that threshold yet still carry orphaned chunks — most commonly after a prior
flatten squashed its history but the post-flatten full GC was deferred (a
concurrent writer raced the flatten), quarantined and later cleared, or
otherwise never completed. Scheduled compaction then skips the database forever
and the space is never reclaimed. The skip is visible in the compactor log as:

```
compact: db=<database> commits=<n> below_threshold=<t> oldgen_archives=present pending_gc=absent — skip ...
```

Use the operator-invoked reclaim path to recover such a database without waiting
for its commit count to climb back over the threshold:

```bash
# Reclaim one stranded database. Runs CALL DOLT_GC('--full') with no flatten,
# bypassing the commit-count threshold.
gc dolt compact --gc-only --only-db <database>

# Preview first, mutating nothing.
gc dolt compact --gc-only --only-db <database> --dry-run
```

`--gc-only` refuses any database under an integrity-quarantine marker; resolve
the underlying reason (see **Compact Quarantine Reasons** below) before
reclaiming. Unlike the full `dolt gc --archive-level=1` procedure above,
`--gc-only` runs against the live managed server and does not require stopping
the city — though quiescing writers still makes the GC faster and more
thorough.

## Compacting a city whose Dolt remote is uncredentialed

Before flattening (and again before pushing) the compactor runs
`CALL DOLT_FETCH('<remote>')` to reconcile against the remote. Against an
**uncredentialed git+https remote**, that call does not merely return an error —
it **crashes the managed Dolt sql-server process**. The shell tolerates a
non-zero return code ("proceeding from local source of truth") but cannot catch
a server-process death across the process boundary: the supervisor restarts the
server seconds later, but by then every remaining database's probe hits
`connection refused`, so one misconfigured remote takes down compaction for the
whole city.

If a city's remote is not (yet) credentialed, opt out of the fetch so
compaction runs entirely from the local source of truth. The post-compaction
remote push is deferred via a pending-push marker and resumes automatically on a
later run once the fetch path is healthy:

```bash
# Skip the fetch for every database this run.
gc dolt compact --skip-fetch

# Equivalent environment opt-out (e.g. set in a wrapper or on the city).
GC_DOLT_COMPACT_SKIP_FETCH=1 gc dolt compact

# Skip the fetch only for specific, known-uncredentialed databases (CSV);
# credentialed databases in the same city still fetch and push normally.
GC_DOLT_COMPACT_SKIP_FETCH_DBS=<database>[,<database>...] gc dolt compact
```

Prefer the per-database `GC_DOLT_COMPACT_SKIP_FETCH_DBS` form over the global
opt-out when only some databases are uncredentialed — the global form disables
remote sync for every database, including ones whose push would otherwise
succeed. Do **not** set the global opt-out in the shared `mol-dog-compactor`
order for the same reason; set the per-database env on the affected city
instead.

## Compacting a database that must never reach a remote

Some databases are deliberately local — a privacy boundary, or a store whose
remote was configured by accident. Mark those `.no-sync` and compaction skips
its remote phase entirely: no fetch, no push, and no deferred push recorded.
The database still flattens and GCs, so it keeps the disk benefit:

```bash
# Exclude one database from all remote sync, compaction included.
touch <cityPath>/.beads/dolt/<database>/.no-sync
```

`gc dolt sync` and `gc dolt pull` honor the same marker, so one file covers
every remote path.

Choose `.no-sync` over `--skip-fetch` when a database must never reach a
remote. `--skip-fetch` defers the push and waits for the remote to become
usable later; `.no-sync` states that it never will.

## Expected Outcome

DoltHub's archive format typically delivers ~30% compression on top of
normal GC ([DoltHub blog, archive storage](https://www.dolthub.com/blog/)).
Combined with reclamation of orphan chunks from agent churn, a 120 GB
pre-GC store typically drops to somewhere between **5 GB and 20 GB** —
depending on how much of the pre-GC size was live data versus orphan
chunks.

If GC finishes but the size barely moves, the chunks are nearly all live
(no garbage to collect). See **When to Escalate** below.

## Prevention

- **Keep Dolt at 2.1.3 or newer.** These releases clean process-owned Git
  transport refs and bound parented cache history in addition to the managed
  server's normal auto-GC and archive compression.
- **Let the dolt pack's `mol-dog-compactor` order run continuously.**
  It ships embedded in the dolt pack and runs `gc dolt compact` once a
  managed database crosses the commit threshold. Compaction fetches the
  configured remote, flattens live history, runs `CALL DOLT_GC('--full')`,
  and pushes the rewritten main branch back upstream. Dolt 1.86.x does not
  support an atomic `DOLT_PUSH('--force-with-lease', ...)`, so the script
  re-fetches and compares the remote head immediately before its force push.
  That check prevents known drift but cannot eliminate a remote write in the
  small fetch-to-push window.
- **Mind `orders.max_timeout` if you set one.** The compactor order asks
  for a 24-hour timeout to accommodate serialized full-GC runs on large
  stores. A city-level `orders.max_timeout` below 24h will cap the
  compactor and may kill an in-progress GC; raise the cap or leave it
  unset if you want unattended recovery on big databases.
- **Run `gc doctor` regularly.** A daily cron or CI job is enough. The
  `dolt-storage-size` check reports the full footprint and identifies which
  component needs attention.
- **Avoid long-lived `dolt sql` sessions from outside Gas City.** External
  clients hold open transactions that can block GC.

## Compact Quarantine Reasons

`gc dolt compact` writes exact reason strings into
`.gc/runtime/packs/dolt/compact-quarantine/<database>` when it detects
possible writer interference before full GC. Operator dashboards and runbooks
should treat these strings as the current vocabulary:

| Reason | Meaning |
|--------|---------|
| `post-flatten HEAD probe failed` | The compactor could not read the database HEAD after flatten. |
| `post-flatten integrity check failed` | A post-flatten integrity check failed before recording a more specific reason. |
| `post-flatten row count decreased` | A table lost rows after flatten. |
| `post-flatten row count probe failed` | The post-flatten row-count query failed or returned a non-number. |
| `post-flatten table value hash probe failed` | A post-flatten table hash query failed or returned empty. |
| `post-flatten table value hash changed with row-count increase` | A table gained rows and its value hash changed. |
| `post-flatten table value hash changed without row-count increase` | A table's value hash changed without a row-count gain. |
| `post-flatten table list changed` | A table appeared or an invalid table name was observed after preflight. |
| `post-flatten table list probe failed` | The post-flatten `information_schema.tables` query failed. |
| `post-flatten value hash probe failed` | The database hash query failed after flatten. |
| `post-flatten value hash probe returned empty value` | The database hash query returned an empty value after flatten. |
| `post-flatten value hash changed with row-count increase` | The database hash changed after at least one stable-table row-count gain. |
| `post-flatten value hash changed without row-count increase` | The database hash changed without a row-count gain. |

Quarantine markers also carry structured evidence. New markers include the
database name, the preflight/flatten/post-verify HEADs, preflight and
postflight database value hashes when available, `integrity_table_drift` for
table-level row/hash mismatches, `database_value_hash_drift` for aggregate hash
drift, and `decision=preserve_marker_manual_review_required`.

Safe marker-clear procedure:

1. Require a clean application worktree: `git status --short` should show no
   product/config/test changes you have not accounted for.
2. Confirm the Dolt server is reachable with `gc dolt status` and a live query
   such as `gc dolt sql --db <database> -q "SELECT COUNT(*) FROM issues"`.
3. Confirm bead queries are healthy for the affected store, for example
   `bd list --limit 1` in that rig or `gc bd list --rig <rig> --limit 1`.
4. Read the marker and retain it if the HEAD/hash/table evidence is incomplete
   or points at row loss. For table drift, compare the recorded HEADs with
   `DOLT_DIFF` / `DOLT_DIFF_STAT`; only clear when the diff proves preflight
   rows are still reachable and no unexpected table disappeared.
5. When the evidence proves no data loss, remove only that database's marker:
   `rm .gc/runtime/packs/dolt/compact-quarantine/<database>`.
6. Retry reclaim with `gc dolt compact --gc-only --only-db <database>`. If the
   marker returns or health checks fail, preserve the marker and escalate with
   the marker contents and command output.

`gc dolt compact` and `gc dolt compact --gc-only` refuse databases with
quarantine markers. The refusal output repeats the marker path, reason, key
evidence fields, and the clear/retry command so operators have the next action
without opening this runbook first.

## When to Escalate

If a recovery GC reduces `noms/` by less than ~10% and `gc doctor` still
flags `dolt-storage-size` with `noms` as the dominant component:

1. All remaining chunks are probably live — the database legitimately
   contains this much history. Squashing Dolt history is not a supported
   self-service operation today; escalate instead.
2. File a `bd` issue with:
   - `dolt version` output
   - `du -sh` of the `.dolt/` directory
   - `dolt log --oneline | wc -l`
   - a sample of `dolt log --stat` from the busiest day

Attach the `gc doctor --verbose` output as well. Do not delete the
`.dolt.bak-*` directory while the issue is open.
