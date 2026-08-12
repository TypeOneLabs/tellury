#!/usr/bin/env bash
# Print the CHANGELOG.md section for one version, for use as GitHub release notes.
#
#   scripts/release-notes.sh v0.4.0
#
# The changelog is written by hand because a generated list of commit subjects
# says what moved rather than what an operator has to do about it. This script
# is what keeps the release page and the changelog from drifting: the release
# notes are not a second copy, they are the same text.
#
# Exits non-zero when the version has no section, so a release fails loudly
# rather than publishing with empty notes.
set -euo pipefail

version="${1:-}"
if [[ -z "$version" ]]; then
	echo "usage: $0 <version>   e.g. $0 v0.4.0" >&2
	exit 2
fi

# Accept both "v0.4.0" and "0.4.0"; the changelog headings carry no "v".
bare="${version#v}"

changelog="$(dirname "$0")/../CHANGELOG.md"
if [[ ! -f "$changelog" ]]; then
	echo "release-notes: no CHANGELOG.md at $changelog" >&2
	exit 1
fi

# Print everything after the "## [x.y.z]" heading up to the next "## " heading.
notes="$(awk -v ver="$bare" '
	$0 ~ "^## \\[" ver "\\]" { found = 1; next }
	found && /^## / { exit }
	found { print }
' "$changelog")"

# Trim leading and trailing blank lines.
notes="$(printf '%s\n' "$notes" | sed -e '/./,$!d' -e :a -e '/^\n*$/{$d;N;ba' -e '}')"

if [[ -z "$notes" ]]; then
	echo "release-notes: CHANGELOG.md has no section for $version" >&2
	exit 1
fi

printf '%s\n' "$notes"
