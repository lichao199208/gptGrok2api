package httpapi

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/agentidentity"
	"github.com/auucoder/gptgrok2api-go/internal/auth"
	"github.com/auucoder/gptgrok2api-go/internal/config"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/oauth"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	registerruntime "github.com/auucoder/gptgrok2api-go/internal/register"
	"github.com/auucoder/gptgrok2api-go/internal/store"
	"github.com/auucoder/gptgrok2api-go/internal/tasks"
)

type Server struct {
	cfg                config.Config
	auth               *auth.Validator
	store              *store.Store
	catalog            []model.Spec
	client             *http.Client
	requestClient      *http.Client
	accountPool        *accounts.Pool
	chatProvider       *provider.GrokChat
	consoleProvider    *provider.ConsoleChat
	mediaProvider      *provider.Media
	openAIImage        *provider.OpenAIImage
	openAIChat         *provider.OpenAIChat
	gptMail            *provider.GPTMail
	xaiProbe           *provider.XAIProbe
	grokQuota          *provider.GrokQuota
	proxyManager       *proxyruntime.Manager
	oauthStore         *oauth.Store
	openAILogin        *oauth.OpenAILogin
	agentIdentityStore *agentidentity.Store
	deviceOAuth        *oauth.DeviceService
	taskQueue          tasks.QueueAPI
	registerStore      *registerruntime.Store
	registerRuntime    *registerruntime.Runtime
	monitor            *runtimeMonitor
	logMu              sync.Mutex
	videoMu            sync.RWMutex
	videoJobs          map[string]*videoJob
	imageTaskMu        sync.RWMutex
	imageTasks         map[string]*imageTaskState
	imageSlots         chan struct{}
	fileTaskMu         sync.RWMutex
	fileTasks          map[string]*editableFileTaskState
	schedulerMu        sync.Mutex
	schedulerLeases    map[string]map[string]any
	external           *externalManager
	refreshMu          sync.RWMutex
	refreshProgress    map[string]*accountRefreshProgress
	survivalMu         sync.RWMutex
	survivalStatus     map[string]any
	survivalRunning    bool
	survivalWake       chan struct{}
	probeStop          chan struct{}
	probeWake          chan struct{}
	proxyProbeURL      string
}

func New(cfg config.Config) *Server {
	repository := store.New(cfg.AccountsPath, cfg.AuthKeysPath, cfg.ConfigPath)
	proxyManager := proxyruntime.NewManager(cfg.ProxyURL, cfg.ProxyPool)
	groups := runtimeProxyGroups(cfg.ProxyGroups)
	proxyManager.ConfigureImageGroups(cfg.FallbackProxy, groups)
	proxyManager.SetResource(cfg.ResourceProxyURL, cfg.ResourceProxyPool)
	proxyManager.SetUpstreamsFile(cfg.ProxyUpstreamsFile)
	proxyTransport := proxyruntime.NewTransport(http.DefaultTransport)
	requestClient := &http.Client{Transport: proxyTransport, Timeout: cfg.RequestTimeout}
	var taskQueue tasks.QueueAPI = tasks.New(cfg.QueuePath)
	if cfg.QueueBackend == "redis" {
		redisQueue := tasks.NewRedis(cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB, "gptgrok2api")
		if err := redisQueue.Ping(); err != nil {
			log.Printf("Redis queue unavailable, using JSON queue: %v", err)
		} else {
			taskQueue = redisQueue
		}
	}
	server := &Server{
		cfg:                cfg,
		auth:               auth.New(cfg.APIKey, cfg.AdminKey, cfg.AuthKeysPath, cfg.AllowAnonymous, repository),
		store:              repository,
		catalog:            model.Catalog(),
		client:             &http.Client{Timeout: 0},
		requestClient:      requestClient,
		accountPool:        accounts.New(repository),
		chatProvider:       provider.NewGrokChat(cfg.GrokChatURL, requestClient, cfg.RequestTimeout),
		consoleProvider:    provider.NewConsoleChat(cfg.ConsoleURL, requestClient),
		mediaProvider:      provider.NewMedia(requestClient, cfg.MediaChatURL, cfg.MediaPostURL, cfg.AssetUploadURL, cfg.AssetsBaseURL, cfg.RequestTimeout),
		openAIImage:        provider.NewOpenAIImage(cfg.OpenAIBaseURL, requestClient, proxyManager, cfg.RequestTimeout),
		gptMail:            provider.NewGPTMail(requestClient),
		xaiProbe:           provider.NewXAIProbe(cfg.XAICLIBaseURL, cfg.XAICLITokenURL, requestClient),
		grokQuota:          provider.NewGrokQuota(cfg.GrokRateLimitsURL, requestClient, proxyManager),
		proxyManager:       proxyManager,
		videoJobs:          map[string]*videoJob{},
		imageTasks:         map[string]*imageTaskState{},
		imageSlots:         makeImageSlots(cfg.ImageMaxConcurrency),
		fileTasks:          map[string]*editableFileTaskState{},
		schedulerLeases:    map[string]map[string]any{},
		external:           newExternalManager(cfg.DataDir),
		refreshProgress:    map[string]*accountRefreshProgress{},
		survivalStatus:     map[string]any{"running": false, "last_started_at": "", "last_finished_at": "", "last_error": "", "last_summary": map[string]any{}, "next_run_at": ""},
		survivalWake:       make(chan struct{}, 1),
		probeStop:          make(chan struct{}),
		probeWake:          make(chan struct{}, 1),
		oauthStore:         oauth.NewStore(cfg.OAuthPath, firstNonEmpty(cfg.AdminKey, cfg.APIKey, "gptgrok2api")),
		openAILogin:        oauth.NewOpenAILogin(cfg.OpenAIAuthBaseURL, cfg.OpenAIPlatformBaseURL, cfg.OpenAILoginTokenURL, requestClient),
		agentIdentityStore: agentidentity.NewStore(cfg.DataDir, cfg.OpenAIAgentRegisterURL, requestClient),
		taskQueue:          taskQueue,
		monitor:            newRuntimeMonitor(),
		registerStore:      registerruntime.New(cfg.RegisterPath, cfg.GrokAccountsPath),
		registerRuntime:    registerruntime.NewRuntime(),
	}
	server.openAIChat = provider.NewOpenAIChat(server.openAIImage)
	proxyManager.SetImageNodeResultCallback(server.persistProxyGroupRuntimeResult)
	server.accountPool.SetInvalidCallback(server.maybeAutoRemoveInvalidAccount)
	server.loadEditableFileTasks()
	server.chatProvider.SetProxyManager(proxyManager)
	server.consoleProvider.SetProxyManager(proxyManager)
	server.mediaProvider.SetProxyManager(proxyManager)
	server.deviceOAuth = oauth.NewDeviceService(requestClient, cfg.OAuthDeviceURL, cfg.OAuthTokenURL, server.oauthStore)
	server.taskQueue.Register("grok_oauth_authorize", func(task *tasks.Task) (map[string]any, error) {
		accountID := stringValue(task.Payload["account_id"])
		_, _ = server.registerStore.SetOAuthAuthorization(accountID, "manual_action_required", "")
		return map[string]any{
			"status":     "manual_action_required",
			"message":    "请使用 /api/grok/oauth/device/start 完成 xAI Device Code 授权",
			"account_id": task.Payload["account_id"],
		}, nil
	})
	if cfg.Version != "test" {
		server.taskQueue.Start(2)
		go server.imageRetentionScheduler()
		go server.grokProbeScheduler()
		go server.openAISurvivalScheduler()
	}
	return server
}

