#!/bin/bash
docker inspect chatgpt2api-captcha-solver --format '{{json .Mounts}}' 2>&1 | python3 -m json.tool 2>/dev/null | head -40
echo "=== workdir ==="
docker inspect chatgpt2api-captcha-solver --format '{{.Config.WorkingDir}} {{.Config.Cmd}} {{.Config.Entrypoint}}' 2>&1
echo "=== find flow.py in container ==="
docker exec chatgpt2api-captcha-solver sh -c 'find / -maxdepth 4 -name "flow.py" -path "*xai*" 2>/dev/null; ls /opt 2>/dev/null'
