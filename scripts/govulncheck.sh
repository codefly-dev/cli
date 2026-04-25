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

# Suppression entries older than this are treated as expired — script
# fails so reviewers re-check whether the upstream has shipped a patch
# or our reachability analysis still holds. Stale .govulncheck.yaml
# entries silently shielding new exploit chains is the failure mode
# this guards against.
STALE_AFTER_DAYS=${GOVULN_STALE_AFTER_DAYS:-45}

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

# Extract suppressed IDs and (id, reviewed-date) pairs without a yq dep.
# Plain awk: the yaml block format is stable enough — `id:` and `reviewed:`
# are the only keys we care about, and they're emitted in lockstep.
suppressed_ids=""
stale_suppressions=""
if [ -n "$suppressions_file" ]; then
    pairs="$(awk '
        /^[[:space:]]*-[[:space:]]+id:/ {
            sub(/^[[:space:]]*-[[:space:]]+id:[[:space:]]*/, "")
            id = $0; sub(/[[:space:]]*$/, "", id)
        }
        /^[[:space:]]*reviewed:/ {
            sub(/^[[:space:]]*reviewed:[[:space:]]*/, "")
            sub(/[[:space:]]*$/, "")
            if (id != "") { print id "\t" $0; id = "" }
        }
    ' "$suppressions_file")"
    suppressed_ids="$(echo "$pairs" | awk -F'\t' '{print $1}' | sort -u)"
    if [ -n "$suppressed_ids" ]; then
        echo "Loaded $(echo "$suppressed_ids" | wc -l | tr -d ' ') suppression(s) from $suppressions_file"
    fi

    # Staleness check. macOS `date` (BSD) and GNU `date` differ — handle both.
    today_epoch=$(date +%s)
    while IFS=$'\t' read -r id reviewed; do
        [ -z "$id" ] && continue
        [ -z "$reviewed" ] && continue
        if reviewed_epoch=$(date -j -f "%Y-%m-%d" "$reviewed" "+%s" 2>/dev/null); then :
        elif reviewed_epoch=$(date -d "$reviewed" "+%s" 2>/dev/null); then :
        else
            echo "⚠️  $id: cannot parse reviewed date '$reviewed' — skipping staleness check"
            continue
        fi
        age_days=$(( (today_epoch - reviewed_epoch) / 86400 ))
        if [ "$age_days" -gt "$STALE_AFTER_DAYS" ]; then
            stale_suppressions="$stale_suppressions $id($age_days days)"
        fi
    done <<< "$pairs"
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

if [ -n "$stale_suppressions" ]; then
    echo
    echo "❌ STALE suppressions (>$STALE_AFTER_DAYS days since last review):$stale_suppressions"
    echo "Re-verify the suppression rationale, bump the 'reviewed:' date in .govulncheck.yaml, and commit."
    exit 1
fi

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
