#!/bin/bash
echo "=== find xai_browser ==="
find /app -maxdepth 3 -name "flow.py" -path "*xai*" 2>/dev/null
find /app -maxdepth 3 -type d -name "*browser*" 2>/dev/null
echo "=== ms-playwright dirs ==="
ls -la /ms-playwright/ 2>&1 | head
echo "=== env PLAYWRIGHT ==="
env | grep -i playwright
echo "=== where is python ==="
which python3
pip show playwright 2>&1 | head -4
