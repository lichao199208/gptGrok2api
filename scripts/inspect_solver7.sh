#!/bin/bash
docker exec chatgpt2api-captcha-solver sh -c 'grep -rn "browser_kwargs" /app/xai_browser/ | head'
docker exec chatgpt2api-captcha-solver sh -c 'grep -rn "def browser_kwargs" -A 50 /app/common/*.py /app/xai_browser/*.py 2>/dev/null | head -70'
