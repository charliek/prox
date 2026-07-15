#!/bin/sh
# Leader shell for the #29 orphan-grandchild integration tests.
#
# prox launches this via `sh -c '<cmd>'`. Because this script is the sole
# command in that invocation, the launching shell execs directly into it, so
# this script's own process is the process-group leader (Setpgid'd by prox at
# launch). It backgrounds stubborn_listener.py, which is therefore a true
# *grandchild* relative to prox.
#
# On SIGTERM the leader exits cleanly (like a well-behaved process would), but
# the backgrounded python grandchild ignores SIGTERM (see stubborn_listener.py)
# and keeps holding its TCP port -- exactly the scenario where naive
# leader-only shutdown reports success while an orphan lingers.

PORT="${STUBBORN_PORT:-15561}"
SCRIPT_DIR=$(dirname "$0")

echo "LEADER_PID=$$"

trap 'echo LEADER_GOT_TERM; exit 0' TERM

python3 "$SCRIPT_DIR/stubborn_listener.py" "$PORT" &
CHILD_PID=$!

wait "$CHILD_PID"
