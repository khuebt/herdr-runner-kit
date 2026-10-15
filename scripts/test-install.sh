#!/bin/sh
# Test install.sh against a release built from this checkout and served on
# localhost: a good install over one from before the rename, a tampered
# checksum, doctor on a machine without herdr, and a release from before the
# rename. Used by CI; runs locally too. Needs go and python3.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
port=${HRK_TEST_PORT:-18765}
work=$(mktemp -d)
server=
cleanup() {
	[ -n "$server" ] && kill "$server" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

case $(uname -s) in Linux) os=linux ;; Darwin) os=darwin ;; esac
case $(uname -m) in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; esac
version=v0.0.0-test
archive=herdr-runner-kit_${version#v}_${os}_${arch}.tar.gz
rel=$work/releases/download/$version
mkdir -p "$rel" "$work/pkg"

(cd "$root" && CGO_ENABLED=0 go build -trimpath \
	-ldflags "-X github.com/khuebt/herdr-runner-kit/internal/buildinfo.Version=${version#v}" \
	-o "$work/pkg/hrk" ./cmd/hrk)
cp "$work/pkg/hrk" "$work/pkg/herdr-runner-kit"
tar -czf "$rel/$archive" -C "$work/pkg" hrk herdr-runner-kit
(cd "$rel" && { sha256sum "$archive" 2>/dev/null || shasum -a 256 "$archive"; } >checksums.txt)
# A release from before the rename, used last.
old=v0.0.0-old
mkdir -p "$work/oldpkg"
(cd "$root" && CGO_ENABLED=0 go build -trimpath \
	-ldflags "-X github.com/khuebt/herdr-runner-kit/internal/buildinfo.Version=${old#v}" \
	-o "$work/oldpkg/herdr-runner-kit" ./cmd/hrk)

python3 -m http.server --bind 127.0.0.1 --directory "$work" "$port" >/dev/null 2>&1 &
server=$!
for _ in 1 2 3 4 5 6 7 8 9 10; do
	curl -fs -o /dev/null "http://127.0.0.1:$port/" && break
	sleep 0.3
done

# A fresh home: no config, no Claude, and no herdr on PATH.
export HOME="$work/home" XDG_CONFIG_HOME="$work/home/.config" XDG_STATE_HOME="$work/home/.local/state"
unset CLAUDE_CONFIG_DIR HRK_STATE_DIR TELEGRAM_STATE_DIR
mkdir -p "$HOME"
bin=$work/bin
export PATH="$bin:/usr/bin:/bin"
export HRK_BASE_URL="http://127.0.0.1:$port/releases" HRK_VERSION="$version" HRK_INSTALL_DIR="$bin"

# An install upgraded from before the rename: a plain file under the old
# name, and the hrk link its setup made.
mkdir -p "$bin"
printf '#!/bin/sh\necho old\n' >"$bin/herdr-runner-kit"
chmod 755 "$bin/herdr-runner-kit"
ln -s herdr-runner-kit "$bin/hrk"

sh "$root/install.sh" >"$work/install.log" 2>&1 || {
	cat "$work/install.log"
	fail "install"
}
"$bin/hrk" version | grep -q "herdr-runner-kit ${version#v} " || fail "installed version"
[ "$(readlink "$bin/herdr-runner-kit")" = hrk ] || fail "no herdr-runner-kit alias"
if [ ! -f "$bin/hrk" ] || [ -L "$bin/hrk" ]; then fail "hrk is not the binary itself"; fi
[ -d "$HOME/.claude/skills/herdr-runner-kit" ] || fail "install did not run setup"
echo "ok: install and setup"

# A tampered checksum installs nothing and keeps the current binary.
cp "$bin/hrk" "$work/before"
awk '{ print "0000000000000000000000000000000000000000000000000000000000000000", $2 }' "$rel/checksums.txt" >"$work/sums"
mv "$work/sums" "$rel/checksums.txt"
if sh "$root/install.sh" >"$work/tamper.log" 2>&1; then
	fail "install accepted a tampered checksum"
fi
grep -q "checksum mismatch" "$work/tamper.log" || fail "tampered: $(cat "$work/tamper.log")"
cmp -s "$work/before" "$bin/hrk" || fail "tampered install changed the binary"
echo "ok: tampered checksum refused"

# doctor reports the missing herdr and exits non-zero, without crashing. The
# test release is not on GitHub, so its update check stays off.
echo "updateCheck: false" >>"$XDG_CONFIG_HOME/herdr-runner-kit/config.yaml"
status=0
"$bin/hrk" doctor >"$work/doctor.log" 2>&1 || status=$?
[ "$status" = 1 ] || {
	cat "$work/doctor.log"
	fail "doctor exit $status, want 1"
}
grep -q "^fail herdr" "$work/doctor.log" || fail "doctor did not name herdr: $(cat "$work/doctor.log")"
if grep -q "panic\|goroutine " "$work/doctor.log"; then fail "doctor crashed"; fi
echo "ok: doctor without herdr"

# A release from before the rename holds the binary only as herdr-runner-kit.
oldrel=$work/releases/download/$old
oldarchive=herdr-runner-kit_${old#v}_${os}_${arch}.tar.gz
mkdir -p "$oldrel"
tar -czf "$oldrel/$oldarchive" -C "$work/oldpkg" herdr-runner-kit
(cd "$oldrel" && { sha256sum "$oldarchive" 2>/dev/null || shasum -a 256 "$oldarchive"; } >checksums.txt)
HRK_VERSION=$old HRK_NO_SETUP=1 sh "$root/install.sh" >"$work/old.log" 2>&1 || {
	cat "$work/old.log"
	fail "install of a release from before the rename"
}
"$bin/hrk" version | grep -q "herdr-runner-kit ${old#v} " || fail "installed old version"
"$bin/herdr-runner-kit" version | grep -q "herdr-runner-kit ${old#v} " || fail "alias after installing an old release"
echo "ok: release from before the rename"