func runtimeProxyGroups(groups []config.ProxyGroup) []proxyruntime.GroupConfig {
	result := make([]proxyruntime.GroupConfig, 0, len(groups))
	for _, group := range groups {
		item := proxyruntime.GroupConfig{ID: group.ID, Name: group.Name, Enabled: group.Enabled, Strategy: group.Strategy}
		for _, node := range group.Nodes {
			item.Nodes = append(item.Nodes, proxyruntime.NodeConfig{
				ID: node.ID, Name: node.Name, URL: node.URL, Enabled: node.Enabled,
				ImageConcurrencyLimit: node.ImageConcurrencyLimit, LastStatus: node.LastStatus, LastError: node.LastError,
				RuntimeFailures: node.RuntimeFailures, RuntimeSuccesses: node.RuntimeSuccesses, RuntimeLatencyMS: node.RuntimeLatencyMS,
			})
		}
		result = append(result, item)
	}
	return result
}

func (s *Server) refreshProxyRuntime() error {
	if s == nil || s.store == nil || s.proxyManager == nil {
		return nil
	}
	values, err := s.store.Config()
	if err != nil {
		return err
	}
	runtimeConfig := config.Config{}
	config.ApplyProxyConfig(&runtimeConfig, values)
	// Environment variables retain their normal precedence over persisted UI
	// settings, matching config.Load at process startup.
	if strings.TrimSpace(os.Getenv("GO_PROXY_URL")) != "" {
		runtimeConfig.ProxyURL = s.cfg.ProxyURL
	}
	if strings.TrimSpace(os.Getenv("GO_PROXY_POOL")) != "" {
		runtimeConfig.ProxyPool = s.cfg.ProxyPool
	}
	if strings.TrimSpace(os.Getenv("GO_RESOURCE_PROXY_URL")) != "" {
		runtimeConfig.ResourceProxyURL = s.cfg.ResourceProxyURL
	}
	if strings.TrimSpace(os.Getenv("GO_RESOURCE_PROXY_POOL")) != "" {
		runtimeConfig.ResourceProxyPool = s.cfg.ResourceProxyPool
	}
	s.proxyManager.SetDefault(runtimeConfig.ProxyURL, runtimeConfig.ProxyPool)
	s.proxyManager.SetResource(runtimeConfig.ResourceProxyURL, runtimeConfig.ResourceProxyPool)
	s.proxyManager.ConfigureImageGroups(runtimeConfig.FallbackProxy, runtimeProxyGroups(runtimeConfig.ProxyGroups))
	return nil
}

