#!/bin/sh
# Fails when a job of a GitHub workflow file has no timeout-minutes. Usage: check-timeouts.sh <workflow.yml>
set -eu
f=${1:?usage: check-timeouts.sh <workflow.yml>}
awk '
function flush() { if (job != "" && !seen) { print FILENAME ": job " job " has no timeout-minutes"; bad = 1 } }
/^jobs:/ { injobs = 1; next }
/^[^ #]/ { if (injobs) flush(); injobs = 0; job = "" }
injobs && /^  [A-Za-z0-9_-]+:[ ]*$/ { flush(); job = $1; sub(/:$/, "", job); seen = 0; n++; next }
injobs && /^    timeout-minutes:/ { seen = 1 }
END { flush(); if (n == 0) { print FILENAME ": no jobs found"; bad = 1 } exit bad }
' "$f"
echo "$f: every job has timeout-minutes"
