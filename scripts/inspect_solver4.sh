#!/bin/bash
docker exec chatgpt2api-captcha-solver sh -c 'grep -n "launch\|cloak\|executable_path\|channel\|headless\|chromium" /app/xai_browser/flow.py | head -40'
