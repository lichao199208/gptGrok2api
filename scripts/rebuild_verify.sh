#!/bin/bash
cd /opt/gptGrok2api
docker compose -f docker-compose.warp.yml build app 2>&1 | tail -2
docker compose -f docker-compose.warp.yml up -d app 2>&1 | tail -2
sleep 25
docker ps --filter name=chatgpt2api-warp --format '{{.Names}} {{.Status}}'
docker exec chatgpt2api-warp sh -c 'grep -c "invalid_auth_step" /app/services/register/openai_register.py'
docker exec chatgpt2api-warp sh -c 'grep -c "MailComMotherProvider" /app/services/register/mail_provider.py'
