#!/usr/bin/env bash
#
# bootstrap.sh — clone the full Codefly flat checkout from scratch.
#
# This is the bootstrap counterpart to `codefly self pull` / `codefly self
# build --with-agents`. Those maintain an existing checkout; they cannot create
# one, because they only touch directories that already exist and, for the
# build, need the CLI to already be installed. This script has no such
# dependency — it is plain bash + git + gh, so it runs on an empty machine
# before `codefly` exists.
#
# It discovers every repository in the GitHub org (via the GitHub API) and
# clones each into <root>/<repo-name>, the same flat layout `self pull`
# discovers (a checkout is canonical when its directory name matches its origin
# repository name). It is NON-DESTRUCTIVE: a repo whose target directory
# already exists is left untouched and reported as skipped, so re-running is
# safe and never overwrites local work.
#
# Requirements: git, and the GitHub CLI (`gh`) authenticated for org discovery.
#
set -euo pipefail

ORG="codefly-dev"
ROOT="."
BRANCH=""
REMOTE="origin"
PROTOCOL="ssh"
DRY_RUN=0
INCLUDE_ARCHIVED=0
INCLUDE_FORKS=0
WANT_TOPICS=()
NAMES=()

usage() {
	cat <<'EOF'
Clone the full Codefly flat checkout into place.

Usage:
  bootstrap.sh [flags] [repo...]

With no repo arguments, every non-archived, non-fork repository in the org is
discovered via the GitHub API and cloned. Pass explicit repo names to clone
only those (discovery is skipped).

Flags:
  --dir DIR         Root directory to clone into (default: current directory)
  --org ORG         GitHub org to clone from (default: codefly-dev)
  --branch BRANCH   Branch to check out (default: each repo's default branch)
  --remote NAME     Name for the created remote (default: origin)
  --ssh             Clone over SSH (git@github.com:...) [default]
  --https           Clone over HTTPS (https://github.com/...)
  --agents          Only repos tagged with the "codefly-agent" topic
  --services        Only repos tagged with the "codefly-service" topic
  --all             All discovered repos (default; clears --agents/--services)
  --include-archived  Include archived repositories
  --include-forks     Include forked repositories
  --dry-run         Print what would be cloned without cloning
  -h, --help        Show this help

Examples:
  bootstrap.sh
  bootstrap.sh --dir ~/Development/codefly.dev
  bootstrap.sh --https --branch main
  bootstrap.sh --agents
  bootstrap.sh core llm cli
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

while [ $# -gt 0 ]; do
	case "$1" in
	--dir)
		ROOT="${2:-}"
		[ -n "$ROOT" ] || die "--dir requires a value"
		shift 2
		;;
	--org)
		ORG="${2:-}"
		[ -n "$ORG" ] || die "--org requires a value"
		shift 2
		;;
	--branch)
		BRANCH="${2:-}"
		[ -n "$BRANCH" ] || die "--branch requires a value"
		shift 2
		;;
	--remote)
		REMOTE="${2:-}"
		[ -n "$REMOTE" ] || die "--remote requires a value"
		shift 2
		;;
	--ssh) PROTOCOL="ssh"; shift ;;
	--https) PROTOCOL="https"; shift ;;
	--agents) WANT_TOPICS+=("codefly-agent"); shift ;;
	--services) WANT_TOPICS+=("codefly-service"); shift ;;
	--all) WANT_TOPICS=(); shift ;;
	--include-archived) INCLUDE_ARCHIVED=1; shift ;;
	--include-forks) INCLUDE_FORKS=1; shift ;;
	--dry-run) DRY_RUN=1; shift ;;
	-h | --help) usage; exit 0 ;;
	--) shift; while [ $# -gt 0 ]; do NAMES+=("$1"); shift; done ;;
	-*) die "unknown flag: $1" ;;
	*) NAMES+=("$1"); shift ;;
	esac
done

command -v git >/dev/null 2>&1 || die "git is required"

repo_url() {
	local name="$1"
	if [ "$PROTOCOL" = "https" ]; then
		echo "https://github.com/${ORG}/${name}.git"
	else
		echo "git@github.com:${ORG}/${name}.git"
	fi
}

