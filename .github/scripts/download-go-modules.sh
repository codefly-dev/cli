#!/usr/bin/env bash
# download-go-modules — `go mod download` for the module in the current
# directory, with git reading codefly-dev/authority through AUTHORITY_TOKEN.
#
# The CLI requires the authority's interface client, hop and delivery libraries,
# and codefly-dev/authority is private. `go` fetches it with git, so the token
# reaches git through this process's environment (GIT_CONFIG_*), rewriting that
# one repository's URL and nothing else: no git configuration is written, and
# no later step of the job, nor any other github.com fetch, can see it. Every
# step after this one builds from the module cache this fills.
#
# AUTHORITY_TOKEN is supplied by the caller (CI uses the organization GH_PAT).
# The repository's path must be private to go (GOPRIVATE), so the module proxy
# and the checksum database are never asked about it.
set -euo pipefail

: "${AUTHORITY_TOKEN:?AUTHORITY_TOKEN is required: supply a token with contents read access to codefly-dev/authority}"

authority=github.com/codefly-dev/authority
for setting in GONOPROXY GONOSUMDB; do
  case ",$(go env "$setting")," in
    *,github.com/codefly-dev,* | *,github.com/codefly-dev/\*,* | *,"$authority",*) ;;
    *)
      echo "::error::$setting does not cover $authority; set GOPRIVATE=$authority so neither the module proxy nor the checksum database is asked for it" >&2
      exit 1
      ;;
  esac
done

n="${GIT_CONFIG_COUNT:-0}"
export "GIT_CONFIG_KEY_${n}=url.https://x-access-token:${AUTHORITY_TOKEN}@${authority}.insteadOf"
export "GIT_CONFIG_VALUE_${n}=https://${authority}"
export GIT_CONFIG_COUNT=$((n + 1))
unset AUTHORITY_TOKEN

go mod download "$@"
