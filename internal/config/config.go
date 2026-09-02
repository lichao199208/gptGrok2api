package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	RootDir                string
	ListenAddr             string
	DataDir                string
	StaticDir              string
	ConfigPath             string
	AccountsPath           string
	AuthKeysPath           string
	APIKey                 string
	AdminKey               string
	WebUIKey               string
	UpstreamURL            string
	UpstreamAPIKey         string
	OpenAIBaseURL          string
	OpenAIOAuthURL         string
	OpenAIAuthBaseURL      string
	OpenAIPlatformBaseURL  string
	OpenAILoginTokenURL    string
	OpenAIAgentRegisterURL string
	GrokChatURL            string
	GrokRateLimitsURL      string
	XAICLIBaseURL          string
	XAICLITokenURL         string
	ConsoleURL             string
	MediaChatURL           string
	MediaPostURL           string
	AssetUploadURL         string
	AssetsBaseURL          string
	ImagineWSURL           string
	ProxyURL               string
	ProxyPool              []string
	FallbackProxy          string
	ProxyGroups            []ProxyGroup
	ResourceProxyURL       string
	ResourceProxyPool      []string
	ProxyUpstreamsFile     string
	FlareSolverrURL        string
	ClearanceEnabled       bool
	ClearanceTimeout       time.Duration
	QueueBackend           string
	RedisAddr              string
	RedisPassword          string
	RedisDB                int
	OAuthDeviceURL         string
	OAuthTokenURL          string
	ImageDataDir           string
	VideoDataDir           string
	OAuthPath              string
	QueuePath              string
	RegisterPath           string
	GrokAccountsPath       string
	RegisterMailURL        string
	RegisterCaptchaURL     string
	RegisterDriverURL      string
	RegisterDriverKey      string
	Version                string
	AllowAnonymous         bool
	RequestTimeout         time.Duration
	ChatMaxRetries         int
	ChatRetryCodes         map[int]bool
	ImageAccountLimit      int
	ImageMaxConcurrency    int
	ImageRetentionDays     int
	ImageCleanupInterval   time.Duration
}

type ProxyGroup struct {
	ID       string
	Name     string
	Enabled  bool
	Strategy string
	Nodes    []ProxyNode
}

type ProxyNode struct {
	ID                    string
	Name                  string
	URL                   string
	Enabled               bool
	ImageConcurrencyLimit int
	LastStatus            int
	LastError             string
	RuntimeFailures       int
	RuntimeSuccesses      int
	RuntimeLatencyMS      int64
}

