#!/usr/bin/env python3
r"""CI suite-coverage classification and aggregation.

Companion to the ``changes`` job in ``.github/workflows/ci.yml``. That job
gates downstream jobs on dorny/paths-filter outputs. Pull requests normally
run only path-matched suites; a cross-cutting ``shared`` path expands that to
the full union. Main-branch pushes and reusable calls with
``force_full_suite`` also run the full union, independently of the changed
paths.

This module centralizes the deterministic mechanism behind that wiring and the
metric that measures it. Callers provide the event, force flag, and
already-computed filter results:

  * ``classify_mode`` — label a run ``full`` or ``filtered``.
  * ``classify_reason`` — explain why that mode was selected.
  * ``path_suite_selected`` — model the path-gated suite policy.
  * ``full_rest_selected`` — model the full REST policy.
  * ``paths_match`` — dorny-compatible glob matching for deterministic policy
    fixtures and offline changed-file simulation.
  * ``aggregate`` — compute the share of runs that took each path.

Usage:

  # In CI (the ``changes`` job), record this run's classification:
  #   python3 ci_suite_coverage.py classify \
  #     "$SHARED_FILTER_RESULT" "$EVENT_NAME" "$FORCE_FULL_SUITE"
  # Writes ``suite_mode`` and ``suite_reason`` to $GITHUB_OUTPUT and a row to
  # $GITHUB_STEP_SUMMARY.

  # Aggregate the metric across recent main-branch merges. Collect the
  # per-run modes (emitted by the classify step as ``ci_suite_mode=<mode>``
  # notices) and pipe them in, one token per line:
  #   gh run list --workflow=CI --branch=main --json databaseId --jq '.[].databaseId' \
  #     | while read -r id; do \
  #         gh run view "$id" --log 2>/dev/null | sed -n 's/.*ci_suite_mode=\([a-z]*\).*/\1/p' | head -n1; \
  #       done \
  #     | python3 ci_suite_coverage.py
"""

from __future__ import annotations

import functools
import json
import os
import re
import sys
from typing import Iterable, Mapping

FULL = "full"
FILTERED = "filtered"

REASON_FORCED = "forced"
REASON_MAIN_PUSH = "main-push"
REASON_SHARED_PATH = "shared-path"
REASON_PATH_FILTERED = "path-filtered"

# Values dorny/paths-filter and GitHub boolean inputs render as when true.
TRUE = "true"


def full_suite_requested(event_name: str, force_full_suite: bool) -> bool:
    """Return whether the invocation requires every deterministic suite."""
    return event_name == "push" or force_full_suite


def classify_mode(
    shared_matched: bool,
    event_name: str = "pull_request",
    force_full_suite: bool = False,
) -> str:
    """Return ``full`` when shared paths, push, or an explicit force selects it."""
    return (
        FULL
        if shared_matched or full_suite_requested(event_name, force_full_suite)
        else FILTERED
    )


def classify_reason(
    shared_matched: bool,
    event_name: str = "pull_request",
    force_full_suite: bool = False,
) -> str:
    """Return the stable reason code for a suite-coverage classification."""
    if force_full_suite:
        return REASON_FORCED
    if event_name == "push":
        return REASON_MAIN_PUSH
    if shared_matched:
        return REASON_SHARED_PATH
    return REASON_PATH_FILTERED


def path_suite_selected(
    path_matched: bool,
    shared_matched: bool,
    event_name: str,
    force_full_suite: bool,
) -> bool:
    """Model one path-gated suite output from the workflow's ``changes`` job."""
    return (
        path_matched
        or shared_matched
        or full_suite_requested(event_name, force_full_suite)
    )


def full_rest_selected(
    integration_selected: bool,
    event_name: str,
    force_full_suite: bool,
) -> bool:
    """Return whether the broad REST matrix runs for this invocation."""
    return integration_selected and full_suite_requested(event_name, force_full_suite)