func makeImageSlots(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

// acquireImageSlot bounds the number of expensive browser-image flows in the
// process. Waiting here is real handler queue time, unlike provider stages that
// may overlap later in the request.
func (s *Server) acquireImageSlot(ctx context.Context, r *http.Request) (func(), error) {
	if s == nil || s.imageSlots == nil {
		return func() {}, nil
	}
	started := time.Now()
	select {
	case s.imageSlots <- struct{}{}:
		waited := time.Since(started)
		if r != nil {
			s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": waited.Milliseconds()})
		}
		var once sync.Once
		return func() { once.Do(func() { <-s.imageSlots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maybeAutoRemoveInvalidAccount removes browser/session accounts only after a
// provider explicitly rejects their credential. OAuth accounts with a refresh
// token remain available for the normal AT refresh flow, and the setting is
// checked on every event so changes take effect without restarting the server.
func (s *Server) maybeAutoRemoveInvalidAccount(account accounts.Account) {
	if strings.TrimSpace(account.Token) == "" ||
		strings.TrimSpace(stringValue(account.Fields["refresh_token"])) != "" ||
		(strings.TrimSpace(stringValue(account.Fields["login_password"])) != "" &&
			strings.TrimSpace(firstNonEmpty(stringValue(account.Fields["two_factor_secret"]), stringValue(account.Fields["totp_secret"]))) != "") {
		return
	}
	settings, err := s.store.Config()
	if err != nil || !boolValue(settings["auto_remove_invalid_accounts"], false) {
		return
	}
	removed, _, err := s.store.DeleteAccounts([]string{account.Token})
	if err != nil {
		log.Printf("auto remove invalid account failed: %v", err)
		return
	}
	if removed > 0 {
		log.Printf("auto removed invalid account %s", accountPublicRef(account.Fields))
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/models", s.listModels)
	mux.HandleFunc("/v1/models/", s.getModel)
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/responses", s.responses)
	mux.HandleFunc("/v1/messages", s.messages)
	mux.HandleFunc("/v1/images/generations", s.imageGenerations)
	mux.HandleFunc("/v1/images/edits", s.imageEdits)
	mux.HandleFunc("/v1/files/image", s.imageFile)
	mux.HandleFunc("/v1/files/video", s.videoFile)
	mux.HandleFunc("/upimg/v1/files/image", s.imageFile)
	mux.HandleFunc("/upimg/v1/files/video", s.videoFile)
	mux.HandleFunc("/v1/files", s.files)
	mux.HandleFunc("/v1/videos", s.videosCreate)
	mux.HandleFunc("/v1/videos/", s.videoByID)
	mux.HandleFunc("/v1/", s.v1)
	mux.HandleFunc("/grok/", s.grok)
	mux.HandleFunc("/auth/login", s.login)
	mux.HandleFunc("/auth/status", s.authStatus)
	mux.HandleFunc("/version", s.version)
	mux.HandleFunc("/meta/update", s.metaUpdate)
	mux.HandleFunc("/updates/VERSION", s.versionFile)
	mux.HandleFunc("/updates/CHANGELOG.md", s.changelogFile)
	mux.HandleFunc("/api/auth/users", s.userKeys)
	mux.HandleFunc("/api/auth/users/", s.userKeyByID)
	mux.HandleFunc("/api/accounts", s.accounts)
	mux.HandleFunc("/api/accounts/token", s.accountToken)
	mux.HandleFunc("/api/accounts/refresh", s.accountRefreshStart)
	mux.HandleFunc("/api/accounts/refresh-at", s.accountAccessTokenRefresh)
	mux.HandleFunc("/api/accounts/refresh/progress/", s.accountRefreshProgressAPI)
	mux.HandleFunc("/api/accounts/oauth/start", s.accountOAuthStart)
	mux.HandleFunc("/api/accounts/oauth/finish", s.accountOAuthFinish)
	mux.HandleFunc("/api/accounts/export", s.accountExport)
	mux.HandleFunc("/api/accounts/import-api", s.importAccountsAPI)
	mux.HandleFunc("/api/accounts/agent-identities", s.agentIdentities)
	mux.HandleFunc("/api/accounts/import-cleanup", s.cleanupImportedAbnormalAccounts)
	mux.HandleFunc("/api/accounts/update", s.updateAccount)
	mux.HandleFunc("/api/accounts/batch-update", s.batchUpdateAccounts)
	mux.HandleFunc("/api/accounts/group", s.bindAccountGroup)
	mux.HandleFunc("/api/account-groups", s.accountGroups)
	mux.HandleFunc("/api/account-groups/", s.accountGroupByID)
	mux.HandleFunc("/api/settings", s.settings)
	mux.HandleFunc("/api/settings/retention-cleanup/preview", s.retentionCleanup)
	mux.HandleFunc("/api/settings/retention-cleanup/run", s.retentionCleanup)
	mux.HandleFunc("/api/settings/account-cleanup/preview", s.accountCleanup)
	mux.HandleFunc("/api/settings/account-cleanup/run", s.accountCleanup)
	mux.HandleFunc("/api/third-party-apps", s.thirdPartyApps)
	mux.HandleFunc("/api/model-catalog", s.modelCatalog)
	mux.HandleFunc("/api/logs", s.logsAPI)
	mux.HandleFunc("/api/logs/delete", s.deleteLogs)
	mux.HandleFunc("/api/runtime-logs", s.runtimeLogs)
	mux.HandleFunc("/api/proxy/runtime", s.proxyRuntime)
	mux.HandleFunc("/api/prompts", s.prompts)
	mux.HandleFunc("/api/admin/prompt-sources", s.promptSources)
	mux.HandleFunc("/api/admin/prompt-sources/", s.promptSource)
	mux.HandleFunc("/api/image-tasks", s.imageTasksAPI)
	mux.HandleFunc("/api/image-tasks/quota", s.imageTaskQuota)
	mux.HandleFunc("/api/image-tasks/generations", s.imageTasksAPI)
	mux.HandleFunc("/api/image-tasks/edits", s.imageTaskEdits)
	mux.HandleFunc("/api/image-tasks/", s.imageTaskByID)
	mux.HandleFunc("/api/storage/info", s.storageInfo)
	mux.HandleFunc("/api/grok/oauth/", s.grokOAuth)
	mux.HandleFunc("/accounts", s.grokOAuthLegacy)
	mux.HandleFunc("/accounts/", s.grokOAuthLegacy)
	mux.HandleFunc("/device/", s.grokOAuthLegacy)
	mux.HandleFunc("/protocol/", s.grokOAuthProtocolLegacy)
	mux.HandleFunc("/api/proxy/profiles", s.proxyProfiles)
	mux.HandleFunc("/api/proxy/profiles/", s.proxyProfileByID)
	mux.HandleFunc("/api/proxy/groups", s.proxyGroups)
	mux.HandleFunc("/api/proxy/groups/", s.proxyGroupByID)
	mux.HandleFunc("/api/proxy/health", s.proxyHealth)
	mux.HandleFunc("/api/proxy/test", s.proxyTest)
	mux.HandleFunc("/api/proxy/sample-test", s.proxyTest)
	mux.HandleFunc("/api/proxy/profiles/test", s.proxyTest)
	mux.HandleFunc("/api/proxy/groups/test", s.proxyGroupTest)
	mux.HandleFunc("/api/proxy/clearance/test", s.proxyTest)
	mux.HandleFunc("/api/backup/test", s.backupTest)
	mux.HandleFunc("/api/image-storage/test", s.imageStorageTest)
	mux.HandleFunc("/api/image-storage/sync", s.imageStorageSync)
	mux.HandleFunc("/api/icloud/claim-status/sync", s.iCloudClaimStatusSync)
	mux.HandleFunc("/api/images", s.adminImages)
	mux.HandleFunc("/api/images/", s.adminImages)
	mux.HandleFunc("/images/", s.publicImage)
	mux.HandleFunc("/image-thumbnails/", s.publicImage)
	mux.HandleFunc("/api/dashboard", s.dashboard)
	mux.HandleFunc("/api/monitor/realtime", s.monitorAPI)
	mux.HandleFunc("/api/monitor/realtime/", s.monitorAPI)
	mux.HandleFunc("/api/backups", s.backupsAPI)
	mux.HandleFunc("/api/backups/", s.backupsAPI)
	mux.HandleFunc("/api/register", s.registerAPI)
	mux.HandleFunc("/api/register/", s.registerAPI)
	mux.HandleFunc("/api/cpa/pools", s.cpaPoolsAPI)
	mux.HandleFunc("/api/cpa/pools/", s.cpaPoolAPI)
	mux.HandleFunc("/api/sub2api/servers", s.sub2APIServersAPI)
	mux.HandleFunc("/api/sub2api/servers/", s.sub2APIServerAPI)
	mux.HandleFunc("/api/grok/runtime/admin/", s.grokRuntimeAdminAPI)
	mux.HandleFunc("/api/tasks", s.taskAPI)
	mux.HandleFunc("/api/tasks/", s.taskAPI)
	mux.HandleFunc("/internal/image-monitor/", s.internalImageMonitor)
	mux.HandleFunc("/internal/image-scheduler/", s.internalImageScheduler)
	mux.HandleFunc("/internal/logs/call", s.internalCallLog)
	mux.HandleFunc("/v1/search", s.searchAPI)
	mux.HandleFunc("/v1/editable-file-tasks", s.editableFileTasksAPI)
	mux.HandleFunc("/v1/editable-file-tasks/", s.editableFileTaskByID)
	mux.HandleFunc("/v1/ppt/generations", s.pptGenerations)
	mux.HandleFunc("/v1/psd/generations", s.psdGenerations)
	mux.HandleFunc("/files/", s.downloadEditableFile)
	mux.HandleFunc("/api/", s.adminAPI)
	mux.HandleFunc("/admin/api/", s.adminAPI)
	mux.HandleFunc("/", s.static)
	return s.withMiddleware(mux)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Admin-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.shouldMonitorRequest(r) {
			s.withRequestMonitor(w, r, next)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusCaptureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

const maxJSONBodyBytes = 64 << 20

func (w *statusCaptureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCaptureWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < 8<<10 {
		remain := 8<<10 - w.body.Len()
		if len(data) > remain {
			w.body.Write(data[:remain])
		} else {
			w.body.Write(data)
		}
	}
	return w.ResponseWriter.Write(data)
}

func (w *statusCaptureWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) shouldMonitorRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := strings.TrimRight(r.URL.Path, "/")
	return path == "/v1/images/generations" || path == "/v1/images/edits" || path == "/v1/chat/completions"
}

func (s *Server) withRequestMonitor(w http.ResponseWriter, r *http.Request, next http.Handler) {
	modelName, summary, requestShape := monitorRequestShape(r)
	id := newChatID()
	s.monitor.start(id, r.URL.Path, modelName, summary)
	proxySnapshot := s.proxyManager.Snapshot()
	meta := map[string]any{"model": modelName, "endpoint": r.URL.Path, "has_proxy": boolValue(proxySnapshot["proxy_configured"], false), "egress_mode": stringValue(proxySnapshot["mode"])}
	if identity, ok := s.auth.Identity(s.auth.APIKey(r)); ok {
		meta["key_id"] = identity.ID
		meta["key_name"] = identity.Name
		meta["role"] = identity.Role
	}
	if meta["has_proxy"] == true {
		meta["proxy_source"] = "default"
	} else {
		meta["proxy_source"] = "direct"
	}
	s.monitor.enrich(id, meta)
	s.monitor.update(id, "handler_started", 10, "")
	s.monitor.enrich(id, map[string]any{"metrics": map[string]any{"handler_queue_ms": 0}})
	capture := &statusCaptureWriter{ResponseWriter: w}
	next.ServeHTTP(capture, r.WithContext(context.WithValue(r.Context(), monitorCallIDKey{}, id)))
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	errText := monitorResponseErrorText(capture.body.Bytes(), status)
	if status >= 400 {
		s.monitor.finish(id, "failed", modelName, summary, errText)
	} else {
		s.monitor.finish(id, "success", modelName, summary, "")
	}
	if record, ok := s.monitor.detail(id); ok {
		s.appendCallLog(record, status, requestShape, capture.body.Bytes(), errText)
	}
}

type monitorCallIDKey struct{}

func (s *Server) enrichRequestMonitor(r *http.Request, meta map[string]any) {
	if r == nil {
		return
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return
	}
	// Keep live monitor columns in sync with request metadata. Previously these
	// values were only stored under request_meta, so the active-request table
	// could not show the selected account or actual egress proxy.
	s.monitor.enrich(id, meta)
	s.monitor.mu.Lock()
	defer s.monitor.mu.Unlock()
	if item := s.monitor.active[id]; item != nil {
		if item.RequestMeta == nil {
			item.RequestMeta = map[string]any{}
		}
		for key, value := range meta {
			item.RequestMeta[key] = value
		}
		if model := stringValue(meta["model"]); model != "" {
			item.Model = model
		}
	}
}

func (s *Server) stageRequestMonitor(r *http.Request, stage string, progress int, metrics map[string]any) {
	if r == nil {
		return
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return
	}
	s.monitor.update(id, stage, progress, "")
	s.monitor.enrich(id, map[string]any{"metrics": metrics})
}

func (s *Server) requestMonitorElapsed(r *http.Request) int64 {
	if s == nil || s.monitor == nil || r == nil {
		return 0
	}
	id, _ := r.Context().Value(monitorCallIDKey{}).(string)
	if id == "" {
		return 0
	}
	if record, ok := s.monitor.detail(id); ok && record.StartedAt > 0 {
		return time.Now().UnixMilli() - record.StartedAt
	}
	return 0
}

func (s *Server) enrichMonitorAccount(r *http.Request, account accounts.Account) {
	if r == nil {
		return
	}
	meta := map[string]any{}
	for _, key := range []string{"email", "account_email", "profile.email", "user.email"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["account_email"] = value
			break
		}
	}
	for _, key := range []string{"chatgpt_account_id", "chatgpt_account_user_id", "user_id", "account_id", "id"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["account_id"] = value
			break
		}
	}
	for _, key := range []string{"key_name", "token_name", "name"} {
		if value := strings.TrimSpace(accountFieldValue(account.Fields, key)); value != "" {
			meta["key_name"] = value
			break
		}
	}
	for _, key := range []string{"proxy", "proxy_url", "proxyUrl"} {
		proxyURL := strings.TrimSpace(accountFieldValue(account.Fields, key))
		if proxyURL == "" || strings.HasPrefix(strings.ToLower(proxyURL), "group:") {
			continue
		}
		if label := sanitizedEgressLabel(proxyURL); label != "" {
			meta["proxy_source"] = "account"
			meta["egress_label"] = label
			meta["has_proxy"] = true
		}
		break
	}
	if _, ok := meta["account_id"]; !ok {
		// The token is intentionally never logged; a stable pool label is a
		// useful last-resort account identifier for older imports.
		if pool := strings.TrimSpace(account.Pool); pool != "" {
			meta["account_id"] = pool
		}
	}
	if len(meta) > 0 {
		s.enrichRequestMonitor(r, meta)
	}
}

// accountFieldValue supports both the flat account JSON used by current
// imports and the nested profile/user objects used by older imports.
func accountFieldValue(fields map[string]any, key string) string {
	if fields == nil {
		return ""
	}
	if value := stringValue(fields[key]); value != "" {
		return value
	}
	parts := strings.Split(key, ".")
	var current any = fields
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[part]
	}
	return stringValue(current)
}

func monitorRequestShape(r *http.Request) (string, string, any) {
	if r == nil {
		return "", "", ""
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			return "", "", "multipart/form-data"
		}
		values := r.MultipartForm.Value
		modelName := strings.TrimSpace(firstFormValue(values, "model"))
		summary := strings.TrimSpace(firstFormValue(values, "prompt"))
		if summary == "" {
			summary = strings.TrimSpace(firstFormValue(values, "input"))
		}
		if summary == "" {
			summary = strings.TrimSpace(firstFormValue(values, "message"))
		}
		if len(summary) > 180 {
			summary = summary[:180]
		}
		count := 0
		for _, key := range imageEditReferenceFields {
			count += len(r.MultipartForm.File[key]) + len(values[key])
		}
		return modelName, summary, map[string]any{"content_type": "multipart/form-data", "image_url_parts": count, "data_url_images": count, "size": firstFormValue(values, "size")}
	}
	if r.Body == nil || (r.ContentLength > maxJSONBodyBytes && r.ContentLength != -1) {
		return "", "", "application/json"
	}
	if contentType != "application/json" && contentType != "" {
		return "", "", contentType
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBodyBytes))
	if err != nil {
		r.Body = io.NopCloser(strings.NewReader(""))
		return "", "", "application/json"
	}
	r.Body = io.NopCloser(strings.NewReader(string(raw)))
	decoded, ok := normalizeJSONBytes(raw, r.Header.Get("Content-Encoding"))
	if !ok {
		return "", "", "application/json"
	}
	r.Body = io.NopCloser(bytes.NewReader(decoded))
	var payload map[string]any
	if json.Unmarshal(decoded, &payload) != nil {
		return "", "", "application/json"
	}
	modelName := stringValue(payload["model"])
	summary := stringValue(payload["prompt"])
	if summary == "" {
		summary = protocol.ExtractMessage(chatMessagesFromAny(payload["messages"]))
	}
	if len(summary) > 180 {
		summary = summary[:180]
	}
	urlParts, dataURLs := imageReferenceStats(payload)
	return modelName, summary, map[string]any{"content_type": "application/json", "image_url_parts": urlParts, "data_url_images": dataURLs, "size": stringValue(payload["size"])}
}

func imageReferenceStats(value any) (int, int) {
	urls, data := 0, 0
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "data:image/") {
			return 1, 1
		}
		if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			return 1, 0
		}
	case []any:
		for _, x := range v {
			a, b := imageReferenceStats(x)
			urls += a
			data += b
		}
	case map[string]any:
		for k, x := range v {
			if k == "image_url" || k == "image_url_parts" || k == "image" || k == "images" || k == "images[]" || k == "content" || k == "messages" {
				a, b := imageReferenceStats(x)
				urls += a
				data += b
			}
		}
	}
	return urls, data
}

