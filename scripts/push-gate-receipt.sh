#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: push-gate-receipt.sh {check|record} <local-commit> <base-commit|-> <gate-id>" >&2
  exit 2
}

[ "$#" -eq 4 ] || usage
action="$1"
local_commit="$2"
base_commit="$3"
gate_id="$4"

case "$action" in
  check|record) ;;
  *) usage ;;
esac

local_tree="$(git rev-parse --verify --end-of-options "${local_commit}^{tree}")"
if [ "$base_commit" = "-" ]; then
  base_tree="-"
else
  base_tree="$(git rev-parse --verify --end-of-options "${base_commit}^{tree}")"
fi
go_version="${PUSH_GATE_GO_VERSION:-$(go version)}"
payload="$(printf 'receipt-version=1\nlocal-tree=%s\nbase-tree=%s\ngo-version=%s\ngate-id=%s\n' \
  "$local_tree" "$base_tree" "$go_version" "$gate_id")"
key="$(printf '%s\n' "$payload" | git hash-object --stdin)"

if [ -n "${PUSH_GATE_RESULTS_DIR:-}" ]; then
  results_dir="$PUSH_GATE_RESULTS_DIR"
else
  common_dir="$(git rev-parse --git-common-dir)"
  results_dir="$common_dir/gate-results"
fi
receipt="$results_dir/$key"

if [ "$action" = "check" ]; then
  [ -f "$receipt" ] || exit 1
  IFS= read -r recorded_key < "$receipt" || exit 1
  [ "$recorded_key" = "$key" ]
  exit
fi

mkdir -p "$results_dir"
temporary="$results_dir/.${key}.$$"
trap 'rm -f "$temporary"' EXIT
umask 077
printf '%s\n%s\n' "$key" "$payload" > "$temporary"
mv -f "$temporary" "$receipt"
trap - EXIT
printf 'pre-push: recorded gate receipt %s for tree %s\n' "$key" "$local_tree"
