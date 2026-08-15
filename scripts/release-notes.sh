#!/usr/bin/env bash
# release-notes.sh VERSION [CHANGELOG]
#
# Prints the CHANGELOG section for VERSION — everything between that version's
# heading and the next level-2 heading. The release workflow feeds the result
# to the Gitea and GitHub release APIs, so the release pages and the file
# cannot disagree.
#
# VERSION may be given with or without a leading "v".
#
# Exits non-zero with nothing on stdout when the version has no section, which
# is what stops a tag from publishing empty release notes.
#
# This lives in a script rather than inline in the workflow so it can be
# tested: the first cut stopped only at a bracketed "## [" heading and ran
# straight through the trailing "## Versioning" section and the link
# definitions into the release notes.
set -euo pipefail

version="${1:?usage: release-notes.sh VERSION [CHANGELOG]}"
changelog="${2:-CHANGELOG.md}"
semver="${version#v}"

if [ ! -r "$changelog" ]; then
	echo "release-notes.sh: cannot read $changelog" >&2
	exit 2
fi

notes=$(
	awk -v want="$semver" '
		# Any level-2 heading ends the section, not just a bracketed version
		# one — the file ends with a plain "## Versioning" section.
		/^## / {
			if (found) exit
			if (index($0, "[" want "]") > 0) { found = 1; next }
		}
		found { print }
	' "$changelog"
)

# Trim leading and trailing blank lines; the section always opens with one.
notes=$(printf '%s\n' "$notes" | sed -e '/./,$!d' | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}')

if [ -z "$notes" ]; then
	echo "release-notes.sh: no section for $version in $changelog" >&2
	exit 1
fi

printf '%s\n' "$notes"
