#!/usr/bin/env bash
# Usage: scripts/build-helper.sh claude-shim|nano-launcher
# Builds and installs a helper without restarting it. Set NANO_CODESIGN_IDENTITY
# to sign it with a stable identity, preserving macOS privacy grants on rebuild.
set -euo pipefail
repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "${1:-}" in
claude-shim) package=./cmd/claude-shim output=claude-shim-bin identifier=local.nano.claude-shim ;;
nano-launcher) package=./cmd/nano-launcher output=nano-launcher-bin identifier=local.nano.launcher ;;
*)
	echo "usage: scripts/build-helper.sh claude-shim|nano-launcher" >&2
	exit 2
	;;
esac

next="$repo/.$output.next.$$"
trap 'rm -f "$next"' EXIT

(cd "$repo" && go build -o "$next" "$package")
if [[ -n "${NANO_CODESIGN_IDENTITY:-}" ]]; then
	sign_args=(--force --sign "$NANO_CODESIGN_IDENTITY" --identifier "$identifier")
	[[ -z "${NANO_CODESIGN_KEYCHAIN:-}" ]] || sign_args+=(--keychain "$NANO_CODESIGN_KEYCHAIN")
	codesign "${sign_args[@]}" "$next"
	codesign --verify --strict "$next"
fi
mv "$next" "$repo/$output"
echo "build-helper: installed $output; restart its launchd job to run it"