func monitorResponseErrorText(raw []byte, status int) string {
	if status < 400 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return http.StatusText(status)
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if message := stringValue(mapValue(payload["error"])["message"]); message != "" {
			return message
		}
		if message := stringValue(payload["message"]); message != "" {
			return message
		}
	}
	if len(trimmed) > 1000 {
		return trimmed[:1000]
	}
	return trimmed
}

func chatMessagesFromAny(value any) []protocol.Message {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	messages := make([]protocol.Message, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		messages = append(messages, protocol.Message{
			Role:    stringValue(entry["role"]),
			Content: entry["content"],
		})
	}
	return messages
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":              "ok",
		"runtime":             "go",
		"version":             s.cfg.Version,
		"upstream_configured": s.cfg.UpstreamURL != "",
		"queue_backend":       s.cfg.QueueBackend,
		"proxy":               s.proxyManager.Snapshot(),
		"timestamp":           time.Now().UTC(),
	})
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	data := make([]map[string]any, 0, len(s.catalog))
	for _, item := range s.catalog {
		if item.Enabled {
			data = append(data, item.Public())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	item, ok := model.Find(s.catalog, id)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, item.Public())
}

func (s *Server) v1(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "API endpoint not found", "not_found")
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request protocol.ChatRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if len(request.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages cannot be empty", "invalid_request_error")
		return
	}
	for index, message := range request.Messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer", "user", "assistant", "tool":
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid message role at index %d", index), "invalid_request_error")
			return
		}
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		writeError(w, http.StatusBadRequest, "temperature must be between 0 and 2", "invalid_request_error")
		return
	}
	if request.TopP != nil && (*request.TopP < 0 || *request.TopP > 1) {
		writeError(w, http.StatusBadRequest, "top_p must be between 0 and 1", "invalid_request_error")
		return
	}
	route, ok := model.ResolveChat(request.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	if route.Console {
		s.consoleChatCompletions(w, r, request)
		return
	}
	if route.Image {
		if request.Stream {
			s.streamOpenAIImageChat(w, r, request)
		} else {
			s.completeOpenAIImageChat(w, r, request)
		}
		return
	}
	if route.OpenAI {
		if request.Stream {
			s.streamOpenAIChat(w, r, request, route)
		} else {
			s.completeOpenAIChat(w, r, request, route)
		}
		return
	}
	if len(request.Tools) > 0 || request.ToolChoice != nil {
		if request.Stream {
			s.streamToolChat(w, r, request, route)
		} else {
			s.completeChat(w, r, request, route)
		}
		return
	}
	if request.Stream {
		s.streamChat(w, r, request, route)
		return
	}
	s.completeChat(w, r, request, route)
}

