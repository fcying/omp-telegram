#!/usr/bin/env bash
set -euo pipefail

shard=$1
count=$2
shift 2
if [[ ! "$shard" =~ ^[0-9]+$ || ! "$count" =~ ^[1-9][0-9]*$ ]]; then
    echo "Expected a zero-based shard and a positive shard count" >&2
    exit 2
fi
if (( shard >= count )); then
    echo "Shard must be smaller than the shard count" >&2
    exit 2
fi

# Listing must succeed before any shard is allowed to report success.
listing=$(go test "$@" -list '^(Test|Fuzz|Example)' ./...)
declare -A seen=()
selected=()
total=0
while IFS= read -r name; do
    case "$name" in
        Test*|Fuzz*|Example*) ;;
        *) continue ;;
    esac
    if [[ -n "${seen[$name]+present}" ]]; then
        continue
    fi
    seen[$name]=1
    total=$((total + 1))
    # Hash names, not their positions, so adding a test does not reshuffle the suite.
    read -r checksum _ <<< "$(printf '%s' "$name" | cksum)"
    if (( checksum % count == shard )); then
        selected+=("$name")
    fi
done <<< "$listing"

pattern='^$'
if (( ${#selected[@]} != 0 )); then
    pattern=$(IFS='|'; printf '^(%s)$' "${selected[*]}")
fi
printf 'Shard %s/%s: %s of %s root names\n' "$shard" "$count" "${#selected[@]}" "$total"
# An anchored root name runs all of its subtests and fuzz seeds together.
go test "$@" -run "$pattern" ./...
