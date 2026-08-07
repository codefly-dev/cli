#!/bin/sh
set -eu

: "${RELEASE_PRERELEASE:?RELEASE_PRERELEASE is required}"
: "${RELEASE_TAG:?RELEASE_TAG is required}"

case "$RELEASE_PRERELEASE" in
true)
	exit 0
	;;
false)
	;;
*)
	printf 'invalid RELEASE_PRERELEASE value: %s\n' "$RELEASE_PRERELEASE" >&2
	exit 2
	;;
esac

cask="$(
	gh api repos/codefly-dev/homebrew-cli/contents/Casks/codefly.rb --jq .content |
		base64 --decode
)"
version="${RELEASE_TAG#v}"
printf '%s\n' "$cask" | grep -F 'cask "codefly"'
printf '%s\n' "$cask" | grep -F "version \"$version\""
printf '%s\n' "$cask" | grep -F 'releases/download/v#{version}/'
