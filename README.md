# GPTGrok2API Go

GPTGrok2API Go 是一个自托管的 OpenAI 兼容网关。它使用 Go 运行时接入 ChatGPT/OpenAI JWT 账号池、Grok SSO/OAuth 账号池和代理出口，并提供账号调度、图片任务、文件存储、实时监控和 Web 管理控制台。

当前发布版本：<code>v1.2.4-go</code>  ·  [GitHub Releases](https://github.com/lichao199208/gptGrok2api/releases)

> 本项目通过逆向研究接入 ChatGPT 和 Grok 网页能力，不是 OpenAI 或 xAI 官方服务。上游协议、账号策略和可用模型随时可能变化。请使用你有权使用的账号，遵守相关服务条款和当地法律，并自行承担账号、网络和内容风险。

## 能力概览

- OpenAI 兼容接口：Chat Completions、Responses、Anthropic Messages、搜索、图片、视频和可编辑文件任务。
- OpenAI 图片：<code>gpt-image-2</code> 文生图、图生图和多参考图编辑。
- Grok：文本、Grok Imagine 图片、图片编辑、视频，以及 Console/Thinking 模型。
- 多账号池：JWT、OAuth refresh token、Grok SSO/OAuth、账号分组、失败换号、限流冷却和并发调度。
- 代理出口：默认代理、代理池、代理组、订阅导入、节点健康检测和图片任务专用并发限制。
- 管理控制台：账号、代理、图片图库、日志、实时请求、提示词、注册任务、备份和系统设置。
- 本地持久化：账号和配置使用 JSON 文件，队列可使用 JSON 或 Redis，图片和视频保存在 <code>data/files/</code>。

## 架构

~~~mermaid
flowchart LR
  Client[兼容 API 客户端] --> API[/v1 API]
  Admin[管理员 / Web 控制台] --> Console[/api 管理接口]
  API --> Core[Go 网关与账号调度]
  Console --> Core
  Core --> ChatGPT[ChatGPT Web API]
  Core --> Grok[Grok Web / Console API]
  Core --> Proxy[代理出口与健康检测]
  Core --> Files[data/files 图片和视频]
  Core --> Queue[JSON 或 Redis 队列]
  Gateway[可选图片队列网关] --> Queue
  Gateway --> API
~~~

## Docker 快速开始

要求 Docker Engine 24+、Docker Compose v2，以及能够访问 ChatGPT/Grok 的网络出口。

~~~bash
git clone https://github.com/lichao199208/gptGrok2api.git
cd gptGrok2api
cp .env.example .env
mkdir -p data logs
test -f data/auth_keys.json || printf '{"items":[]}\n' > data/auth_keys.json
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
~~~

编辑 <code>.env</code>，至少设置两个不同的随机密钥：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-api-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_GO_PORT=3000
GO_PUBLIC_BASE_URL=http://your-server:3000
~~~

启动 Go 服务、Redis 和图片队列网关：

~~~bash
docker compose -f docker-compose.go.yml up -d --build
docker compose -f docker-compose.go.yml ps
curl -fsS http://127.0.0.1:3000/health
~~~

管理控制台地址为 <code>http://服务器地址:3000/</code>。图片队列网关默认只监听 <code>127.0.0.1:3001</code>，不应直接暴露到公网；普通客户端直接访问主服务的 <code>/v1</code> 接口即可。

### 服务器 8000 端口

服务器部署使用 Compose 覆盖文件。覆盖文件会把主服务绑定到本机 <code>127.0.0.1:8000</code>，适合由 Nginx 或其他 HTTPS 反向代理对外提供服务：

~~~bash
cp .env.example .env
mkdir -p data logs
test -f data/auth_keys.json || printf '{"items":[]}\n' > data/auth_keys.json
docker network inspect gptgrok2api_default >/dev/null 2>&1 || docker network create gptgrok2api_default
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

服务器 <code>.env</code> 示例：

~~~dotenv
CHATGPT2API_AUTH_KEY=replace-with-a-long-random-api-key
CHATGPT2API_ADMIN_KEY=replace-with-a-different-admin-key
CHATGPT2API_PORT=8000
GO_PUBLIC_BASE_URL=https://gpt.example.com
GO_VERSION=1.2.4-go
~~~

<code>CHATGPT2API_PORT</code> 是服务器覆盖文件使用的宿主机端口；本地单 Compose 部署使用 <code>CHATGPT2API_GO_PORT</code>。首次接入域名时，请确认反向代理把 <code>/</code>、<code>/v1</code>、<code>/api</code> 和 <code>/images</code> 一并转发到 <code>127.0.0.1:8000</code>。

## 认证和控制台

AI 接口支持以下认证方式：

~~~http
Authorization: Bearer <api-key>
~~~

也兼容 <code>X-API-Key</code>。<code>CHATGPT2API_AUTH_KEY</code> 用于普通 API 调用，<code>CHATGPT2API_ADMIN_KEY</code> 用于管理接口；控制台中创建的用户密钥会保存为哈希，不会以明文写入 <code>data/auth_keys.json</code>。除非明确需要，不要开启 <code>GO_ALLOW_ANONYMOUS</code>。

## API

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| <code>GET</code> | <code>/health</code> | 健康检查 |
| <code>GET</code> | <code>/v1/models</code> | 当前模型目录 |
| <code>POST</code> | <code>/v1/chat/completions</code> | 文本、图片聊天和工具调用 |
| <code>POST</code> | <code>/v1/responses</code> | Responses 兼容接口 |
| <code>POST</code> | <code>/v1/messages</code> | Anthropic Messages 兼容接口 |
| <code>POST</code> | <code>/v1/search</code> | 搜索请求 |
| <code>POST</code> | <code>/v1/images/generations</code> | 文生图 |
| <code>POST</code> | <code>/v1/images/edits</code> | 图生图/图片编辑 |
| <code>GET</code> | <code>/v1/files/image?id=...</code> | 下载生成图片 |
| <code>POST</code> | <code>/v1/videos</code> | 创建视频任务 |
| <code>GET</code> | <code>/v1/videos/{id}</code> | 查询视频任务 |
| <code>POST</code> | <code>/v1/editable-file-tasks</code> | 创建 PPT/PSD 等可编辑文件任务 |
| <code>GET</code> | <code>/files/{path}</code> | 下载可编辑文件产物 |

可用模型以 <code>/v1/models</code> 返回值和账号实际权限为准。当前目录按能力分为 OpenAI GPT 文本、Grok 文本、<code>gpt-image-2</code>/Grok Imagine 图片、Grok Imagine 视频和 Grok Console/Thinking 模型，不建议在客户端硬编码完整模型清单。

### 文本聊天

~~~bash
curl http://127.0.0.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5","messages":[{"role":"user","content":"介绍一下这个项目"}]}'
~~~

## 图片生成、编辑与参考图逻辑

### 调用示例

文生图：

~~~bash
curl http://127.0.0.1:3000/v1/images/generations \
  -H 'Authorization: Bearer your-api-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-image-2","prompt":"一只漂浮在太空里的猫","size":"1024x1024","n":1}'
~~~

图片编辑使用 <code>multipart/form-data</code>，可以提交一张或多张 <code>image</code>：

~~~bash
curl http://127.0.0.1:3000/v1/images/edits \
  -H 'Authorization: Bearer your-api-key' \
  -F 'model=gpt-image-2' \
  -F 'prompt=保留主体，把背景改成蓝色' \
  -F 'image=@reference.png;type=image/png'
~~~

<code>gpt-image-2</code> 也可以通过 <code>/v1/chat/completions</code> 调用。纯文本 content 是提示词；只有 <code>image_url</code>、<code>input_image</code> 或 <code>image</code> 内容块会被当作参考图输入。

### 结果筛选规则

参考图和生成图在 ChatGPT 上游响应中都可能表现为 <code>file-service://</code> 或 <code>sediment://</code> 资产指针，且 SSE 过程中可能先回显上传的参考图。为避免“把参考图当成生成结果”，Go 版按以下规则处理：

1. 请求中的客户端 <code>user</code> 图片只用于上传和生成，不加入结果列表。
2. 轮询 conversation 的 <code>mapping</code> 时，只读取 <code>tool</code> 和 <code>assistant</code> 记录中的图片资产；<code>user</code> 记录全部忽略。
3. 按消息 <code>create_time</code> 排序，并过滤 SSE 回显、输入资产指针和与参考图字节相同的内容。
4. 识别 <code>file-service://</code>、<code>sediment://</code> 以及当前文件 ID 结构，下载真正的生成资产。
5. 生成资产写入本地图片存储，API 返回本站 URL，而不是上游临时下载地址。

因此，编辑请求的参考图只会参与生成，不会出现在返回的 <code>data</code> 图片列表中。默认返回格式是 URL，例如：

~~~text
http://your-server:3000/v1/files/image?id=<image-id>
~~~

公网部署时请设置 <code>GO_PUBLIC_BASE_URL</code> 为外部 HTTPS 地址。图片文件默认保留 1 天，后台定期清理；可用 <code>GO_IMAGE_RETENTION_DAYS</code> 和 <code>GO_IMAGE_CLEANUP_INTERVAL_SECONDS</code> 调整。

## 账号、重试和代理

- 图片和聊天请求遇到 <code>401</code>、<code>403</code>、<code>429</code>、<code>500</code>、<code>502</code>、<code>503</code>、<code>504</code> 或网络错误时，会排除当前账号并按限制尝试其他账号。
- OAuth 账号优先使用 <code>refresh_token</code> 刷新 access token，并持久化新 token；Go 版不会使用账户密码或 2FA Secret 自动登录生成 token。
- 图片请求默认单账号并发为 <code>1</code>，单进程总并发为 <code>128</code>。代理组会按真实请求的成功率、延迟和并发容量选择节点。
- 代理订阅支持每行一个 HTTP/HTTPS/SOCKS 节点或 Base64 编码列表，自动去重并保留手工节点；节点检测结果会保存到配置。
- 代理、账号 Token、Cookie 和生成文件都属于敏感数据，请只在可信网络中使用，并通过 HTTPS 暴露服务。

## 主要环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| <code>CHATGPT2API_AUTH_KEY</code> | 空 | 普通 API 密钥 |
| <code>CHATGPT2API_ADMIN_KEY</code> | 空 | 管理密钥 |
| <code>CHATGPT2API_GO_PORT</code> | <code>3000</code> | <code>docker-compose.go.yml</code> 宿主机端口 |
| <code>CHATGPT2API_PORT</code> | <code>8000</code> | 服务器覆盖 Compose 的宿主机端口 |
| <code>GO_PUBLIC_BASE_URL</code> | 空 | 图片和文件公开 URL 前缀 |
| <code>GO_CONFIG_PATH</code> | <code>config.json</code> | JSON 配置文件路径 |
| <code>GO_ACCOUNTS_PATH</code> | <code>data/accounts.json</code> | OpenAI 账号文件 |
| <code>GO_AUTH_KEYS_PATH</code> | <code>data/auth_keys.json</code> | 用户密钥文件 |
| <code>GO_QUEUE_BACKEND</code> | <code>json</code> | <code>json</code> 或 <code>redis</code> |
| <code>GO_REDIS_ADDR</code> | <code>127.0.0.1:6379</code> | Redis 地址 |
| <code>GO_REQUEST_TIMEOUT_SECONDS</code> | <code>180</code> | 上游请求超时，范围 10-300 秒 |
| <code>GO_CHAT_MAX_RETRIES</code> | <code>2</code> | 聊天/图片最大重试次数 |
| <code>GO_IMAGE_ACCOUNT_CONCURRENCY</code> | <code>1</code> | 单账号图片并发，范围 1-4 |
| <code>GO_IMAGE_MAX_CONCURRENCY</code> | <code>128</code> | 单进程图片总并发，范围 1-1024 |
| <code>GO_IMAGE_RETENTION_DAYS</code> | <code>1</code> | 本地图片保留天数 |
| <code>GO_IMAGE_CLEANUP_INTERVAL_SECONDS</code> | <code>3600</code> | 图片清理间隔，最少 60 秒 |
| <code>GO_PROXY_URL</code> | 空 | 默认代理 |
| <code>GO_PROXY_POOL</code> | 空 | 逗号分隔的代理池 |
| <code>GO_VERSION</code> | <code>1.2.4-go</code> | 版本标识 |

完整配置项和上游地址见 [<code>.env.example</code>](./.env.example) 与 [<code>config.example.yaml</code>](./config.example.yaml)。运行时配置通过控制台保存到 <code>config.json</code>，不要把真实配置提交到 Git。

## 升级与备份

升级前先备份 <code>data/</code>、<code>.env</code> 和自定义反向代理配置。然后在仓库目录执行：

~~~bash
git fetch --tags user-origin
git checkout v1.2.4-go
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml pull
docker compose -f docker-compose.go.yml -f deploy/docker-compose.server.yml up -d --build
curl -fsS http://127.0.0.1:8000/health
~~~

如果使用本地构建而非服务器覆盖文件，去掉第二个 Compose 文件并把端口检查改为 <code>CHATGPT2API_GO_PORT</code> 对应的端口。不要删除 <code>data/</code>，其中包含账号、密钥、队列和图片。

## 本地开发与测试

Go 版运行不需要 Python 或 Uvicorn：

~~~bash
go run ./cmd/gptgrok2api
go test ./internal/... ./cmd/...
CGO_ENABLED=0 go build -trimpath -o gptgrok2api ./cmd/gptgrok2api
~~~

前端源码在 <code>web-vue/</code>，需要 Node.js 22+：

~~~bash
cd web-vue
npm ci
npm run build
~~~

## 数据安全

以下内容只属于本地运行时数据，不要提交到 Git：

~~~text
.env
config.json
data/
logs/
web_dist/
~~~

<code>data/</code> 可能包含 OpenAI/Grok Token、Cookie、OAuth 凭据、管理密钥、调用日志和生成图片。生产环境请使用随机密钥、限制管理接口来源、启用 HTTPS，并定期备份数据目录。

## 上游与许可证

本项目参考并继承了 [yukkcat/chatgpt2api](https://github.com/yukkcat/chatgpt2api) 的接口和业务思路，当前 Go 版代码与修改以本仓库为准。请保留仓库中的 [<code>LICENSE</code>](./LICENSE) 和 [<code>GROK2API_LICENSE</code>](./GROK2API_LICENSE) 文件，并按其中条款使用和分发。