func (s *Server) completeChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	message := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(message) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	if len(request.Tools) > 0 {
		message = protocol.InjectToolPrompt(message, protocol.BuildToolSystemPrompt(request.Tools, request.ToolChoice))
	}
	responseID := newChatID()
	excluded := map[string]bool{}
	var text, thinking string
	var lastErr error
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		lease, err := s.accountPool.ReserveMatching(r.Context(), route.PoolCandidates, excluded, isGrokAccount)
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		payload := protocol.BuildGrokPayload(message, route.Mode, request.Temperature, request.TopP, request.MaxTokens)
		response, err := s.chatProvider.Do(r.Context(), lease.Account, payload)
		if err != nil {
			s.accountPool.Release(lease)
			s.accountPool.Feedback(lease.Account, http.StatusBadGateway, err)
			excluded[lease.Account.Token] = true
			lastErr = err
			if attempt < s.cfg.ChatMaxRetries {
				continue
			}
			break
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			upstreamErr := provider.ReadError(response)
			s.accountPool.Release(lease)
			s.accountPool.Feedback(lease.Account, upstreamErr.Status, upstreamErr)
			excluded[lease.Account.Token] = true
			lastErr = upstreamErr
			if s.shouldRetry(upstreamErr.Status, attempt) {
				continue
			}
			break
		}
		var scanErr error
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		scanErr = protocol.ScanUpstream(scanner, func(event protocol.UpstreamEvent) error {
			text += event.Text
			thinking += event.Thinking
			return nil
		})
		response.Body.Close()
		s.accountPool.Release(lease)
		if scanErr != nil {
			s.accountPool.Feedback(lease.Account, http.StatusBadGateway, scanErr)
			excluded[lease.Account.Token] = true
			lastErr = scanErr
			if attempt < s.cfg.ChatMaxRetries {
				text, thinking = "", ""
				continue
			}
			break
		}
		s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
		lastErr = nil
		break
	}
	if lastErr != nil {
		status := http.StatusBadGateway
		if errors.Is(lastErr, accounts.ErrUnavailable) {
			status = http.StatusTooManyRequests
		}
		if upstreamErr, ok := lastErr.(*protocol.UpstreamError); ok && upstreamErr.Status >= 400 && upstreamErr.Status < 600 {
			status = upstreamErr.Status
		}
		writeError(w, status, lastErr.Error(), "upstream_error")
		return
	}
	response := map[string]any{
		"id":      newChatIDFrom(responseID),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   request.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usageFor(message, text, thinking),
	}
	if thinking != "" {
		response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["reasoning_content"] = thinking
	}
	if len(request.Tools) > 0 {
		calls := protocol.ParseToolCalls(text, protocol.ToolNames(request.Tools))
		if len(calls) > 0 {
			toolCalls := make([]map[string]any, 0, len(calls))
			for _, call := range calls {
				toolCalls = append(toolCalls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
			}
			choice := response["choices"].([]any)[0].(map[string]any)
			choice["message"] = map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
			choice["finish_reason"] = "tool_calls"
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) streamToolChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	recorder := &responseCapture{header: make(http.Header)}
	request.Stream = false
	s.completeChat(recorder, r, request, route)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var response map[string]any
	if json.Unmarshal(recorder.body.Bytes(), &response) != nil {
		writeError(w, http.StatusBadGateway, "invalid internal tool response", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	choice := response["choices"].([]any)[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if calls, ok := message["tool_calls"].([]any); ok {
		for index, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": index, "id": call["id"], "type": "function", "function": map[string]any{"name": function["name"], "arguments": function["arguments"]}}}}}}})
		}
		writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	} else {
		writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": message["content"]}}}})
		writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	message := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(message) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	responseID := newChatID()
	excluded := map[string]bool{}
	emitted := false
	var lastErr error
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		lease, err := s.accountPool.ReserveMatching(r.Context(), route.PoolCandidates, excluded, isGrokAccount)
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		payload := protocol.BuildGrokPayload(message, route.Mode, request.Temperature, request.TopP, request.MaxTokens)
		response, err := s.chatProvider.Do(r.Context(), lease.Account, payload)
		if err != nil {
			s.accountPool.Release(lease)
			s.accountPool.Feedback(lease.Account, http.StatusBadGateway, err)
			excluded[lease.Account.Token] = true
			lastErr = err
			if !emitted && attempt < s.cfg.ChatMaxRetries {
				continue
			}
			break
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			upstreamErr := provider.ReadError(response)
			s.accountPool.Release(lease)
			s.accountPool.Feedback(lease.Account, upstreamErr.Status, upstreamErr)
			excluded[lease.Account.Token] = true
			lastErr = upstreamErr
			if !emitted && s.shouldRetry(upstreamErr.Status, attempt) {
				continue
			}
			break
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		scanErr := protocol.ScanUpstream(scanner, func(event protocol.UpstreamEvent) error {
			if event.Text == "" && event.Thinking == "" && !event.SoftStop {
				return nil
			}
			if !emitted {
				writeSSE(w, map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}},
				})
				emitted = true
			}
			if event.Text != "" {
				writeSSE(w, map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": event.Text}}},
				})
			}
			if event.Thinking != "" && request.ReasoningEffort != "none" {
				writeSSE(w, map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": event.Thinking}}},
				})
			}
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		response.Body.Close()
		s.accountPool.Release(lease)
		if scanErr != nil {
			s.accountPool.Feedback(lease.Account, http.StatusBadGateway, scanErr)
			excluded[lease.Account.Token] = true
			lastErr = scanErr
			if !emitted && attempt < s.cfg.ChatMaxRetries {
				continue
			}
			break
		}
		s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
		lastErr = nil
		break
	}
	if lastErr != nil {
		writeSSE(w, map[string]any{"error": map[string]any{"message": lastErr.Error(), "type": "upstream_error"}})
	}
	if !emitted {
		writeSSE(w, map[string]any{
			"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}},
		})
	}
	writeSSE(w, map[string]any{
		"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (s *Server) shouldRetry(status, attempt int) bool {
	return attempt < s.cfg.ChatMaxRetries && s.cfg.ChatRetryCodes[status]
}

func writeSSE(w http.ResponseWriter, value any) {
	raw, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
}

func newChatID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

func newChatIDFrom(value string) string {
	if value != "" {
		return value
	}
	return newChatID()
}

func usageFor(prompt, completion, reasoning string) map[string]any {
	promptTokens := len([]rune(prompt)) / 4
	completionTokens := len([]rune(completion)) / 4
	reasoningTokens := len([]rune(reasoning)) / 4
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens + reasoningTokens,
		"total_tokens":      promptTokens + completionTokens + reasoningTokens,
	}
}

func (s *Server) grok(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "Grok endpoint not found", "not_found")
}

func (s *Server) adminAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeError(w, http.StatusNotFound, "admin endpoint not found", "not_found")
}

func (s *Server) proxyUpstream(w http.ResponseWriter, r *http.Request) {
	target := s.cfg.UpstreamURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Host")
	if s.cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.UpstreamAPIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("upstream response copy: %v", err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	token := s.auth.APIKey(r)
	identity, ok := s.auth.Identity(token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid authentication token", "authentication_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
		"version":       s.cfg.Version,
		"role":          identity.Role,
		"subject_id":    identity.ID,
		"name":          identity.Name,
	})
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	token := s.auth.APIKey(r)
	identity, ok := s.auth.Identity(token)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            false,
			"authenticated": false,
			"version":       s.cfg.Version,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"authenticated": true,
		"runtime":       "go",
		"version":       s.cfg.Version,
		"role":          identity.Role,
		"subject_id":    identity.ID,
		"name":          identity.Name,
	})
}

