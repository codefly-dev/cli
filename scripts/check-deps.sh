#!/usr/bin/env bash
# check-deps.sh — validate (and optionally install) the codefly CLI toolchain.
#
# Dependencies are split into two tiers:
#
#   REQUIRED — needed to BUILD the CLI (scripts/build/local.sh).
#   OPTIONAL — shelled out to at runtime by specific commands (docker, git,
#              buf, kubectl, …). The CLI builds and runs fine without them;
#              individual subcommands fail clearly if their tool is absent.
#
# Usage:
#   ./scripts/check-deps.sh                 # report status of every dependency
#   ./scripts/check-deps.sh --required-only # only the build-required tier
#   ./scripts/check-deps.sh --install       # install everything missing (asks first)
#   ./scripts/check-deps.sh --install --yes # install without the confirmation prompt
#   ./scripts/check-deps.sh --quiet         # print nothing if all-required is satisfied
#
# Exit status: 0 when every REQUIRED dependency is present, 1 otherwise.
set -euo pipefail

# ── dependency table ─────────────────────────────────────────────────────────
#   probe | tier | reason | brew | apt | custom_install
DEPS=(
  # ── REQUIRED: build the Go CLI ──────────────────────────────────────────────
  "go|required|Go toolchain — compiles the CLI (go 1.25+)|go|golang-go|"

  # ── OPTIONAL: shelled out by specific subcommands ───────────────────────────
  "git|optional|VCS ops (also vendored via go-git, but the CLI shells out too)|git|git|"
  "docker|optional|build/deploy/companion image commands|docker|docker.io|"
  "buf|optional|proto generation (generate)|buf||go install github.com/bufbuild/buf/cmd/buf@latest"
  "kubectl|optional|Kubernetes deploys|kubernetes-cli|kubectl|"
  "kustomize|optional|Kustomize overlays for deploys|kustomize||go install sigs.k8s.io/kustomize/kustomize/v5@latest"
  "yq|optional|YAML parsing (cli_image.sh reads pkg/cli/info.yaml)|yq|yq|"
  "nix|optional|Nix-based agent/plugin builds|nix||sh <(curl -L https://nixos.org/nix/install)"
)

# ── flags ────────────────────────────────────────────────────────────────────
DO_INSTALL=0
ASSUME_YES=0
REQUIRED_ONLY=0
QUIET=0
for arg in "$@"; do
  case "$arg" in
    --install)        DO_INSTALL=1 ;;
    --yes|-y)         ASSUME_YES=1 ;;
    --required-only)  REQUIRED_ONLY=1 ;;
    --quiet)          QUIET=1 ;;
    -h|--help)
      sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "unknown flag: $arg (try --help)" >&2; exit 2 ;;
  esac
done

# ── colors (only when stdout is a tty) ───────────────────────────────────────
if [ -t 1 ]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GRN=$'\033[32m'; YLW=$'\033[33m'; RST=$'\033[0m'
else
  BOLD=''; DIM=''; RED=''; GRN=''; YLW=''; RST=''
fi

# ── package-manager detection ────────────────────────────────────────────────
detect_pm() {
  if command -v brew    >/dev/null 2>&1; then echo brew
  elif command -v apt-get >/dev/null 2>&1; then echo apt
  elif command -v dnf   >/dev/null 2>&1; then echo dnf
  elif command -v pacman >/dev/null 2>&1; then echo pacman
  else echo none; fi
}
PM="$(detect_pm)"

have()    { command -v "$1" >/dev/null 2>&1; }
version() {
  local out
  out="$("$1" --version 2>/dev/null | head -1)" || true
  [ -z "$out" ] && { out="$("$1" version 2>/dev/null | head -1)" || true; }
  echo "$out"
}

install_cmd_for() {
  local brew="$1" apt="$2" custom="$3"
  if [ -n "$custom" ]; then echo "$custom"; return; fi
  case "$PM" in
    brew)   [ -n "$brew" ] && echo "brew install $brew" ;;
    apt)    [ -n "$apt" ]  && echo "sudo apt-get update && sudo apt-get install -y $apt" ;;
    dnf)    [ -n "$apt" ]  && echo "sudo dnf install -y $apt" ;;
    pacman) [ -n "$apt" ]  && echo "sudo pacman -S --noconfirm $apt" ;;
  esac
}

# ── scan ─────────────────────────────────────────────────────────────────────
missing_required=()
missing_optional=()
rows=()

