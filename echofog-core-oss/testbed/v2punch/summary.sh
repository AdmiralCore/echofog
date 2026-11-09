#!/bin/sh
# Runs the lab and prints only the verdict lines (what a report needs).
cd "$(dirname "$0")/../.." || exit 2
NO_BUILD="${NO_BUILD:-1}" sh testbed/v2punch/run.sh 2>&1 | grep -E '^== |"direct"|tried|waited|efv2:'
