# GPTGrok2API Go

GPTGrok2API Go 是一个自托管的 OpenAI 兼容网关，使用 Go 运行时接入 OpenAI/ChatGPT JWT 账号池、Grok SSO/OAuth 账号池和代理出口，并提供 Web 控制台、账号调度、图片存储、实时监控与日志管理。

当前版本：`1.2.1-go`

## 能力

- OpenAI 兼容的 Chat Completions、Responses、Anthropic Messages。
- GPT 文本对话、`gpt-image-2` 图片生成和图片编辑。
- Grok 聊天、Responses、图片、图片编辑和视频。
- 账号池、限流冷却、失败反馈、代理绑定和并发调度。
- 图片本地存储、公开下载 URL、图片管理和批量清理。
- 实时显示入口排队、账号等待、出口代理、上游准备、生成、下载和总耗时。
- Vue 管理控制台、Redis 队列、健康检查和 Docker Compose 部署。

## Docker 部署

要求 Docker Engine 24+、Docker Compose v2、4 GB 以上可用内存，以及可访问 ChatGPT/Grok 的网络出口。

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git
cd gptGrok2api
cp .env.example .env
~~~

在 `.env` 中至少设置：

~~~dotenv
CHATGPT2API_AUTH_KEY=change-this-api-key
CHATGPT2API_ADMIN_KEY=change-this-admin-key
CHATGPT2API_GO_PORT=8000
GO_PUBLIC_BASE_URL=http://your-server-ip:8000
GO_VERSION=1.2.1-go
~~~

启动 Go 版：

~~~bash
docker compose -f docker-compose.go.yml up -d --build
docker compose -f docker-compose.go.yml ps
curl -fsS http://127.0.0.1:8000/health
~~~

Compose 包含 Go API、Redis 和图片网关。主 API 端口由 `CHATGPT2API_GO_PORT` 映射，图片网关默认只监听本机 3001。

~~~bash
docker logs -f gptgrok2api-go
docker compose -f docker-compose.go.yml logs -f image-gateway
~~~

## API

所有接口默认使用 Bearer 认证：

~~~bash
export API_KEY='your-api-key'
curl http://127.0.0.1:8000/v1/models \
  -H "Authorization: Bearer $API_KEY"
~~~

文本聊天：

~~~bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"你好"}]}'
~~~

### GPT 图片聊天

`gpt-image-2` 可通过 `/v1/chat/completions` 调用。纯文本提示词直接使用字符串；只有 `image_url`、`input_image` 或 `image` 内容块会进行图片/Base64 解析。

~~~bash
curl http://127.0.0.1:8000/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","messages":[{"role":"user","content":"画一个蓝色方块"}]}'
~~~

返回图片链接形如：

~~~text
http://your-server:8000/v1/files/image?id=<image-id>
~~~

该链接由 Go 服务直接提供下载，不依赖上游临时链接。

### 图片生成和编辑

~~~bash
curl http://127.0.0.1:8000/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"画一只猫","size":"1024x1024"}'
~~~

图片编辑必须使用 `multipart/form-data`：

~~~bash
curl http://127.0.0.1:8000/v1/images/edits \
  -H "Authorization: Bearer $API_KEY" \
  -F 'model=gpt-image-2' \
  -F 'prompt=把背景改成蓝色' \
  -F 'image=@input.png;type=image/png'
~~~

### 主要路由

| 功能 | 方法和路径 |
| --- | --- |
| 健康检查 | `GET /health` |
| 模型列表 | `GET /v1/models` |
| 对话补全 | `POST /v1/chat/completions` |
| Responses | `POST /v1/responses` |
| Anthropic Messages | `POST /v1/messages` |
| 图片生成 | `POST /v1/images/generations` |
| 图片编辑 | `POST /v1/images/edits` |
| 图片下载 | `GET /v1/files/image?id=...` |
| 实时监控 | `GET /api/monitor/realtime` |
| 日志管理 | `GET /api/logs` |
| 图片管理 | `GET /api/images` |
| 管理控制台 | `GET /` |

管理路由需要 `CHATGPT2API_ADMIN_KEY`。

## Go 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CHATGPT2API_AUTH_KEY` | 无 | API 认证密钥 |
| `CHATGPT2API_ADMIN_KEY` | 无 | 管理密钥 |
| `CHATGPT2API_GO_PORT` | `3000` | 宿主机端口 |
| `GO_LISTEN_ADDR` | `:8080` | 容器内监听地址 |
| `GO_PUBLIC_BASE_URL` | 空 | 图片公开 URL 基础地址 |
| `GO_CONFIG_PATH` | `/app/data/config.json` | 配置文件路径 |
| `GO_ACCOUNTS_PATH` | `data/accounts.json` | OpenAI 账号文件 |
| `GO_AUTH_KEYS_PATH` | `data/auth_keys.json` | 用户密钥文件 |
| `GO_REQUEST_TIMEOUT_SECONDS` | `180` | 上游超时，最大 300 秒 |
| `GO_CHAT_MAX_RETRIES` | `2` | 聊天/图片最大重试次数 |
| `GO_QUEUE_BACKEND` | `redis` | `redis` 或 `json` |
| `GO_REDIS_ADDR` | `redis:6379` | Redis 地址 |
| `GO_PROXY_URL` | 空 | 默认代理 |
| `GO_PROXY_POOL` | 空 | 逗号分隔代理池 |
| `GO_OPENAI_BASE_URL` | `https://chatgpt.com` | ChatGPT 上游 |
| `GO_VERSION` | `1.2.1-go` | 版本标识 |

完整示例见 `.env.example` 和 `config.example.yaml`。

## 服务器 8000 端口

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git /opt/gpt2api-go
cd /opt/gpt2api-go
cp .env.example .env
docker compose -f docker-compose.go.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

服务器配置示例：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_GO_PORT=8000
GO_PUBLIC_BASE_URL=http://23.148.212.231:8000
GO_VERSION=1.2.1-go
~~~

更新前备份 `/opt/gpt2api-go/data`，更新后检查 `/health`、`/v1/models` 和 `/v1/files/image?id=...`。

## 本地开发

Go 版不需要 Python 或 Uvicorn：

~~~bash
go run ./cmd/gptgrok2api
go test ./internal/...
CGO_ENABLED=0 go build -trimpath -o gptgrok2api ./cmd/gptgrok2api
~~~

前端源码位于 `web-vue/`，Docker 构建时会自动生成 `web_dist/`。

## 数据安全

以下运行时数据不会提交到 Git：

~~~text
.env
config.json
data/
logs/
~~~

`data/` 可能包含账号 Token、Cookie、OAuth 凭据、图片和管理密钥。生产环境请使用随机密钥，不要上传运行时数据，只通过 Nginx/HTTPS 暴露 API，并定期备份 `data/`。

## 许可证

请保留 `LICENSE` 和 `GROK2API_LICENSE`。项目基于 [yukkcat/chatgpt2api](https://github.com/yukkcat/chatgpt2api) 开发，并包含 GPTGrok2API Go 自有修改。