func (s *Server) userKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.store.ListPublicKeys("user")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		item, rawKey, err := s.store.CreateKey("user", body.Name, firstNonEmpty(s.cfg.AdminKey, s.cfg.APIKey))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"item": item, "key": rawKey, "items": items})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) userKeyByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/auth/users/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "user key not found", "not_found")
		return
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		filtered := map[string]any{}
		for _, key := range []string{"name", "enabled", "key"} {
			if value, ok := updates[key]; ok {
				filtered[key] = value
			}
		}
		if len(filtered) == 0 {
			writeError(w, http.StatusBadRequest, "no updates provided", "invalid_request_error")
			return
		}
		item, err := s.store.UpdateKey(id, "user", filtered, s.cfg.AdminKey)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeError(w, http.StatusNotFound, "user key not found", "not_found")
			} else {
				writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			}
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"item": item, "items": items})
	case http.MethodDelete:
		if err := s.store.DeleteKey(id, "user"); err != nil {
			writeError(w, http.StatusNotFound, "user key not found", "not_found")
			return
		}
		items, _ := s.store.ListPublicKeys("user")
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listAccounts(w, r)
	case http.MethodPost:
		s.addAccounts(w, r)
	case http.MethodDelete:
		s.deleteAccounts(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accountToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessToken string `json:"access_token"`
		AccountRef  string `json:"account_ref"`
		ID          string `json:"id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	ref := firstNonEmpty(body.AccountRef, body.ID, body.AccessToken)
	if strings.TrimSpace(ref) == "" {
		writeError(w, http.StatusBadRequest, "account_ref is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, _ := resolveAccountRefTokens(items, []string{ref})
	if len(tokens) == 0 || strings.TrimSpace(tokens[0]) == "" {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}
	var account map[string]any
	for _, item := range items {
		if accountToken(item) == tokens[0] {
			account = item
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      tokens[0],
		"account_ref":       accountRefForToken(items, tokens[0]),
		"token_preview":     tokenPreview(tokens[0]),
		"has_access_token":  true,
		"user_id":           stringValue(account["user_id"]),
		"email":             stringValue(account["email"]),
		"login_password":    firstNonEmpty(stringValue(account["login_password"]), stringValue(account["password"])),
		"two_factor_secret": firstNonEmpty(stringValue(account["two_factor_secret"]), stringValue(account["totp_secret"])),
	})
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	query := r.URL.Query()
	page := positiveInt(query.Get("page"), 1)
	pageSize := positiveInt(query.Get("page_size"), 500)
	if pageSize > 500 {
		pageSize = 500
	}
	keyword := strings.ToLower(strings.TrimSpace(query.Get("keyword")))
	status := strings.ToLower(strings.TrimSpace(query.Get("status")))
	groupID := strings.TrimSpace(query.Get("group_id"))
	filtered := make([]map[string]any, 0, len(items))
	for _, item := range items {
		// The OpenAI account page must not expose Grok SSO credentials.
		// Both providers share the runtime store for backwards compatibility,
		// but their pools are intentionally separated by source/type.
		if !isOpenAIAccountFields(item) {
			continue
		}
		if keyword != "" && !accountContains(item, keyword) {
			continue
		}
		if !accountStatusMatches(item, status) {
			continue
		}
		if !accountGroupMatches(item, groupID) {
			continue
		}
		filtered = append(filtered, accountForAPI(item))
	}
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":     filtered[start:end],
		"accounts":  filtered[start:end],
		"total":     len(filtered),
		"all_total": len(filtered),
		"page":      page,
		"page_size": pageSize,
	})
}

func (s *Server) addAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tokens      []string         `json:"tokens"`
		Accounts    []map[string]any `json:"accounts"`
		Refresh     *bool            `json:"refresh"`
		ReturnItems *bool            `json:"return_items"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	added, skipped, items, err := s.store.AddAccounts(body.Tokens, body.Accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	returnItems := body.ReturnItems == nil || *body.ReturnItems
	response := map[string]any{
		"added":     added,
		"skipped":   skipped,
		"refreshed": 0,
		"errors":    []string{},
	}
	if returnItems {
		response["items"] = accountsForAPI(items)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) cleanupImportedAbnormalAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
		Remove       bool     `json:"remove"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	refs := uniqueAccountRefs(body.AccessTokens)
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, "access_tokens is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(items, refs)
	targets := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		targets[token] = struct{}{}
	}
	abnormalTokens := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, item := range items {
		token := accountToken(item)
		if _, ok := targets[token]; !ok || accountStatusCategory(item) != "abnormal" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		abnormalTokens = append(abnormalTokens, token)
	}
	removed := 0
	if body.Remove && len(abnormalTokens) > 0 {
		removed, _, err = s.store.DeleteAccounts(abnormalTokens)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checked":  len(refs),
		"abnormal": len(abnormalTokens),
		"removed":  removed,
		"errors":   missing,
	})
}

func (s *Server) deleteAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tokens []string `json:"tokens"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Tokens) == 0 {
		writeError(w, http.StatusBadRequest, "tokens is required", "invalid_request_error")
		return
	}
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(items, body.Tokens)
	removed, items, err := s.store.DeleteAccounts(tokens)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removed,
		"errors":  missing,
		"items":   accountsForAPI(items),
	})
}

func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body map[string]any
	if !decodeJSON(w, r, &body) {
		return
	}
	ref := strings.TrimSpace(stringValue(body["access_token"]))
	if ref == "" {
		writeError(w, http.StatusBadRequest, "access_token is required", "invalid_request_error")
		return
	}
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, _ := resolveAccountRefTokens(current, []string{ref})
	if len(tokens) == 0 {
		writeError(w, http.StatusNotFound, "account not found", "not_found")
		return
	}
	token := tokens[0]
	updates := map[string]any{}
	for _, key := range []string{"type", "source_type", "status", "quota", "proxy", "group_id", "enabled", "email", "user_id", "login_password", "two_factor_secret"} {
		if value, ok := body[key]; ok {
			updates[key] = value
		}
	}
	if len(updates) == 0 {
		writeError(w, http.StatusBadRequest, "no updates provided", "invalid_request_error")
		return
	}
	item, items, err := s.store.UpdateAccount(token, updates)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "account not found", "not_found")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"item": accountForAPI(item), "items": accountsForAPI(items)})
}

func (s *Server) batchUpdateAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
		Status       string   `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.AccessTokens) == 0 || strings.TrimSpace(body.Status) == "" {
		writeError(w, http.StatusBadRequest, "access_tokens and status are required", "invalid_request_error")
		return
	}
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(current, body.AccessTokens)
	updated := 0
	errItems := make([]string, 0, len(missing))
	for _, ref := range missing {
		errItems = append(errItems, tokenPreview(ref)+"... not found")
	}
	var all []map[string]any
	for _, token := range tokens {
		item, items, err := s.store.UpdateAccount(token, map[string]any{"status": body.Status})
		if err != nil {
			errItems = append(errItems, tokenPreview(token)+"... not found")
			continue
		}
		_ = item
		all = items
		updated++
	}
	if all == nil {
		all, _ = s.store.AccountList()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated": updated,
		"errors":  errItems,
		"items":   accountsForAPI(all),
	})
}

func (s *Server) bindAccountGroup(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
		GroupID      string   `json:"group_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	groupID := strings.TrimSpace(body.GroupID)
	if groupID == "__ungrouped__" {
		groupID = ""
	}
	updated := 0
	current, err := s.store.AccountList()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	tokens, missing := resolveAccountRefTokens(current, body.AccessTokens)
	errItems := make([]string, 0, len(missing))
	for _, ref := range missing {
		errItems = append(errItems, tokenPreview(ref)+"... not found")
	}
	var all []map[string]any
	for _, token := range tokens {
		item, items, err := s.store.UpdateAccount(token, map[string]any{"group_id": groupID})
		if err != nil {
			errItems = append(errItems, tokenPreview(token)+"... not found")
			continue
		}
		_ = item
		all = items
		updated++
	}
	if all == nil {
		all, _ = s.store.AccountList()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"updated":  updated,
		"errors":   errItems,
		"group_id": groupID,
		"items":    accountsForAPI(all),
	})
}

