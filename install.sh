#!/bin/sh
# Installs the latest bqto release: curl -fsSL https://raw.githubusercontent.com/pilef-co/bqto/main/install.sh | sh
# BQTO_VERSION=v0.16.0 picks a release, BQTO_INSTALL_DIR a folder (default /usr/local/bin),
# and GITHUB_TOKEN avoids GitHub's rate limit for anonymous requests.
set -eu

main() {
	repo=pilef-co/bqto
	dir=${BQTO_INSTALL_DIR:-/usr/local/bin}

	fail() {
		echo "error: $1" >&2
		echo "Fix: $2" >&2
		exit 1
	}

	case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "bqto runs on macOS and Linux, not on $(uname -s)" "use a Mac or Linux machine, or WSL on Windows" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "bqto has no build for $(uname -m)" "build it from source: go install github.com/$repo/cmd/bqto@latest" ;;
	esac
	command -v curl >/dev/null || fail "curl is not installed" "install curl and run this again"

	get() {
		if [ -n "${GITHUB_TOKEN:-}" ]; then
			curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" "$@"
		else
			curl -fsSL "$@"
		fi
	}

	api=https://api.github.com/repos/$repo/releases
	if [ -n "${BQTO_VERSION:-}" ]; then
		release=$(get "$api/tags/$BQTO_VERSION") || fail "no release $BQTO_VERSION" "pick one from https://github.com/$repo/releases"
	else
		release=$(get "$api/latest") || fail "could not read the latest release from GitHub" "check your connection, or set GITHUB_TOKEN when GitHub limits requests from your address"
	fi
	tag=$(printf '%s\n' "$release" | sed -n 's/^  "tag_name": "\(.*\)",$/\1/p')
	[ -n "$tag" ] || fail "GitHub answered without a release tag" "run it again, or download bqto from https://github.com/$repo/releases"
	archive=bqto_${tag#v}_${os}_${arch}.tar.gz

	asset() {
		url=$(printf '%s\n' "$release" | grep -B3 "\"name\": \"$1\"" | sed -n 's/^ *"url": "\(.*\)",$/\1/p' | head -n 1)
		[ -n "$url" ] || fail "release $tag has no $1" "download bqto from https://github.com/$repo/releases"
		get -H "Accept: application/octet-stream" -o "$tmp/$1" "$url"
	}

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	asset "$archive"
	asset checksums.txt

	want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
	if command -v sha256sum >/dev/null; then
		got=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
	else
		got=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
	fi
	[ -n "$want" ] && [ "$want" = "$got" ] || fail "the SHA-256 of $archive does not match checksums.txt" "run it again; if it still fails, report it as https://github.com/$repo/blob/main/SECURITY.md says"

	tar -xzf "$tmp/$archive" -C "$tmp" bqto
	if [ -w "$dir" ] || { [ ! -e "$dir" ] && mkdir -p "$dir" 2>/dev/null; }; then
		mv "$tmp/bqto" "$dir/bqto"
	else
		echo "installing to $dir needs sudo" >&2
		sudo mkdir -p "$dir"
		sudo mv "$tmp/bqto" "$dir/bqto"
	fi
	echo "installed bqto $tag in $dir/bqto"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "warn: $dir is not on your PATH; add it, or run $dir/bqto" >&2 ;;
	esac
}

main "$@"
