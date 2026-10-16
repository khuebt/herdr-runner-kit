#!/bin/sh
# Print the release notes for a release candidate tag: the commit subjects
# since the last stable tag. release-please writes notes only for stable
# releases, so the release-candidate workflow passes this to goreleaser.
# Usage: rc-notes.sh vX.Y.Z-rc.N
set -eu

tag=${1:?usage: rc-notes.sh vX.Y.Z-rc.N}
prev=$(git describe --tags --abbrev=0 --exclude '*-rc.*' "$tag^")

echo "## $tag (release candidate)"
echo
echo "A preview of the next stable release. Changes since $prev:"
echo
git log --format='- %s' "$prev..$tag" | grep -v '^- chore(main): release' || true
