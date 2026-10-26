#!/usr/bin/env bash
# Compile once per package, then run every discovered top-level test in its
# own process: Cobra flags and other package globals survive between tests.
set -euo pipefail

root=$(pwd)
evidence="${root}/generate-evidence"
mkdir -p "$evidence"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failed=0
printf 'package\ttest\tseconds\tstatus\n' > "$evidence/timings.tsv"

for package in cmd/generate cmd/publish cmd/sync pkg/generators; do
  name=${package//\//-}
  binary="$work/$name.test"
  go test -tags proto_companion_required -c -o "$binary" "./$package"
  # Do not hide discovery failures behind process substitution. A package
  # with no tests must fail instead of silently claiming qualification.
  "$binary" -test.list '^Test' > "$work/tests"
  grep '^Test' "$work/tests" > "$work/selected"
  while IFS= read -r test; do
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
exit "$failed"
