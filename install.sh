#!/bin/sh
# Install herdr-runner-kit (the hrk command) from its GitHub release:
#
#   curl -fsSL https://raw.githubusercontent.com/khuebt/herdr-runner-kit/main/install.sh | sh
#
# HRK_VERSION       release to install, such as v0.2.0 (default: the latest)
# HRK_INSTALL_DIR   where the binary goes (default: ~/.local/bin)
# HRK_NO_SETUP=1    skip `hrk setup` afterwards
# HRK_BASE_URL      release download root, for testing against a local copy
set -eu

die() {
	echo "install.sh: $*" >&2
	exit 1
}

# Everything runs from main, called on the last line, so a download cut short
# by `curl | sh` runs nothing.
main() {
	repo=khuebt/herdr-runner-kit
	base=${HRK_BASE_URL:-https://github.com/$repo/releases}
	dir=${HRK_INSTALL_DIR:-$HOME/.local/bin}

	command -v curl >/dev/null 2>&1 || die "curl is required"
	command -v tar >/dev/null 2>&1 || die "tar is required"

	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS $(uname -s); releases exist for Linux and macOS" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU $(uname -m); releases exist for amd64 and arm64" ;;
	esac

	version=${HRK_VERSION:-}
	if [ -z "$version" ]; then
		# The latest-release page redirects to .../tag/<version>.
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/latest") || die "cannot reach $base/latest"
		version=${url##*/}
		case $version in
		v[0-9]*) ;;
		*) die "no release found at $base/latest" ;;
		esac
	fi
	case $version in v*) ;; *) version=v$version ;; esac

	archive=herdr-runner-kit_${version#v}_${os}_${arch}.tar.gz
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	echo "Downloading herdr-runner-kit $version for $os/$arch"
	curl -fsSL -o "$tmp/$archive" "$base/download/$version/$archive" || die "cannot download $archive"
	curl -fsSL -o "$tmp/checksums.txt" "$base/download/$version/checksums.txt" || die "cannot download checksums.txt"

	want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt lists no $archive"
	if command -v sha256sum >/dev/null 2>&1; then
		got=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
	elif command -v shasum >/dev/null 2>&1; then
		got=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
	else
		die "sha256sum or shasum is required to verify the download"
	fi
	[ "$got" = "$want" ] || die "checksum mismatch for $archive; nothing was installed"

	tar -xzf "$tmp/$archive" -C "$tmp" || die "cannot unpack $archive"
	# Releases from before the rename hold the binary only as herdr-runner-kit.
	src=$tmp/hrk
	[ -f "$src" ] || src=$tmp/herdr-runner-kit
	[ -f "$src" ] || die "$archive holds no hrk"
	mkdir -p "$dir"
	# Copy beside the target, then rename over it, so a running copy keeps working.
	cp "$src" "$dir/.hrk.new"
	chmod 755 "$dir/.hrk.new"
	mv -f "$dir/.hrk.new" "$dir/hrk"
	# The old name stays as an alias, so scripts, hooks and runners that call
	# it keep working; it too is swapped in by rename.
	ln -sf hrk "$dir/.herdr-runner-kit.new"
	mv -f "$dir/.herdr-runner-kit.new" "$dir/herdr-runner-kit"
	echo "Installed $dir/hrk (alias: herdr-runner-kit)"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "Note: $dir is not on your PATH. Add it, for example: export PATH=\"$dir:\$PATH\"" ;;
	esac

	if [ "${HRK_NO_SETUP:-}" != 1 ]; then
		"$dir/hrk" setup
	fi
}

main "$@"
