FROM node:22-alpine AS web-build

WORKDIR /src/web-vue
COPY web-vue/package.json web-vue/package-lock.json ./
RUN npm ci
COPY VERSION /src/VERSION
COPY CHANGELOG.md /src/CHANGELOG.md
COPY web-vue ./
COPY web-vue/tsconfig.json /src/web-vue/tsconfig.json
RUN npm run build

FROM golang:1.23-alpine AS go-build
WORKDIR /src
COPY go.mod ./
COPY go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gptgrok2api ./cmd/gptgrok2api

FROM alpine:3.21 AS app
RUN adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=go-build /gptgrok2api /app/gptgrok2api
COPY --from=web-build /src/web-vue/dist /app/web_dist
COPY VERSION CHANGELOG.md config.example.yaml ./
COPY services/default_prompt_library.json /app/services/default_prompt_library.json
RUN mkdir -p /app/data /app/logs && chown -R app:app /app
USER app
ENV GO_LISTEN_ADDR=:80 \
    GO_STATIC_DIR=/app/web_dist \
    GO_CONFIG_PATH=/app/data/config.json \
    GO_AUTH_KEYS_PATH=/app/data/auth_keys.json \
    GROK_DATA_DIR=/app/data \
    GO_QUEUE_BACKEND=json
EXPOSE 80
ENTRYPOINT ["/app/gptgrok2api"]
