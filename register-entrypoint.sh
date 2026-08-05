#!/bin/bash
set -e
Xvfb :99 -screen 0 1366x768x24 >/dev/null 2>&1 &
sleep 2
cd /app
exec python register.py "$@"
