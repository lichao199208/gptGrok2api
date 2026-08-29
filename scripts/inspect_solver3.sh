#!/bin/bash
echo "=== launch approach in flow.py ==="
grep -n "launch\|executable_path\|cloakbrowser\|channel\|headless" /app/xai_browser/flow.py | head -30
echo "=== /root/.cloakbrowser ==="
docker exec chatgpt2api-captcha-solver sh -c 'ls /root/.cloakbrowser 2>/dev/null; ls /root/.cloakbrowser/* 2>/dev/null | head'
