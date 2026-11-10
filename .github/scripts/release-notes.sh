#!/usr/bin/env bash
# Prints the CHANGELOG.md section for a release tag as release notes.
#
#   release-notes.sh vX.Y.Z [CHANGELOG.md]
#
# Exits non-zero when the changelog has no section for the version. Both
# release.yml and image.yml run this before they build anything, so a tag
# without a changelog entry publishes neither a release nor a version image.
#
# GitHub renders every newline in a release body as a line break, so the
# hard-wrapped paragraphs and list items of the changelog are joined into
# single lines. Headings, list-item starts, blank lines and fenced code keep
# their place. The section ends at the next version heading or at the
# link-reference definitions at the bottom of the file, which only mean
# something inside CHANGELOG.md.
set -euo pipefail

tag="${1:?usage: release-notes.sh vX.Y.Z [CHANGELOG.md]}"
changelog="${2:-CHANGELOG.md}"
version="${tag#v}"

notes=$(awk -v v="$version" '
  function flush() { if (buf != "") { emit(buf); buf = "" } }
  # Blank lines are held back and written only between two pieces of
  # content, so the notes neither start nor end with one, and runs collapse.
  function emit(line) {
    if (blank && printed) print ""
    blank = 0; printed = 1
    print line
  }

  !found { if (index($0, "## [" v "]") == 1) found = 1; next }
  /^## \[/ || /^\[.+\]: / { exit }

  /^[[:space:]]*```/ { flush(); fence = !fence; emit($0); next }
  fence { emit($0); next }

  /^[[:space:]]*$/ { flush(); blank = 1; next }
  /^#/ { flush(); emit($0); next }
  /^[[:space:]]*([-*+]|[0-9]+[.)])[[:space:]]/ { flush(); buf = $0; next }
  {
    line = $0
    if (buf == "") { buf = line; next }
    sub(/^[[:space:]]+/, "", line)
    buf = buf " " line
  }
  END { flush() }
' "$changelog")

if [[ -z "${notes//[[:space:]]/}" ]]; then
  echo "::error::${changelog} has no section for ${version}" >&2
  exit 1
fi
printf '%s\n' "$notes"