func (s *Server) accountGroups(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.accountGroupsPayload())
	case http.MethodPost:
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		id := slugID(stringValue(body["id"]))
		if id == "" {
			id = slugID(stringValue(body["name"]))
		}
		if id == "" {
			writeError(w, http.StatusBadRequest, "account group id is required", "invalid_request_error")
			return
		}
		item := map[string]any{
			"id":      id,
			"name":    firstNonEmpty(stringValue(body["name"]), id),
			"proxy":   stringValue(body["proxy"]),
			"enabled": boolValue(body["enabled"], true),
			"notes":   stringValue(body["notes"]),
		}
		cfg, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		groups := mapList(cfg["account_groups"])
		next := make([]map[string]any, 0, len(groups)+1)
		for _, group := range groups {
			if slugID(stringValue(group["id"])) != id {
				next = append(next, group)
			}
		}
		next = append(next, item)
		updated, err := s.store.UpdateConfig("account_groups", next)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"group": item, "groups": accountGroupsFromConfig(updated, s.accountCountByGroup())})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) accountGroupByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	id := slugID(strings.TrimPrefix(r.URL.Path, "/api/account-groups/"))
	cfg, err := s.store.Config()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	groups := mapList(cfg["account_groups"])
	next := make([]map[string]any, 0, len(groups))
	found := false
	for _, group := range groups {
		if slugID(stringValue(group["id"])) == id {
			found = true
			continue
		}
		next = append(next, group)
	}
	if !found {
		writeError(w, http.StatusNotFound, "account group not found", "not_found")
		return
	}
	updated, err := s.store.UpdateConfig("account_groups", next)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	accounts, _ := s.store.AccountList()
	for _, account := range accounts {
		if stringValue(account["group_id"]) == id {
			_, _, _ = s.store.UpdateAccount(accountToken(account), map[string]any{"group_id": ""})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": id,
		"groups":  accountGroupsFromConfig(updated, s.accountCountByGroup()),
	})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": s.cfg.Version})
}

func (s *Server) versionFile(w http.ResponseWriter, _ *http.Request) {
	serveTextFile(w, s.cfg.RelativePath("VERSION"), "text/plain; charset=utf-8")
}

func (s *Server) changelogFile(w http.ResponseWriter, _ *http.Request) {
	serveTextFile(w, s.cfg.RelativePath("CHANGELOG.md"), "text/markdown; charset=utf-8")
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		configValue, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runtime": "go", "config": configValue})
	case http.MethodPost:
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		current, err := s.store.Config()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		for key, value := range updates {
			current[key] = value
		}
		if err := s.store.ReplaceConfig(current); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		if err := s.refreshProxyRuntime(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runtime": "go", "config": current})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
	}
}

func (s *Server) storageInfo(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":        "json",
		"data_dir":       s.cfg.DataDir,
		"accounts_path":  s.cfg.AccountsPath,
		"auth_keys_path": s.cfg.AuthKeysPath,
		"config_path":    s.cfg.ConfigPath,
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(w, http.StatusRequestEntityTooLarge, "JSON body exceeds 64MB limit", "invalid_request_error")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body: request body could not be read", "invalid_request_error")
		}
		return false
	}
	decoded, ok := normalizeJSONBytes(raw, r.Header.Get("Content-Encoding"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid JSON body: body is empty, truncated, or has invalid content encoding", "invalid_request_error")
		return false
	}
	if err := json.Unmarshal(decoded, target); err != nil {
		// A few reverse proxies forward an already JSON-encoded body as a
		// quoted string (for example, {\"name\":\"demo\"}). Accept that
		// representation for object payloads before rejecting the request.
		var wrapped string
		if json.Unmarshal(decoded, &wrapped) == nil {
			if inner := strings.TrimSpace(wrapped); inner != "" && json.Unmarshal([]byte(inner), target) == nil {
				return true
			}
		}
		writeError(w, http.StatusBadRequest, describeJSONBodyError(err), "invalid_request_error")
		return false
	}
	return true
}

func describeJSONBodyError(err error) string {
	if err == nil {
		return "invalid JSON body"
	}
	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) {
		if strings.Contains(strings.ToLower(err.Error()), "unexpected end") {
			return fmt.Sprintf("invalid JSON body: request body is truncated near byte %d", syntaxError.Offset)
		}
		return fmt.Sprintf("invalid JSON body: malformed JSON near byte %d", syntaxError.Offset)
	}
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return fmt.Sprintf("invalid JSON body: field %q has the wrong value type", typeError.Field)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(strings.ToLower(err.Error()), "unexpected end") {
		return "invalid JSON body: request body is truncated"
	}
	return "invalid JSON body: malformed JSON"
}

// normalizeJSONBytes accepts the encodings emitted by common API gateways.
// Some clients prepend a UTF-8 BOM and some compress JSON request bodies.
func normalizeJSONBytes(raw []byte, contentEncoding string) ([]byte, bool) {
	encoding := strings.ToLower(strings.TrimSpace(strings.Split(contentEncoding, ",")[0]))
	// A few reverse proxies strip Content-Encoding while forwarding the body.
	// The gzip magic bytes let us still recognize and decode those requests.
	isGzip := len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b
	if encoding == "gzip" || encoding == "x-gzip" || isGzip {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			decompressed, readErr := io.ReadAll(io.LimitReader(reader, maxJSONBodyBytes))
			_ = reader.Close()
			if readErr != nil {
				return nil, false
			}
			raw = decompressed
		} else if isGzip {
			// A malformed gzip body must not be treated as JSON. If only the
			// header was retained by a proxy, however, the body is already plain.
			return nil, false
		}
	}
	raw = bytes.TrimSpace(raw)
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

func positiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func accountForAPI(account map[string]any) map[string]any {
	item := cloneMap(account)
	ref := accountPublicRef(account)
	if ref != "" {
		item["id"] = ref
		item["account_ref"] = ref
	}
	if token := accountToken(account); token != "" {
		item["has_access_token"] = true
		item["token_preview"] = tokenPreview(token)
	} else {
		item["has_access_token"] = false
		item["token_preview"] = ""
	}
	for _, key := range []string{"access_token", "accessToken", "token", "cookie_header", "session_token", "refresh_token", "id_token", "login_password", "password", "two_factor_secret", "totp_secret"} {
		delete(item, key)
	}
	category := accountStatusCategory(item)
	item["status_category"] = category
	item["status_label"] = map[string]string{
		"normal":   "正常",
		"limited":  "限流",
		"abnormal": "异常",
		"disabled": "禁用",
	}[category]
	return item
}

func accountsForAPI(items []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, accountForAPI(item))
	}
	return result
}

func isOpenAIAccountFields(item map[string]any) bool {
	token := accountToken(item)
	if token == "" {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(stringValue(item["source_type"])))
	switch source {
	case "grok", "grok_sso", "xai", "xai_sso":
		return false
	case "chatgpt_web", "oauth_login", "openai", "codex", "openai_oauth":
		return true
	}
	if strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "grok") {
		return false
	}
	// Legacy records did not always persist source_type. Keep those records
	// visible for compatibility; newly synchronized Grok records are explicitly
	// marked above and are therefore excluded.
	return true
}

func accountStatusCategory(account map[string]any) string {
	status := strings.ToLower(strings.TrimSpace(stringValue(account["status"])))
	reason := strings.ToLower(strings.TrimSpace(stringValue(account["status_reason_code"])))
	errorKind := strings.ToLower(strings.TrimSpace(stringValue(account["last_error_kind"])))
	if !boolValue(account["enabled"], true) || status == "disabled" || status == "auto_disabled" || status == "禁用" || reason == "disabled" {
		return "disabled"
	}
	if status == "limited" || status == "rate_limited" || status == "cooling" || status == "backoff" || status == "限流" {
		return "limited"
	}
	switch reason {
	case "pro_cooldown", "video_cooldown", "lane_backoff", "lane_degraded", "image_generation_unavailable", "image_quota_exhausted", "text_pending":
		return "limited"
	}
	switch errorKind {
	case "quota_exhausted", "media_pending", "media_generation_unavailable", "media_degraded", "lane_degraded", "text_pending":
		return "limited"
	case "auth_invalid", "parse_failure":
		return "abnormal"
	}
	if status == "abnormal" || status == "invalid" || status == "error" || status == "incomplete" || status == "异常" {
		if intValue(account["quota"]) > 0 || boolValue(account["survival_alive"], false) {
			return "normal"
		}
		return "abnormal"
	}
	return "normal"
}

