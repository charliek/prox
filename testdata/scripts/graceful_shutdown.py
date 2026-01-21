#!/usr/bin/env python3
"""Test script that prints distinctive shutdown messages when receiving SIGTERM.

This script is used to verify that prox captures output from grandchild processes
during graceful shutdown. When run through a shell (sh -c "python3 script.py"),
the Python process is a grandchild of prox. The shell exits quickly on SIGTERM,
but the Python process should have time to print its shutdown messages.
"""
import signal
import sys
import time
import os

def handler(signum, frame):
    # Print distinctive shutdown messages that we can verify in tests
    print("GRACEFUL_SHUTDOWN_START", flush=True)
    print(f"GRACEFUL_SHUTDOWN_PID={os.getpid()}", flush=True)
    time.sleep(0.2)  # Brief delay to simulate cleanup
    print("GRACEFUL_SHUTDOWN_COMPLETE", flush=True)
    sys.exit(0)

signal.signal(signal.SIGTERM, handler)
signal.signal(signal.SIGINT, handler)

print(f"PROCESS_STARTED_PID={os.getpid()}", flush=True)
while True:
    time.sleep(1)
