#!/usr/bin/env bash
# Report how many lines this fork changes in upstream-owned files.
# Those lines are what conflict when rebasing onto upstream, so keep them few.
# Exits 1 when some of those changes are whitespace-only (reindented or realigned
# upstream code), which conflicts on every upstream edit and can always be avoided.
#
# Usage: scripts/fork-footprint.sh [upstream-ref]   (default: origin/main)
set -euo pipefail

upstream="${1:-origin/main}"
base="$(git merge-base "$upstream" HEAD)"

# files that exist upstream and that the fork modifies; new files never conflict. Comparing
# against the working tree includes staged and unstaged edits, so it works before committing
raw="$(git diff --diff-filter=M --numstat "$base")"
nows="$(git diff -w --diff-filter=M --numstat "$base")"

echo "Fork changes in upstream-owned files (vs $(git rev-parse --short "$base"), largest first):"
printf '%6s %6s  %s\n' "+" "-" "file"
echo "$raw" | sort -k1,1nr -k2,2nr | awk 'NF { printf "%6s %6s  %s\n", $1, $2, $3 }'

total() { awk 'NF { a += $1; d += $2; n++ } END { printf "%d files, +%d -%d", n, a, d }'; }
echo
echo "total:             $(echo "$raw" | total)"
echo "ignoring spacing:  $(echo "$nows" | total)"
echo "new fork files:    $(git diff --diff-filter=A --name-only "$base" | wc -l | tr -d ' ')"

# a file whose counts drop under -w has reindented or realigned upstream lines; files whose
# changes are all spacing are missing from the -w list, so they count as zero there
noisy="$(awk -F'\t' '
	NR == FNR { a[$3] = $1; d[$3] = $2; next }
	{ wa = ($3 in a) ? a[$3] : 0; wd = ($3 in d) ? d[$3] : 0
	  if ($1 != wa || $2 != wd) printf "  %s (+%s -%s, +%s -%s ignoring spacing)\n", $3, $1, $2, wa, wd }
' <(echo "$nows") <(echo "$raw"))"
if [[ -n "$noisy" ]]; then
	echo
	echo "Whitespace-only changes to upstream lines (see FORK.md):"
	echo "$noisy"
	exit 1
fi
