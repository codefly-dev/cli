#!/usr/bin/env bash
# Prints the `go test` arguments of test shard INDEX of TOTAL, one per line.
#
#   go test -race ... $(./scripts/test-shard.sh 3 9)
#
# The suite's four slowest packages take two to four minutes each under the
# race detector, longer than the whole budget for a shard, so they are split by
# test name: each SPLITS line below is a shard of its own, running one package
# with a -run pattern, and a package's patterns together match every test it
# declares exactly once (pkg/conformance's TestTestShardsRunEveryTestOnce holds
# that). Every other package is packed onto the remaining shards, heaviest
# first, each on the least-loaded shard.
#
# The weights only balance the shards. A stale weight costs wall clock, never
# coverage: an unlisted package still lands on exactly one shard.
set -euo pipefail

index="${1:?usage: test-shard.sh INDEX TOTAL}"
total="${2:?usage: test-shard.sh INDEX TOTAL}"
if ! [[ "$index" =~ ^[0-9]+$ && "$total" =~ ^[0-9]+$ ]] || ((index < 1 || index > total)); then
	echo "test-shard.sh: INDEX must be in 1..TOTAL (got $index of $total)" >&2
	exit 2
fi

cd "$(git rev-parse --show-toplevel)"
module="$(go list -m)"

# One shard each: a package (relative to the module), and the -run pattern of
# its share when it is split. A split package's patterns divide its top-level
# tests by the letter after "Test", where that halves its measured time (Go CLI
# run 42706145692). A package with no pattern runs whole, on a shard alone.
SPLITS='
pkg/gitops ^Test[A-L]
pkg/gitops ^Test[^A-L]
pkg/orchestration ^Test[A-F]
pkg/orchestration ^Test[^A-F]
pkg/composition ^Test[A-R]
pkg/composition ^Test[^A-R]
pkg/gateway
'

# Seconds per package under -race (same run), for packing the rest.
WEIGHTS='
cmd/agents 72
cmd 37
cmd/generate 26
cmd/ci 25
cmd/publish 23
pkg/conformance 20
pkg/engine 17
pkg/gateway/dockerexec 10
pkg/librarystore 9
cmd/run 7
cmd/common 6
pkg/control 6
pkg/solutionrun 5
'

# A read loop rather than mapfile: macOS ships Bash 3.2, and the partition
# guard runs this script from the ordinary test suite.
splits=()
while IFS= read -r line; do
	[[ -n "${line// /}" ]] && splits+=("$line")
done <<<"$SPLITS"
if ((total <= ${#splits[@]})); then
	echo "test-shard.sh: TOTAL must exceed the ${#splits[@]} split shards, or no shard runs the other packages" >&2
	exit 2
fi

if ((index <= ${#splits[@]})); then
	read -r pkg pattern <<<"${splits[index - 1]}"
	if [[ -n "${pattern:-}" ]]; then
		printf -- '-run=%s\n' "$pattern"
	fi
	printf '%s/%s\n' "$module" "$pkg"
	exit 0
fi

split_packages="$(printf '%s\n' "${splits[@]}" | awk -v m="$module" '{ print m "/" $1 }' | sort -u)"
go list ./... | grep -vxF -f <(printf '%s\n' "$split_packages") |
	MODULE="$module" WEIGHTS="$WEIGHTS" awk -v index_="$((index - ${#splits[@]}))" -v total="$((total - ${#splits[@]}))" '
	BEGIN {
		n = split(ENVIRON["WEIGHTS"], lines, "\n")
		for (i = 1; i <= n; i++) {
			if (split(lines[i], f, " ") == 2) weight[ENVIRON["MODULE"] "/" f[1]] = f[2]
		}
	}
	{ pkgs[++count] = $0 }
	END {
		# Heaviest first; ties keep go list order, which is sorted, so the
		# packing is the same on every shard.
		for (i = 1; i <= count; i++) order[i] = i
		for (i = 2; i <= count; i++) {
			j = i
			while (j > 1 && w(pkgs[order[j]]) > w(pkgs[order[j - 1]])) {
				t = order[j]; order[j] = order[j - 1]; order[j - 1] = t; j--
			}
		}
		for (s = 1; s <= total; s++) load[s] = 0
		for (i = 1; i <= count; i++) {
			best = 1
			for (s = 2; s <= total; s++) if (load[s] < load[best]) best = s
			load[best] += w(pkgs[order[i]])
			if (best == index_) print pkgs[order[i]]
		}
	}
	function w(p) { return (p in weight) ? weight[p] : 1 }
'
