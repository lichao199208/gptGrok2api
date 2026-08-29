#!/usr/bin/env bash
set +e
BASE="http://127.0.0.1:3010"
API="test-api-key"
ADMIN="test-admin-key"

call() {
  method="$1"
  path="$2"
  key="$3"
  data="${4:-}"
  body=$(mktemp)
  if [ -n "$data" ]; then
    code=$(curl -sS --max-time 15 -o "$body" -w "%{http_code}" -X "$method" \
      -H "Authorization: Bearer $key" -H "Content-Type: application/json" \
      --data "$data" "$BASE$path")
  else
    code=$(curl -sS --max-time 15 -o "$body" -w "%{http_code}" -X "$method" \
      -H "Authorization: Bearer $key" "$BASE$path")
  fi
  printf '%-6s %-48s %s ' "$method" "$path" "$code"
  tr '\n' ' ' < "$body" | head -c 260
  printf '\n'
  rm -f "$body"
}

echo "NEW_ENDPOINTS"
call GET /health "$API"
call GET /api/model-catalog "$ADMIN"
call GET /api/third-party-apps "$API"
call GET /api/proxy/runtime "$ADMIN"
call GET /api/logs "$ADMIN"
call GET /api/runtime-logs "$ADMIN"
call GET /api/prompts "$API"
call GET /api/admin/prompt-sources "$ADMIN"
call POST /api/settings/retention-cleanup/preview "$ADMIN" '{}'
call POST /api/settings/account-cleanup/preview "$ADMIN" '{}'
call GET /api/image-tasks "$API"
call GET /api/image-tasks/quota "$API"
call GET /v1/editable-file-tasks "$API"

echo "ACTIONS"
call POST /api/image-tasks "$API" '{"client_task_id":"blackbox-1","prompt":"test"}'
call GET '/api/image-tasks?ids=blackbox-1' "$API"
call POST /api/settings/retention-cleanup/run "$ADMIN" '{}'
call POST /v1/search "$API" '{"prompt":"test"}'

echo "PORTS"
for port in 3000 3001 3002 3010; do
  code=$(curl -sS --max-time 5 -o /dev/null -w "%{http_code}" "http://127.0.0.1:$port/health")
  echo "$port $code"
done