// accountAutoRemoveInvalid reports whether an abnormal account is definitely
// unrecoverable by the automated token refresh path. An account with a
// refresh token is retained so it can still be rotated, while browser/session
// accounts with an expired or explicitly rejected access token are removable.
func accountAutoRemoveInvalid(account map[string]any) bool {
	if accountStatusCategory(account) != "abnormal" {
		return false
	}
	if strings.TrimSpace(stringValue(account["refresh_token"])) != "" {
		return false
	}
	if strings.TrimSpace(stringValue(account["login_password"])) != "" &&
		strings.TrimSpace(firstNonEmpty(stringValue(account["two_factor_secret"]), stringValue(account["totp_secret"]))) != "" {
		return false
	}

	status := strings.ToLower(strings.TrimSpace(stringValue(account["status"])))
	reason := strings.ToLower(strings.TrimSpace(stringValue(account["status_reason_code"])))
	errorKind := strings.ToLower(strings.TrimSpace(stringValue(account["last_error_kind"])))
	errorStatus := intValue(account["last_error_status"])
	if reason == "auth_invalid" || reason == "account_invalid" {
		return true
	}
	if status == "invalid" || status == "expired" || status == "unauthorized" {
		return true
	}
	return errorKind == "auth_invalid" && errorStatus == http.StatusUnauthorized
}

func accountStatusMatches(account map[string]any, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" || filter == "all" {
		return true
	}
	return accountStatusCategory(account) == filter || strings.ToLower(stringValue(account["status"])) == filter
}

func accountContains(account map[string]any, keyword string) bool {
	for _, key := range []string{"access_token", "email", "user_id", "type", "source_type", "status", "proxy", "group_id"} {
		if strings.Contains(strings.ToLower(stringValue(account[key])), keyword) {
			return true
		}
	}
	return false
}

func accountGroupMatches(account map[string]any, groupID string) bool {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || groupID == "all" {
		return true
	}
	current := stringValue(account["group_id"])
	if groupID == "__ungrouped__" {
		return current == ""
	}
	return current == groupID
}

func accountToken(account map[string]any) string {
	if token := stringValue(account["access_token"]); token != "" {
		return token
	}
	return stringValue(account["accessToken"])
}

func accountPublicRef(account map[string]any) string {
	if ref := firstNonEmpty(
		stringValue(account["id"]),
		stringValue(account["account_ref"]),
		stringValue(account["account_id"]),
		stringValue(account["chatgpt_account_id"]),
		stringValue(account["user_id"]),
		strings.ToLower(stringValue(account["email"])),
	); ref != "" {
		return ref
	}
	token := accountToken(account)
	if token == "" {
		token = firstNonEmpty(stringValue(account["sso"]), stringValue(account["session_token"]), stringValue(account["token"]))
	}
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return "acct_" + hex.EncodeToString(sum[:8])
}

func accountRefForToken(items []map[string]any, token string) string {
	token = strings.TrimSpace(token)
	for _, item := range items {
		if accountToken(item) == token {
			return accountPublicRef(item)
		}
	}
	return ""
}

func accountRefMatches(account map[string]any, ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	refLower := strings.ToLower(ref)
	for _, candidate := range []string{
		accountToken(account),
		accountPublicRef(account),
		stringValue(account["id"]),
		stringValue(account["account_ref"]),
		stringValue(account["account_id"]),
		stringValue(account["chatgpt_account_id"]),
		stringValue(account["user_id"]),
		stringValue(account["email"]),
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if candidate == ref || strings.ToLower(candidate) == refLower {
			return true
		}
	}
	return false
}

func uniqueAccountRefs(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		ref := strings.TrimSpace(value)
		if ref == "" {
			continue
		}
		key := strings.ToLower(ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, ref)
	}
	return result
}

func resolveAccountRefTokens(items []map[string]any, refs []string) ([]string, []string) {
	refs = uniqueAccountRefs(refs)
	tokens := make([]string, 0, len(refs))
	missing := []string{}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		found := false
		for _, item := range items {
			if !accountRefMatches(item, ref) {
				continue
			}
			found = true
			if token := accountToken(item); token != "" {
				if _, ok := seen[token]; !ok {
					seen[token] = struct{}{}
					tokens = append(tokens, token)
				}
			}
			break
		}
		if !found {
			missing = append(missing, ref)
		}
	}
	return tokens, missing
}

func uniqueAccountTokens(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		token := strings.TrimSpace(value)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, token)
	}
	return result
}

func (s *Server) accountGroupsPayload() map[string]any {
	cfg, err := s.store.Config()
	if err != nil {
		return map[string]any{"groups": []map[string]any{}, "proxy_groups": []map[string]any{}}
	}
	return map[string]any{
		"groups":       accountGroupsFromConfig(cfg, s.accountCountByGroup()),
		"proxy_groups": mapList(cfg["proxy_groups"]),
	}
}

func (s *Server) accountCountByGroup() map[string]int {
	counts := map[string]int{}
	items, err := s.store.AccountList()
	if err != nil {
		return counts
	}
	for _, item := range items {
		if id := stringValue(item["group_id"]); id != "" {
			counts[id]++
		}
	}
	return counts
}

func accountGroupsFromConfig(cfg map[string]any, counts map[string]int) []map[string]any {
	groups := mapList(cfg["account_groups"])
	result := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		id := slugID(stringValue(group["id"]))
		if id == "" {
			continue
		}
		proxy := stringValue(group["proxy"])
		item := map[string]any{
			"id":             id,
			"name":           firstNonEmpty(stringValue(group["name"]), id),
			"proxy":          proxy,
			"proxy_group_id": strings.TrimPrefix(proxy, "group:"),
			"enabled":        boolValue(group["enabled"], true),
			"notes":          stringValue(group["notes"]),
			"account_count":  counts[id],
		}
		result = append(result, item)
	}
	return result
}

func mapList(value any) []map[string]any {
	raw, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return typed
		}
		return []map[string]any{}
	}
	result := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func boolValue(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func tokenPreview(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	if len(token) <= 12 {
		return "********"
	}
	return token[:6] + "..." + token[len(token)-4:]
}

func slugID(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			builder.WriteRune(r)
		case unicode.IsSpace(r):
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-_")
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	staticDir := s.cfg.StaticDir
	relative := strings.TrimPrefix(filepath.ToSlash(filepath.Clean("/"+r.URL.Path)), "/")
	if relative == "" || relative == "." {
		relative = "index.html"
	}
	candidate := filepath.Join(staticDir, filepath.FromSlash(relative))
	if !isWithin(staticDir, candidate) {
		http.NotFound(w, r)
		return
	}
	if info, err := os.Stat(candidate); err != nil || info.IsDir() {
		candidate = filepath.Join(staticDir, "index.html")
	}
	if _, err := os.Stat(candidate); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "frontend assets are not built",
			"hint":  "run npm --prefix web-vue run build:server",
		})
		return
	}
	if filepath.Base(candidate) == "index.html" {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	}
	http.ServeFile(w, r, candidate)
}

func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAPIRequest(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing API key", "authentication_error")
	return false
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.ValidAdminRequest(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "invalid or missing admin key", "authentication_error")
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}

func serveTextFile(w http.ResponseWriter, filename, contentType string) {
	raw, err := os.ReadFile(filename)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found", "not_found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func isWithin(root, candidate string) bool {
	root, _ = filepath.Abs(root)
	candidate, _ = filepath.Abs(candidate)
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}
