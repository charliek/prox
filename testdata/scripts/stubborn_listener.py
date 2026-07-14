#!/usr/bin/env python3
"""Grandchild process that ignores SIGTERM and holds a real TCP listener.

Used by testdata/scripts/stubborn_grandchild.sh to reproduce the #29 orphan
scenario: a leader shell exits gracefully on SIGTERM while a backgrounded
grandchild ignores SIGTERM and keeps the port bound. It must be reaped with
SIGKILL -- this script deliberately has no clean way to exit otherwise.

Dependency-free: stdlib only (socket/signal/os/sys/time), so it needs no
virtualenv or installed packages in CI.

Markers printed (flush=True) once bound:
  GRANDCHILD_PID=<pid>
  LISTENING=<port>
"""
import os
import signal
import socket
import sys
import time


def main():
    if len(sys.argv) < 2:
        print("usage: stubborn_listener.py <port>", file=sys.stderr, flush=True)
        sys.exit(1)
    port = int(sys.argv[1])

    # Ignore SIGTERM: this process must survive a graceful shutdown attempt so
    # the integration tests can prove that Stop/Restart escalate to SIGKILL
    # for a stubborn grandchild that outlives its leader.
    signal.signal(signal.SIGTERM, signal.SIG_IGN)

    sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    # Deliberately do NOT set SO_REUSEADDR: a surviving listener must
    # genuinely block a rebind attempt on the same port, so a successful
    # rebind after restart/stop is proof the old grandchild is truly gone.
    sock.bind(("127.0.0.1", port))
    sock.listen(1)

    print(f"GRANDCHILD_PID={os.getpid()}", flush=True)
    print(f"LISTENING={port}", flush=True)

    while True:
        time.sleep(1)


if __name__ == "__main__":
    main()