for rec in "${DEPS[@]}"; do
  IFS='|' read -r probe tier reason brew apt custom <<<"$rec"
  [ "$REQUIRED_ONLY" = 1 ] && [ "$tier" != required ] && continue

  icmd="$(install_cmd_for "$brew" "$apt" "$custom")"

  if have "$probe"; then
    rows+=("  ${GRN}✓${RST} ${BOLD}$(printf '%-10s' "$probe")${RST} ${DIM}$(version "$probe")${RST}")
  elif [ "$tier" = required ]; then
    rows+=("  ${RED}✗${RST} ${BOLD}$(printf '%-10s' "$probe")${RST} ${RED}MISSING${RST} ${DIM}— $reason${RST}")
    missing_required+=("$probe|$reason|$icmd")
  else
    rows+=("  ${YLW}○${RST} ${BOLD}$(printf '%-10s' "$probe")${RST} ${YLW}not installed${RST} ${DIM}— $reason${RST}")
    missing_optional+=("$probe|$reason|$icmd")
  fi
done

if [ "$QUIET" = 1 ] && [ "${#missing_required[@]}" -eq 0 ]; then
  exit 0
fi

echo
echo "${BOLD}codefly CLI dependency check${RST} ${DIM}(package manager: ${PM})${RST}"
printf '%s\n' "${rows[@]}"
echo

print_fix_list() {
  local -n arr="$1"
  for rec in "${arr[@]}"; do
    IFS='|' read -r probe reason icmd <<<"$rec"
    if [ -n "$icmd" ]; then
      echo "    ${BOLD}$probe${RST}: ${DIM}$icmd${RST}"
    else
      echo "    ${BOLD}$probe${RST}: ${YLW}no known install for '$PM' — see project docs${RST}"
    fi
  done
}

# ── install path ─────────────────────────────────────────────────────────────
if [ "$DO_INSTALL" = 1 ]; then
  to_install=("${missing_required[@]}")
  [ "$REQUIRED_ONLY" = 0 ] && to_install+=("${missing_optional[@]}")

  if [ "${#to_install[@]}" -eq 0 ]; then
    echo "${GRN}Nothing to install — everything's present.${RST}"
    exit 0
  fi
  if [ "$PM" = none ]; then
    echo "${RED}No supported package manager (brew/apt/dnf/pacman) found.${RST}" >&2
    echo "Install these manually:" >&2
    print_fix_list to_install >&2
    exit 1
  fi

  echo "${BOLD}Will install ${#to_install[@]} missing dependenc$([ ${#to_install[@]} -eq 1 ] && echo y || echo ies):${RST}"
  print_fix_list to_install
  echo
  if [ "$ASSUME_YES" != 1 ]; then
    printf "Proceed? [y/N] "
    read -r reply
    case "$reply" in [yY]*) ;; *) echo "Aborted."; exit 1 ;; esac
  fi

  fail=0
  for rec in "${to_install[@]}"; do
    IFS='|' read -r probe reason icmd <<<"$rec"
    if [ -z "$icmd" ]; then
      echo "${YLW}↷ skip $probe — no install command for $PM${RST}"; fail=1; continue
    fi
    echo "${BOLD}→ installing $probe${RST}  ${DIM}($icmd)${RST}"
    if bash -c "$icmd"; then
      echo "${GRN}  ✓ $probe installed${RST}"
    else
      echo "${RED}  ✗ $probe failed${RST}"; fail=1
    fi
  done
  echo
  [ "$fail" = 0 ] && echo "${GRN}Done. Re-run without --install to verify.${RST}" \
                  || echo "${YLW}Some installs failed — see output above.${RST}"
  exit "$fail"
fi

# ── report-only path ─────────────────────────────────────────────────────────
if [ "${#missing_required[@]}" -gt 0 ]; then
  echo "${RED}${BOLD}Missing required dependencies:${RST}"
  print_fix_list missing_required
  echo
  echo "Install them all with: ${BOLD}./scripts/check-deps.sh --install${RST}"
  echo
  exit 1
fi

if [ "${#missing_optional[@]}" -gt 0 ] && [ "$QUIET" != 1 ]; then
  echo "${YLW}Optional dependencies not installed${RST} ${DIM}(fine for building the CLI; some subcommands need them):${RST}"
  print_fix_list missing_optional
  echo
  echo "Install everything with: ${BOLD}./scripts/check-deps.sh --install${RST}"
  echo
fi

echo "${GRN}${BOLD}All required dependencies present.${RST}"
