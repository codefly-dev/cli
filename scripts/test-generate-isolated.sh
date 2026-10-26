#!/usr/bin/env bash
# Compile once per package, then run every discovered top-level test in its
# own process: Cobra flags and other package globals survive between tests.
#
# The row comes from CODEFLY_CONFORMANCE_REQUIRED, the claim the job already
# makes. Its build tag and packages come from pkg/conformance/matrix.json and
# the tagged sources, never from a list kept here: `--list` prints the compile
# command and the packages.
set -euo pipefail

row=${CODEFLY_CONFORMANCE_REQUIRED:-}
if [[ -z "$row" || "$row" == *,* ]]; then
  echo "CODEFLY_CONFORMANCE_REQUIRED must name exactly one conformance row, got '${row}'" >&2
  exit 1
fi
# A command substitution under set -e fails the script when the helper does;
# process substitution would hide that failure.
derived=$(go run ./scripts/conformance-packages "$row")
tag=$(head -n 1 <<<"$derived")
packages=()
while IFS= read -r package; do
  packages+=("$package")
done < <(tail -n +2 <<<"$derived")
if (( ${#packages[@]} == 0 )); then
  echo "row $row derived no packages" >&2
  exit 1
fi
# The one compile command every package uses. --list prints it, then walks
# the same loop as a run and prints each package it would compile, so a
# repository test holds what actually runs to the matrix.
compile=(go test)
if [[ -n "$tag" ]]; then
  compile+=(-tags "$tag")
fi
compile+=(-c)
list=0
if [[ "${1:-}" == "--list" ]]; then
  list=1
  echo "${compile[*]}"
else
  shard=${1:-0}
  shards=${2:-1}
  if [[ ! "$shard" =~ ^[0-9]+$ || ! "$shards" =~ ^[1-9][0-9]*$ ]] || (( shard >= shards )); then
    echo "expected a zero-based shard index smaller than the positive shard count" >&2
    exit 1
  fi
  index=0
  selected=0
  failed=0
  root=$(pwd)
  evidence="${root}/generate-evidence"
  mkdir -p "$evidence"
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  printf 'package\ttest\tseconds\tstatus\n' > "$evidence/timings.tsv"
fi

for package in "${packages[@]}"; do
  if (( list )); then
    echo "$package"
    continue
  fi
  name=${package//\//-}
  binary="$work/$name.test"
  "${compile[@]}" -o "$binary" "./$package"
  # Do not hide discovery failures behind process substitution. A package
  # with no tests must fail instead of silently claiming qualification.
  "$binary" -test.list '^Test' > "$work/tests"
  grep '^Test' "$work/tests" > "$work/selected"
  while IFS= read -r test; do
    slot=$((index % shards))
    index=$((index + 1))
    if (( slot != shard )); then
      continue
    fi
    selected=$((selected + 1))
    echo "::group::$package/$test"
    started=$SECONDS
    status=passed
    # Use the package directory just as go test does (fixtures are relative).
    # Each process also gets its own on-disk Codefly state.
    if ! (cd "$root/$package" && CODEFLY_HOME="$work/home-$name-$test" \
      "$binary" -test.run "^${test}$" -test.v -test.count=1 -test.timeout=4m) \
      2>&1 | tee "$evidence/$name-$test.log"; then
      status=failed
      failed=1
    fi
    printf '%s\t%s\t%s\t%s\n' "$package" "$test" "$((SECONDS - started))" "$status" >> "$evidence/timings.tsv"
    echo '::endgroup::'
  done < "$work/selected"
done
if (( list )); then
  exit 0
fi
if (( selected == 0 )); then
  echo "shard $shard/$shards selected no tests" >&2
  exit 1
fi
echo "shard $shard/$shards ran $selected of $index discovered tests"
exit "$failed"