def _glob_to_regex_body(pattern: str) -> str:
    """Translate one dorny-style glob into an (unanchored) regex body.

    Mirrors the picomatch semantics dorny/paths-filter relies on, covering
    every glob shape the ``ci.yml`` filters actually use:

      * ``*`` matches any run of characters within a single path segment
        (it never crosses ``/``) — e.g. ``cmd/gc/session_*`` or the mid-path
        ``cmd/gc/template_resolve*.go``.
      * ``**`` matches across path segments.
      * A leading ``**/`` collapses to zero-or-more segments, so ``**/*.go``
        matches both ``main.go`` at the repo root and nested ``a/b/c.go``.
      * A trailing ``/**`` matches the directory itself and everything under
        it, so ``internal/beads/**`` covers ``internal/beads/store.go``.
    """
    if pattern.endswith("/**"):
        return _glob_to_regex_body(pattern[: -len("/**")]) + r"(?:/.*)?"

    parts: list[str] = []
    i, n = 0, len(pattern)
    while i < n:
        char = pattern[i]
        if char == "*":
            if i + 1 < n and pattern[i + 1] == "*":
                # ``**`` globstar; ``**/`` collapses to optional leading segments.
                if i + 2 < n and pattern[i + 2] == "/":
                    parts.append(r"(?:.*/)?")
                    i += 3
                    continue
                parts.append(r".*")
                i += 2
                continue
            parts.append(r"[^/]*")
            i += 1
            continue
        parts.append(re.escape(char))
        i += 1
    return "".join(parts)


@functools.lru_cache(maxsize=None)
def _glob_to_regex(pattern: str) -> "re.Pattern[str]":
    """Compile a dorny-style glob into a fully anchored regex.

    Translating to a regex (rather than hand-casing a few prefixes) keeps the
    simulator faithful to dorny for the full set of shapes, closing the
    silent-under-fire gap where a real glob like ``cmd/gc/template_resolve*.go``
    matched nothing.
    """
    return re.compile(_glob_to_regex_body(pattern) + r"\Z")


def _match_one(path: str, pattern: str) -> bool:
    """Match a single repo-relative path against one dorny-style glob."""
    return _glob_to_regex(pattern).match(path) is not None


def paths_match(changed_files: Iterable[str], globs: Iterable[str]) -> bool:
    """Return True if any changed file matches any glob in the filter."""
    globs = list(globs)
    return any(_match_one(path, glob) for path in changed_files for glob in globs)


def aggregate(modes: Iterable[str]) -> Mapping[str, object]:
    """Summarize a sequence of run modes into coverage percentages."""
    modes = list(modes)
    total = len(modes)
    full = sum(1 for mode in modes if mode == FULL)
    filtered = sum(1 for mode in modes if mode == FILTERED)
    unknown = total - full - filtered

    def pct(count: int) -> float:
        return round(100.0 * count / total, 1) if total else 0.0

    return {
        "total": total,
        "full": full,
        "filtered": filtered,
        "unknown": unknown,
        "full_pct": pct(full),
        "filtered_pct": pct(filtered),
    }


def _emit_classification(
    shared_result: str,
    event_name: str,
    force_full_suite_result: str,
) -> None:
    """Record this run's suite mode and reason for the metric.

    Writes the ``suite_mode`` and ``suite_reason`` job outputs and a
    human-readable row to the step summary. The notice retains the existing
    ``ci_suite_mode=<full|filtered>`` token consumed by aggregation tooling.
    """
    shared_matched = shared_result.strip().lower() == TRUE
    force_full_suite = force_full_suite_result.strip().lower() == TRUE
    mode = classify_mode(shared_matched, event_name, force_full_suite)
    reason = classify_reason(shared_matched, event_name, force_full_suite)

    github_output = os.environ.get("GITHUB_OUTPUT")
    if github_output:
        with open(github_output, "a", encoding="utf-8") as handle:
            handle.write(f"suite_mode={mode}\n")
            handle.write(f"suite_reason={reason}\n")

    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary:
        with open(step_summary, "a", encoding="utf-8") as handle:
            handle.write("## CI Suite Coverage\n\n")
            handle.write(f"- mode: `{mode}`\n")
            handle.write(f"- reason: `{reason}`\n")
            handle.write(f"- event: `{event_name}`\n")
            handle.write(f"- full suite forced: `{force_full_suite_result}`\n")
            handle.write(f"- cross-cutting core path changed: `{shared_result}`\n")

    # A workflow notice annotation, also grep-able from `gh run view --log`.
    print(
        f"::notice title=CI suite coverage::"
        f"ci_suite_mode={mode} ci_suite_reason={reason}"
    )


def main(argv: list[str]) -> int:
    if len(argv) >= 2 and argv[1] == "classify":
        if len(argv) != 5:
            print(
                "usage: ci_suite_coverage.py classify "
                "<shared-filter-result> <event-name> <force-full-suite>",
                file=sys.stderr,
            )
            return 2
        _emit_classification(argv[2], argv[3], argv[4])
        return 0

    # Default: aggregate mode tokens read from stdin, one per line.
    modes = [line.strip() for line in sys.stdin if line.strip()]
    print(json.dumps(aggregate(modes), indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
