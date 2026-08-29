#!/bin/bash
docker exec chatgpt2api-captcha-solver sh -c 'grep -n "def browser_kwargs" -A 40 /app/xai_browser/flow.py | head -60'
