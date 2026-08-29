#!/bin/bash
echo "=== Dockerfile ==="
cat /opt/gptGrok2api/Dockerfile 2>/dev/null | head -40
echo "=== entrypoint / run ==="
docker inspect chatgpt2api-warp --format '{{.Config.Cmd}} {{.Config.Entrypoint}} {{.Config.WorkingDir}}' 2>&1
echo "=== venvs in container ==="
docker exec chatgpt2api-warp sh -c 'ls / ; ls /venv/bin 2>/dev/null | head; ls /opt/venv/bin 2>/dev/null | head; cat /proc/1/cmdline 2>/dev/null | tr "\0" " "'
