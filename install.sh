#!/usr/bin/env bash
# Installs or updates garner, Garner's terminal interface, on macOS or
# Linux: downloads the newest release for this machine from GitHub,
# checks it against the release's SHA256SUMS and puts it into
# ~/.local/bin.
#
#   curl -fsSL https://raw.githubusercontent.com/trucke/garner/main/install.sh | bash
#
# GARNER_VERSION=v0.6.1 installs that version instead of the newest, and
# GARNER_INSTALL_DIR another folder instead of ~/.local/bin.
set -euo pipefail

repo=https://github.com/trucke/garner
# What a failed or cut-short run leaves behind; removed on exit.
tmp=""
part=""
trap 'rm -rf "$tmp" "$part"' EXIT

fail() {
	printf '✗ %s\n' "$1" >&2
	exit 1
}

# Everything runs from main, so a download cut short runs nothing.
main() {
	command -v curl >/dev/null || fail "garner's installer needs curl"

	local os arch
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "garner runs on macOS and Linux, not on $(uname -s)" ;;
	esac
	case "$(uname -m)" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) fail "garner runs on ARM and x86 machines, not on $(uname -m)" ;;
	esac
	# A terminal under Rosetta reports x86 on a Mac with Apple silicon.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
		arch=arm64
	fi

	local version=${GARNER_VERSION:-}
	if [ -z "$version" ]; then
		# GitHub redirects the newest release to its tag's page.
		version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$repo/releases/latest") ||
			fail "could not reach GitHub to find the newest release"
		version=${version##*/}
	fi
	[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "no release version found: $version"

	local file="garner-$version-$os-$arch"
	local dir=${GARNER_INSTALL_DIR:-$HOME/.local/bin}
	mkdir -p "$dir"
	tmp=$(mktemp -d)
	# Next to its target, so the rename below replaces it in one step, even
	# while garner runs.
	part="$dir/.garner-$version.$$"

	printf 'Downloading garner %s for %s on %s...\n' "$version" "$os" "$arch"
	curl -fsSL -o "$part" "$repo/releases/download/$version/$file" ||
		fail "could not download $file from $repo/releases/tag/$version"
	curl -fsSL -o "$tmp/SHA256SUMS" "$repo/releases/download/$version/SHA256SUMS" ||
		fail "could not download the checksums of $version"

	local want got
	want=$(awk -v f="$file" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || fail "SHA256SUMS of $version lists no $file"
	if command -v sha256sum >/dev/null; then
		got=$(sha256sum "$part" | awk '{ print $1 }')
	else
		got=$(shasum -a 256 "$part" | awk '{ print $1 }')
	fi
	[ "$got" = "$want" ] || fail "$file does not match its checksum; nothing was installed"

	chmod 755 "$part"
	mv -f "$part" "$dir/garner"
	part=""
	printf '✓ Installed %s to %s\n' "$("$dir/garner" --version)" "$dir/garner"

	# shellcheck disable=SC2016 # $PATH is for the user to type
	case ":$PATH:" in
	*":$dir:"*) printf '  Run garner to connect to your server.\n' ;;
	*) printf '  %s is not in your PATH; add it, for example in ~/.bashrc or ~/.zshrc:\n    export PATH="%s:$PATH"\n' "$dir" "$dir" ;;
	esac
}

main "$@"
