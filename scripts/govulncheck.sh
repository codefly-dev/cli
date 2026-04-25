#!/usr/bin/env bash
# govulncheck wrapper that mirrors `codefly agent build`'s audit logic:
# runs govulncheck, partitions findings into suppressed (listed in
# .govulncheck.yaml) vs actionable (with upstream fix) vs unpatched
# (no upstream fix yet). Exits non-zero ONLY when an UNSUPPRESSED
# finding has an upstream fix available — unpatched upstream issues
# don't block CI (no rebuild can clear them).
#
# Suppressions file: walks up from this repo looking for .govulncheck.yaml.
# In the codefly-dev workspace that lives at the workspace root and is
# shared across every Go module — same path the cli's runAudit uses.

set -euo pipefail

if ! command -v govulncheck >/dev/null 2>&1; then
    echo "govulncheck not installed — installing"
    go install golang.org/x/vuln/cmd/govulncheck@latest
    export PATH="$(go env GOPATH)/bin:$PATH"
fi

# Find suppressions file by walking up from the repo root.
suppressions_file=""
dir="$(pwd)"
while [ "$dir" != "/" ]; do
    if [ -f "$dir/.govulncheck.yaml" ]; then
        suppressions_file="$dir/.govulncheck.yaml"
        break
    fi
    dir="$(dirname "$dir")"
done

# Extract suppressed IDs. Plain grep avoids a yq dependency in CI.
suppressed_ids=""
if [ -n "$suppressions_file" ]; then
    suppressed_ids="$(grep -oE 'GO-[0-9]+-[0-9]+' "$suppressions_file" | sort -u || true)"
    if [ -n "$suppressed_ids" ]; then
        echo "Loaded $(echo "$suppressed_ids" | wc -l | tr -d ' ') suppression(s) from $suppressions_file"
    fi
fi

# Run govulncheck. Don't fail the script on its non-zero exit (3 means
# vulns found) — we partition + decide ourselves.
output="$(govulncheck ./... 2>&1 || true)"

# Each "Vulnerability #N: GO-YYYY-NNNN" intro plus the "Fixed in: X" line
# below it gives us what we need. Awk pulls (id, fixed) pairs.
findings="$(echo "$output" | awk '
    /^Vulnerability #[0-9]+: GO-/ { id = $3; fixed = ""; next }
    /^[[:space:]]*Fixed in:/ {
        # everything after the colon, trimmed
        sub(/^[[:space:]]*Fixed in:[[:space:]]*/, "")
        fixed = $0
        if (id != "") { print id "\t" fixed; id = "" }
    }
')"

actionable=""
unpatched=""
suppressed_hits=""
while IFS=$'\t' read -r id fixed; do
    [ -z "$id" ] && continue
    if echo "$suppressed_ids" | grep -qx "$id"; then
        suppressed_hits="$suppressed_hits $id"
        continue
    fi
    if [ "$fixed" = "N/A" ] || [ -z "$fixed" ]; then
        unpatched="$unpatched $id($fixed)"
    else
        actionable="$actionable $id(fix:$fixed)"
    fi
done <<< "$findings"

if [ -n "$actionable" ]; then
    echo
    echo "❌ ACTIONABLE vulnerabilities (upstream fix available):$actionable"
    echo
    echo "$output"
    exit 1
fi

if [ -n "$unpatched" ]; then
    echo "⚠️  Unpatched upstream (tracked, no fix yet):$unpatched"
fi
if [ -n "$suppressed_hits" ]; then
    echo "✓ Suppressed (reviewed via .govulncheck.yaml):$suppressed_hits"
fi
if [ -z "$actionable" ] && [ -z "$unpatched" ] && [ -z "$suppressed_hits" ]; then
    echo "✓ No vulnerabilities found."
fi