func Load(root string) (Config, error) {
	if strings.TrimSpace(root) == "" {
		root = strings.TrimSpace(os.Getenv("GROK_ROOT_DIR"))
	}
	if strings.TrimSpace(root) == "" {
		root, _ = os.Getwd()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Config{}, fmt.Errorf("resolve root: %w", err)
	}

	requestTimeoutSeconds := envInt("GO_REQUEST_TIMEOUT_SECONDS", 180)
	if requestTimeoutSeconds < 10 {
		requestTimeoutSeconds = 10
	}
	if requestTimeoutSeconds > 300 {
		requestTimeoutSeconds = 300
	}
	chatMaxRetries := envInt("GO_CHAT_MAX_RETRIES", 2)
	if chatMaxRetries < 0 {
		chatMaxRetries = 0
	}
	if chatMaxRetries > 3 {
		chatMaxRetries = 3
	}
	imageAccountConcurrency := envInt("GO_IMAGE_ACCOUNT_CONCURRENCY", 1)
	if imageAccountConcurrency < 1 {
		imageAccountConcurrency = 1
	}
	if imageAccountConcurrency > 4 {
		imageAccountConcurrency = 4
	}
	imageMaxConcurrency := envInt("GO_IMAGE_MAX_CONCURRENCY", 128)
	if imageMaxConcurrency < 1 {
		imageMaxConcurrency = 1
	}
	if imageMaxConcurrency > 1024 {
		imageMaxConcurrency = 1024
	}
	imageRetentionDays := envInt("GO_IMAGE_RETENTION_DAYS", 1)
	if imageRetentionDays < 1 {
		imageRetentionDays = 1
	}
	if imageRetentionDays > 3650 {
		imageRetentionDays = 3650
	}
	imageCleanupIntervalSeconds := envInt("GO_IMAGE_CLEANUP_INTERVAL_SECONDS", 3600)
	if imageCleanupIntervalSeconds < 60 {
		imageCleanupIntervalSeconds = 60
	}

	cfg := Config{
		RootDir:                root,
		ListenAddr:             env("GO_LISTEN_ADDR", env("CHATGPT2API_LISTEN_ADDR", ":8080")),
		DataDir:                resolvePath(root, env("GROK_DATA_DIR", "data")),
		StaticDir:              resolvePath(root, env("GO_STATIC_DIR", "web_dist")),
		ConfigPath:             resolvePath(root, env("GO_CONFIG_PATH", "config.json")),
		AccountsPath:           resolvePath(root, env("GO_ACCOUNTS_PATH", "data/accounts.json")),
		AuthKeysPath:           resolvePath(root, env("GO_AUTH_KEYS_PATH", "data/auth_keys.json")),
		APIKey:                 strings.TrimSpace(os.Getenv("CHATGPT2API_AUTH_KEY")),
		AdminKey:               strings.TrimSpace(os.Getenv("CHATGPT2API_ADMIN_KEY")),
		WebUIKey:               strings.TrimSpace(os.Getenv("CHATGPT2API_WEBUI_KEY")),
		UpstreamURL:            strings.TrimRight(strings.TrimSpace(os.Getenv("GO_UPSTREAM_URL")), "/"),
		UpstreamAPIKey:         strings.TrimSpace(os.Getenv("GO_UPSTREAM_API_KEY")),
		OpenAIBaseURL:          strings.TrimRight(env("GO_OPENAI_BASE_URL", "https://chatgpt.com"), "/"),
		OpenAIOAuthURL:         env("GO_OPENAI_OAUTH_TOKEN_URL", "https://auth.openai.com/oauth/token"),
		OpenAIAuthBaseURL:      strings.TrimRight(env("GO_OPENAI_AUTH_BASE_URL", "https://auth.openai.com"), "/"),
		OpenAIPlatformBaseURL:  strings.TrimRight(env("GO_OPENAI_PLATFORM_BASE_URL", "https://platform.openai.com"), "/"),
		OpenAILoginTokenURL:    env("GO_OPENAI_LOGIN_TOKEN_URL", "https://auth.openai.com/api/accounts/oauth/token"),
		OpenAIAgentRegisterURL: env("GO_OPENAI_AGENT_REGISTER_URL", "https://auth.openai.com/api/accounts/v1/agent/register"),
		GrokChatURL:            env("GO_GROK_CHAT_URL", "https://grok.com/rest/app-chat/conversations/new"),
		GrokRateLimitsURL:      env("GO_GROK_RATE_LIMITS_URL", "https://grok.com/rest/rate-limits"),
		XAICLIBaseURL:          strings.TrimRight(env("GO_XAI_CLI_BASE_URL", "https://cli-chat-proxy.grok.com/v1"), "/"),
		XAICLITokenURL:         env("GO_XAI_CLI_TOKEN_URL", "https://auth.x.ai/oauth2/token"),
		ConsoleURL:             env("GO_CONSOLE_RESPONSES_URL", "https://console.x.ai/v1/responses"),
		MediaChatURL:           env("GO_MEDIA_CHAT_URL", "https://grok.com/rest/app-chat/conversations/new"),
		MediaPostURL:           env("GO_MEDIA_POST_URL", "https://grok.com/rest/media/post/create"),
		AssetUploadURL:         env("GO_ASSET_UPLOAD_URL", "https://grok.com/rest/app-chat/upload-file"),
		AssetsBaseURL:          env("GO_ASSETS_BASE_URL", "https://assets.grok.com"),
		ImagineWSURL:           env("GO_IMAGINE_WS_URL", "wss://grok.com/ws/imagine/listen"),
		ProxyURL:               strings.TrimSpace(os.Getenv("GO_PROXY_URL")),
		ProxyPool:              splitList(os.Getenv("GO_PROXY_POOL")),
		ResourceProxyURL:       strings.TrimSpace(os.Getenv("GO_RESOURCE_PROXY_URL")),
		ResourceProxyPool:      splitList(os.Getenv("GO_RESOURCE_PROXY_POOL")),
		ProxyUpstreamsFile:     resolvePath(root, env("GO_PROXY_UPSTREAMS_FILE", "upstreams.txt")),
		FlareSolverrURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("GO_FLARESOLVERR_URL")), "/"),
		ClearanceEnabled:       envBool("GO_CLEARANCE_ENABLED", false),
		ClearanceTimeout:       time.Duration(envInt("GO_CLEARANCE_TIMEOUT_SECONDS", 60)) * time.Second,
		QueueBackend:           strings.ToLower(env("GO_QUEUE_BACKEND", "json")),
		RedisAddr:              env("GO_REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:          strings.TrimSpace(os.Getenv("GO_REDIS_PASSWORD")),
		RedisDB:                envIntAllowZero("GO_REDIS_DB", 0),
		OAuthDeviceURL:         env("GO_XAI_OAUTH_DEVICE_URL", "https://auth.x.ai/oauth2/device/code"),
		OAuthTokenURL:          env("GO_XAI_OAUTH_TOKEN_URL", "https://auth.x.ai/oauth2/token"),
		ImageDataDir:           resolvePath(root, env("GO_IMAGE_DATA_DIR", filepath.Join("data", "files", "images"))),
		VideoDataDir:           resolvePath(root, env("GO_VIDEO_DATA_DIR", filepath.Join("data", "files", "videos"))),
		OAuthPath:              resolvePath(root, env("GO_OAUTH_PATH", filepath.Join("data", "oauth_accounts.json.enc"))),
		QueuePath:              resolvePath(root, env("GO_QUEUE_PATH", filepath.Join("data", "tasks.json"))),
		RegisterPath:           resolvePath(root, env("GO_REGISTER_PATH", filepath.Join("data", "register.json"))),
		GrokAccountsPath:       resolvePath(root, env("GO_GROK_ACCOUNTS_PATH", filepath.Join("data", "grok_accounts.json"))),
		RegisterMailURL:        strings.TrimRight(strings.TrimSpace(os.Getenv("GO_REGISTER_MAIL_URL")), "/"),
		RegisterCaptchaURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("GO_REGISTER_CAPTCHA_URL")), "/"),
		RegisterDriverURL:      strings.TrimRight(strings.TrimSpace(os.Getenv("GO_REGISTER_DRIVER_URL")), "/"),
		RegisterDriverKey:      strings.TrimSpace(os.Getenv("GO_REGISTER_DRIVER_KEY")),
		Version:                env("GO_VERSION", "1.2.4-go"),
		AllowAnonymous:         envBool("GO_ALLOW_ANONYMOUS", false),
		RequestTimeout:         time.Duration(requestTimeoutSeconds) * time.Second,
		ChatMaxRetries:         chatMaxRetries,
		ChatRetryCodes:         parseStatusCodes(env("GO_CHAT_RETRY_CODES", "401,403,429,500,502,503,504")),
		ImageAccountLimit:      imageAccountConcurrency,
		ImageMaxConcurrency:    imageMaxConcurrency,
		ImageRetentionDays:     imageRetentionDays,
		ImageCleanupInterval:   time.Duration(imageCleanupIntervalSeconds) * time.Second,
	}

	rawConfig, err := readMap(cfg.ConfigPath)
	if os.IsNotExist(err) && filepath.Clean(cfg.ConfigPath) != filepath.Clean(filepath.Join(root, "config.json")) {
		// Older Docker deployments kept config.json at the project root. Copy it
		// into the writable data path before the first settings update.
		legacyPath := filepath.Join(root, "config.json")
		if legacyRaw, legacyErr := os.ReadFile(legacyPath); legacyErr == nil {
			var legacyConfig map[string]any
			if json.Unmarshal(legacyRaw, &legacyConfig) == nil {
				rawConfig = legacyConfig
				if mkdirErr := os.MkdirAll(filepath.Dir(cfg.ConfigPath), 0o755); mkdirErr == nil {
					_ = os.WriteFile(cfg.ConfigPath, legacyRaw, 0o600)
				}
				err = nil
			}
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if cfg.APIKey == "" {
		cfg.APIKey = firstString(rawConfig, "auth-key", "api_key")
	}
	if cfg.AdminKey == "" {
		cfg.AdminKey = firstString(rawConfig, "app_key", "admin_key")
	}
	if cfg.WebUIKey == "" {
		cfg.WebUIKey = firstString(rawConfig, "webui_key")
	}
	if cfg.APIKey == "" && cfg.AdminKey != "" {
		cfg.APIKey = cfg.AdminKey
	}
	applyProxyConfig(&cfg, rawConfig)

	return cfg, nil
}

func applyProxyConfig(cfg *Config, values map[string]any) {
	cfg.FallbackProxy = firstString(values, "fallback_proxy")
	cfg.ProxyGroups = parseProxyGroups(values["proxy_groups"])
	if proxyURL, ok := values["proxy"].(string); ok {
		if cfg.ProxyURL == "" {
			cfg.ProxyURL = strings.TrimSpace(proxyURL)
		}
		return
	}
	proxyValue, _ := values["proxy"].(map[string]any)
	if proxyValue == nil {
		proxyValue, _ = values["proxy_runtime"].(map[string]any)
	}
	if proxyValue == nil {
		return
	}
	if cfg.ProxyURL == "" {
		cfg.ProxyURL = firstString(proxyValue, "proxy_url", "url")
	}
	if len(cfg.ProxyPool) == 0 {
		cfg.ProxyPool = anyStringList(proxyValue["proxy_pool"])
	}
	if cfg.ResourceProxyURL == "" {
		cfg.ResourceProxyURL = firstString(proxyValue, "resource_proxy_url")
	}
	if len(cfg.ResourceProxyPool) == 0 {
		cfg.ResourceProxyPool = anyStringList(proxyValue["resource_proxy_pool"])
	}
}

// ApplyProxyConfig applies the persisted proxy portion of the configuration.
// It is exported so the HTTP runtime can hot-reload proxy changes without
// rebuilding the entire server configuration.
func ApplyProxyConfig(cfg *Config, values map[string]any) {
	if cfg == nil {
		return
	}
	applyProxyConfig(cfg, values)
}

func parseProxyGroups(value any) []ProxyGroup {
	rawGroups, _ := value.([]any)
	groups := make([]ProxyGroup, 0, len(rawGroups))
	for _, rawGroup := range rawGroups {
		groupMap, _ := rawGroup.(map[string]any)
		if groupMap == nil {
			continue
		}
		group := ProxyGroup{
			ID: firstString(groupMap, "id"), Name: firstString(groupMap, "name"),
			Enabled: configBool(groupMap["enabled"], true), Strategy: firstString(groupMap, "strategy"),
		}
		rawNodes, _ := groupMap["nodes"].([]any)
		for _, rawNode := range rawNodes {
			nodeMap, _ := rawNode.(map[string]any)
			if nodeMap == nil {
				continue
			}
			node := ProxyNode{
				ID: firstString(nodeMap, "id"), Name: firstString(nodeMap, "name"), URL: firstString(nodeMap, "url"),
				Enabled: configBool(nodeMap["enabled"], true), ImageConcurrencyLimit: configInt(nodeMap["image_concurrency_limit"]),
				LastStatus: configInt(nodeMap["last_status"]), LastError: firstString(nodeMap, "last_error"),
				RuntimeFailures:  configInt(nodeMap["runtime_failure_count"]),
				RuntimeSuccesses: configInt(nodeMap["runtime_success_count"]),
				RuntimeLatencyMS: int64(configInt(nodeMap["runtime_latency_ms"])),
			}
			if node.ImageConcurrencyLimit < 1 {
				node.ImageConcurrencyLimit = 3
			}
			group.Nodes = append(group.Nodes, node)
		}
		groups = append(groups, group)
	}
	return groups
}

func configBool(value any, fallback bool) bool {
	if parsed, ok := value.(bool); ok {
		return parsed
	}
	return fallback
}

func configInt(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := strconv.Atoi(typed.String())
		return parsed
	default:
		parsed, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value)))
		return parsed
	}
}

func parseStatusCodes(value string) map[int]bool {
	result := map[int]bool{}
	for _, part := range strings.Split(value, ",") {
		code, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && code >= 400 && code <= 599 {
			result[code] = true
		}
	}
	return result
}

func (c Config) RelativePath(path string) string {
	return resolvePath(c.RootDir, path)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func envIntAllowZero(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func resolvePath(root, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(root, value)
}

func readMap(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func splitList(value string) []string {
	result := []string{}
	for _, item := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func anyStringList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				result = append(result, text)
			}
		}
	case []string:
		for _, item := range typed {
			if text := strings.TrimSpace(item); text != "" {
				result = append(result, text)
			}
		}
	case string:
		return splitList(typed)
	}
	return result
}
