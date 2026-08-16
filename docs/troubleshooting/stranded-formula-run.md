---
title: Recover a Stranded Formula Run
description: Adopt reviewed implementation evidence into a stranded attempt without bypassing the remaining formula lifecycle.
---

A worker can finish valid implementation work while its formulas v2 attempt
remains open—for example, after a session or orchestrator interruption. Recover
the attempt by adopting the reviewed source evidence. Do not close the workflow
root, drain, or control bead to make the run look complete.

## Read each lifecycle separately

```bash
gc run status <workflow-root-id> --json
```

The result keeps five states independent:

| Surface | What it answers |
| --- | --- |
| `owner_lifecycle` | Is the workflow root open, owner-held, awaiting owner close, or closed? |
| `workflow_control` | Are orchestrator controls active, blocked, failed, or complete? |
| `delivery` | Is implementation evidence or a final report recorded? |
| `publish` | Was publishing completed, skipped explicitly, or failed? |
| `merge` | Was a merge or pull-request handoff explicitly recorded? |

A closed implementation bead does not imply a completed workflow. A passing
workflow does not imply publication or merge. `publish.status=noop` means the
formula intentionally left the remote unchanged.

## Verify the source

The adoption command accepts one narrow state transition. Before running it,
verify all of these facts:

- The target bead has `gc.kind=ralph` and exactly one open attempt.
- The open attempt is unassigned.
- The source bead is closed with `gc.outcome=pass`.
- The workflow root names that source in `gc.drain_member_id`.
- The source has a full 40-character `gc.implementation.commit`.
- The source has an absolute `gc.implementation.summary_path` to a non-empty regular file under the workflow or source worktree.
- The source and open attempt name the same absolute `gc.work_dir`.

Inspect the records directly when any field is unclear:

```bash
gc bd show <ralph-control-id> --json
gc bd show <source-bead-id> --json
```

## Adopt the reviewed result

```bash
gc formula adopt <ralph-control-id> \
  --source <source-bead-id> \
  --actor "$(git config user.email)" \
  --reason "Reviewed implementation commit and summary evidence" \
  --json
```

The command atomically revalidates the unique attempt and current source
evidence, copies the commit, summary path, and source revision time into the
audit record, then closes only that attempt. Repeating the same command is
idempotent. A different or concurrently changing source, an assigned attempt,
multiple open attempts, missing evidence, or a finalized control fails without
closing the workflow.

The orchestrator then runs the normal check, summary, review, finalization, and
publish stages. Confirm progress with `gc run status` rather than closing a
blocked descendant manually.

## Retire an owner-held root

An `owned` workflow root remains open after its control graph completes until
its owner retires it. When `owner_lifecycle.status=awaiting_owner_close`, first
verify the delivery, publish, and merge fields match the intended outcome. Then
the owner can close the root with an evidence-backed reason:

```bash
gc bd close <workflow-root-id> --reason "Reviewed terminal run status and delivery evidence"
```

Do not remove the `owned` label globally. It is the persisted authority boundary
that prevents automatic cleanup from claiming completion on the owner's behalf.
