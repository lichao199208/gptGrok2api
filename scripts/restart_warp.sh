#!/bin/bash
docker restart chatgpt2api-warp
sleep 18
docker ps --filter name=chatgpt2api-warp --format '{{.Names}} {{.Status}}'
docker exec chatgpt2api-warp sh -c 'grep -c "DIAG invalid_auth_step" /app/services/register/openai_register.py'
