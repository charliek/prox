#!/bin/bash
echo "Starting long running process"
trap 'echo "Received SIGTERM"; exit 0' SIGTERM
while true; do
    echo "Still running..."
    sleep 1
done
