#!/bin/bash
echo "=== /ms-playwright ==="
ls /ms-playwright/ 2>/dev/null
echo "=== browser revisions ==="
grep -rE "chromium|headless|executablePath|revision" /app/xai_browser/*.py /app/captcha-solver/xai_browser/*.py 2>/dev/null | head -20
echo "=== playwright version pin ==="
pip show playwright 2>/dev/null | head -3
cat /app/requirements*.txt 2>/dev/null | grep -i playwright