# topic_matches reports whether a repo's topic CSV satisfies the active
# category filter. With no --agents/--services filter, everything matches.
topic_matches() {
	local topics_csv="$1"
	[ ${#WANT_TOPICS[@]} -eq 0 ] && return 0
	local want
	for want in "${WANT_TOPICS[@]}"; do
		case ",${topics_csv}," in
		*",${want},"*) return 0 ;;
		esac
	done
	return 1
}

# discover prints the repo names to clone, one per line, honoring the archived,
# fork, and topic filters. Requires gh authenticated against the org.
discover() {
	command -v gh >/dev/null 2>&1 ||
		die "gh (GitHub CLI) is required to discover org repositories; install it or pass explicit repo names"

	local jq_filter='.[]'
	[ "$INCLUDE_ARCHIVED" -eq 1 ] || jq_filter="${jq_filter} | select(.archived == false)"
	[ "$INCLUDE_FORKS" -eq 1 ] || jq_filter="${jq_filter} | select(.fork == false)"
	jq_filter="${jq_filter} | [.name, (.topics // [] | join(\",\"))] | @tsv"

	local name topics
	while IFS=$'\t' read -r name topics; do
		[ -n "$name" ] || continue
		topic_matches "$topics" && printf '%s\n' "$name"
	done < <(gh api --paginate "orgs/${ORG}/repos?per_page=100" --jq "$jq_filter")
}

# Build the target list: explicit names if given, otherwise API discovery.
targets=()
if [ ${#NAMES[@]} -gt 0 ]; then
	targets=("${NAMES[@]}")
else
	while IFS= read -r name; do
		[ -n "$name" ] || continue
		targets+=("$name")
	done < <(discover)
fi

[ ${#targets[@]} -gt 0 ] || die "no repositories to clone"

# Sort and de-duplicate for stable, readable output.
sorted=()
while IFS= read -r name; do
	sorted+=("$name")
done < <(printf '%s\n' "${targets[@]}" | sort -u)
targets=("${sorted[@]}")

# Width the name column to the longest target for aligned reporting.
width=12
for name in "${targets[@]}"; do
	[ ${#name} -gt "$width" ] && width=${#name}
done

mkdir -p "$ROOT"

if [ "$DRY_RUN" -eq 1 ]; then
	echo "Dry run: would clone ${#targets[@]} repositories from ${ORG} into ${ROOT} over ${PROTOCOL}"
else
	echo "Cloning ${#targets[@]} repositories from ${ORG} into ${ROOT} over ${PROTOCOL}"
fi

cloned=0
skipped=0
failed=0
failures=()

for name in "${targets[@]}"; do
	dest="${ROOT}/${name}"
	if [ -e "$dest" ]; then
		printf '%-*s skipped (already exists)\n' "$width" "$name"
		skipped=$((skipped + 1))
		continue
	fi

	url="$(repo_url "$name")"
	clone_args=(clone --origin "$REMOTE")
	[ -n "$BRANCH" ] && clone_args+=(--branch "$BRANCH")
	clone_args+=("$url" "$dest")

	if [ "$DRY_RUN" -eq 1 ]; then
		printf '%-*s would clone %s\n' "$width" "$name" "$url"
		cloned=$((cloned + 1))
		continue
	fi

	if out="$(git "${clone_args[@]}" 2>&1)"; then
		printf '%-*s cloned\n' "$width" "$name"
		cloned=$((cloned + 1))
	else
		first_line="$(printf '%s\n' "$out" | head -n1)"
		printf '%-*s failed: %s\n' "$width" "$name" "$first_line" >&2
		failed=$((failed + 1))
		failures+=("$name")
	fi
done

if [ "$DRY_RUN" -eq 1 ]; then
	echo "Dry run: ${cloned} would clone, ${skipped} skipped"
	exit 0
fi

echo "Done: ${cloned} cloned, ${skipped} skipped, ${failed} failed"
[ "$failed" -eq 0 ] || exit 1
