package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	proxyruntime "github.com/auucoder/gptgrok2api-go/internal/proxy"
	"golang.org/x/crypto/sha3"
)

// OpenAIImage implements the authenticated ChatGPT Web image flow. It is kept
// separate from Media because ChatGPT JWTs and Grok SSO cookies are different
// credential families and must never share request headers or account pools.
type OpenAIImage struct {
	BaseURL        string
	Client         *http.Client
	Proxy          *proxyruntime.Manager
	RequestTimeout time.Duration
	browserMu      sync.Mutex
	browsers       map[string]*browserHTTP
}

type OpenAIImageInput struct {
	Name string
	MIME string
	Data []byte
}

var openAIImageFileIDPattern = regexp.MustCompile(`(?i)^file_[a-z0-9][a-z0-9_-]{5,}$`)

// OpenAIImageStageFunc receives the elapsed time for a completed image-pipeline
// stage. It is intentionally kept in the provider package so callers can add
// observability without coupling the provider to HTTP handlers.
type OpenAIImageStageFunc func(metric string, elapsed time.Duration)

// OpenAIImageEgressFunc receives the proxy URL selected for an upstream call.
// Callers should sanitize it before exposing it in logs or UI.
type OpenAIImageEgressFunc func(proxyURL string)

type openAIImageStageContextKey struct{}
type openAIImageEgressContextKey struct{}

// WithOpenAIImageStage attaches an optional stage observer to ctx.
func WithOpenAIImageStage(ctx context.Context, observer OpenAIImageStageFunc) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIImageStageContextKey{}, observer)
}

// WithOpenAIImageEgress attaches an optional egress observer to ctx.
func WithOpenAIImageEgress(ctx context.Context, observer OpenAIImageEgressFunc) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIImageEgressContextKey{}, observer)
}

func notifyOpenAIImageStage(ctx context.Context, metric string, started time.Time) {
	if observer, ok := ctx.Value(openAIImageStageContextKey{}).(OpenAIImageStageFunc); ok && observer != nil {
		observer(metric, time.Since(started))
	}
}

func notifyOpenAIImageEgress(ctx context.Context, proxyURL string) {
	if observer, ok := ctx.Value(openAIImageEgressContextKey{}).(OpenAIImageEgressFunc); ok && observer != nil {
		observer(proxyURL)
	}
}

const openAIImageDefaultPollTimeout = 3 * time.Minute
const openAIImageUploadAttemptTimeout = 60 * time.Second

func NewOpenAIImage(baseURL string, client *http.Client, proxy *proxyruntime.Manager, timeout time.Duration) *OpenAIImage {
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	return &OpenAIImage{
		BaseURL:        strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Client:         client,
		Proxy:          proxy,
		RequestTimeout: timeout,
		browsers:       map[string]*browserHTTP{},
	}
}

func (o *OpenAIImage) SetProxyManager(manager *proxyruntime.Manager) {
	o.Proxy = manager
}

func (o *OpenAIImage) Generate(ctx context.Context, account accounts.Account, prompt, model, size, quality string, inputs []OpenAIImageInput) (results []ImageResult, err error) {
	if strings.TrimSpace(account.Token) == "" {
		return nil, fmt.Errorf("OpenAI image generation requires an access token")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("prompt cannot be empty")
	}
	if size == "" || strings.EqualFold(size, "auto") {
		size = "1024x1024"
	}
	size = NormalizeOpenAIImageSize(size)
	if quality == "" {
		quality = "auto"
	}
	if o.Proxy != nil {
		lease := o.Proxy.AcquireImage(account.Fields)
		ctx = proxyruntime.WithURL(ctx, lease.URL)
		notifyOpenAIImageEgress(ctx, lease.URL)
		defer func() { lease.Release(openAIImageProxyFailure(err)) }()
	}
	prompt = strings.TrimSpace(prompt) + "\n\n输出图片尺寸为 " + size + "。\n输出图片质量为 " + quality + "。"
	references := make([]openAIImageReference, 0, len(inputs))
	inputStarted := time.Now()
	for index, input := range inputs {
		ref, err := o.uploadInput(ctx, account, input, index+1)
		if err != nil {
			return nil, err
		}
		references = append(references, ref)
	}
	if len(inputs) > 0 {
		notifyOpenAIImageStage(ctx, "upload_ms", inputStarted)
	}

	stageStarted := time.Now()
	scripts, build, err := o.bootstrap(ctx, account)
	if err != nil {
		return nil, err
	}
	notifyOpenAIImageStage(ctx, "bootstrap_ms", stageStarted)

	stageStarted = time.Now()
	requirements, err := o.chatRequirements(ctx, account, scripts, build)
	if err != nil {
		return nil, err
	}
	notifyOpenAIImageStage(ctx, "requirements_ms", stageStarted)

	stageStarted = time.Now()
	conduit, err := o.prepare(ctx, account, requirements, prompt, model)
	if err != nil {
		return nil, err
	}
	notifyOpenAIImageStage(ctx, "prepare_conversation_ms", stageStarted)

	stageStarted = time.Now()
	conversationID, imageRefs, err := o.start(ctx, account, requirements, conduit, prompt, model, size, quality, references)
	if err != nil {
		return nil, err
	}
	notifyOpenAIImageStage(ctx, "generation_start_ms", stageStarted)
	notifyOpenAIImageStage(ctx, "conversation_stream_ms", stageStarted)
	if conversationID == "" && len(imageRefs) == 0 {
		return nil, fmt.Errorf("OpenAI image stream returned no conversation id")
	}
	if len(imageRefs) == 0 {
		if conversationID == "" {
			return nil, fmt.Errorf("OpenAI image generation returned no downloadable files")
		}
		stageStarted = time.Now()
		imageRefs, err = o.pollConversation(ctx, account, conversationID)
		if err != nil {
			return nil, err
		}
		notifyOpenAIImageStage(ctx, "resolve_ms", stageStarted)
	}
	if len(imageRefs) == 0 {
		return nil, fmt.Errorf("OpenAI image generation completed without image files")
	}
	results = make([]ImageResult, 0, len(imageRefs))
	seen := map[string]bool{}
	inputFileIDs := map[string]bool{}
	var lastDownloadErr error
	for _, ref := range references {
		if ref.FileID != "" {
			inputFileIDs[ref.FileID] = true
		}
	}
	for _, imageRef := range imageRefs {
		fileID := strings.TrimPrefix(imageRef, "file-service://")
		// The upstream SSE/poll response can echo uploaded reference assets.
		// Those are inputs, not generated outputs, and must never be returned or
		// persisted in the generated-image gallery.
		if imageRef == "" || seen[imageRef] || inputFileIDs[fileID] {
			continue
		}
		seen[imageRef] = true
		downloadStarted := time.Now()
		raw, mime, err := o.downloadImageRef(ctx, account, conversationID, imageRef)
		if err != nil {
			lastDownloadErr = err
			continue
		}
		notifyOpenAIImageStage(ctx, "download_ms", downloadStarted)
		results = append(results, ImageResult{
			Base64: base64.StdEncoding.EncodeToString(raw),
			MIME:   mime,
		})
	}
	if len(results) > 0 {
		notifyOpenAIImageStage(ctx, "total_ms", inputStarted)
	}
	if len(results) == 0 {
		if lastDownloadErr != nil {
			return nil, lastDownloadErr
		}
		return nil, fmt.Errorf("OpenAI image generation returned no downloadable files")
	}
	return results, nil
}

// NormalizeOpenAIImageSize keeps legacy UI presets compatible with the
// upstream image pipeline, which requires dimensions aligned to 16 pixels.
func NormalizeOpenAIImageSize(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1024x1365":
		return "1024x1360"
	case "1365x1024":
		return "1360x1024"
	case "1920x1080":
		return "1920x1088"
	case "1080x1920":
		return "1088x1920"
	default:
		return strings.TrimSpace(size)
	}
}

func (o *OpenAIImage) Resolve(ctx context.Context, account accounts.Account, image ImageResult, responseFormat, imageDir, publicBase string) (map[string]string, string, error) {
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return nil, "", fmt.Errorf("response_format must be url or b64_json")
	}
	if image.Base64 == "" {
		return nil, "", fmt.Errorf("OpenAI image result has no image bytes")
	}
	raw, err := base64.StdEncoding.DecodeString(image.Base64)
	if err != nil {
		return nil, "", fmt.Errorf("decode OpenAI image: %w", err)
	}
	if err := ensureDir(imageDir); err != nil {
		return nil, "", err
	}
	id := randomMediaID()
	ext := ".png"
	if strings.EqualFold(image.MIME, "image/jpeg") {
		ext = ".jpg"
	}
	if err := writeMediaFile(imageDir, id, ext, raw); err != nil {
		return nil, "", err
	}
	value := "/v1/files/image?id=" + id
	if strings.TrimSpace(publicBase) != "" {
		value = strings.TrimRight(publicBase, "/") + value
	}
	if format == "b64_json" {
		return map[string]string{"b64_json": image.Base64}, value, nil
	}
	return map[string]string{"url": value}, value, nil
}

type openAIRequirements struct {
	Token      string
	ProofToken string
	Turnstile  string
	SOToken    string
}

type openAIImageReference struct {
	FileID   string
	FileName string
	FileSize int
	MIME     string
	Width    int
	Height   int
}

func (o *OpenAIImage) bootstrap(ctx context.Context, account accounts.Account) ([]string, string, error) {
	response, err := o.do(ctx, http.MethodGet, "/", account, nil, nil, false)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", readOpenAIHTTPError(response, "bootstrap")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	sources := []string{}
	re := regexp.MustCompile(`(?i)<script[^>]+src=["']([^"']+)["']`)
	for _, match := range re.FindAllStringSubmatch(string(raw), -1) {
		if len(match) < 2 {
			continue
		}
		value, unescapeErr := url.QueryUnescape(html.UnescapeString(match[1]))
		if unescapeErr == nil && value != "" {
			sources = append(sources, value)
		}
	}
	if len(sources) == 0 {
		sources = []string{"https://chatgpt.com/backend-api/sentinel/sdk.js"}
	}
	build := ""
	if match := regexp.MustCompile(`data-build=["']([^"']+)["']`).FindStringSubmatch(string(raw)); len(match) == 2 {
		build = match[1]
	}
	return sources, build, nil
}

func (o *OpenAIImage) chatRequirements(ctx context.Context, account accounts.Account, scripts []string, build string) (openAIRequirements, error) {
	legacy := buildLegacyRequirementsToken(openAIUserAgent, scripts, build)
	prepareBody := map[string]any{"p": legacy}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/prepare", account, prepareBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var prepare map[string]any
	if err := decodeOpenAIJSON(response, &prepare, "chat requirements prepare"); err != nil {
		return openAIRequirements{}, err
	}
	proof := ""
	if info, ok := prepare["proofofwork"].(map[string]any); ok && boolValue(info["required"]) {
		proof = buildProofToken(stringValue(info["seed"]), stringValue(info["difficulty"]), openAIUserAgent, scripts, build)
	}
	turnstile := ""
	if info, ok := prepare["turnstile"].(map[string]any); ok && boolValue(info["required"]) {
		dx := stringValue(info["dx"])
		if dx != "" {
			turnstile, err = solveSentinelTurnstileToken(dx, legacy)
			if err != nil {
				return openAIRequirements{}, fmt.Errorf("solve OpenAI sentinel Turnstile: %w", err)
			}
		}
	}
	finalizeBody := map[string]any{
		"prepare_token":   stringValue(prepare["prepare_token"]),
		"proof_token":     proof,
		"turnstile_token": turnstile,
	}
	response, err = o.do(ctx, http.MethodPost, "/backend-api/sentinel/chat-requirements/finalize", account, finalizeBody, map[string]string{"Content-Type": "application/json"}, false)
	if err != nil {
		return openAIRequirements{}, err
	}
	var finalized map[string]any
	if err := decodeOpenAIJSON(response, &finalized, "chat requirements finalize"); err != nil {
		return openAIRequirements{}, err
	}
	token := stringValue(finalized["token"])
	if token == "" {
		return openAIRequirements{}, fmt.Errorf("OpenAI chat requirements returned no token")
	}
	return openAIRequirements{Token: token, ProofToken: proof, Turnstile: turnstile, SOToken: stringValue(finalized["so_token"])}, nil
}

func (o *OpenAIImage) prepare(ctx context.Context, account accounts.Account, requirements openAIRequirements, prompt, model string) (string, error) {
	payload := map[string]any{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     firstUUID(),
		"model":                 openAIImageModel(model),
		"client_prepare_state":  "success",
		"timezone_offset_min":   -480,
		"timezone":              "Asia/Shanghai",
		"conversation_mode":     map[string]any{"kind": "primary_assistant"},
		"system_hints":          []any{"picture_v2"},
		"partial_query": map[string]any{
			"id": firstUUID(), "author": map[string]any{"role": "user"},
			"content": map[string]any{"content_type": "text", "parts": []any{prompt}},
		},
		"supports_buffering":     true,
		"supported_encodings":    []any{"v1"},
		"client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", account, payload, o.requirementHeaders(requirements), false)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := decodeOpenAIJSON(response, &value, "image conversation prepare"); err != nil {
		return "", err
	}
	token := stringValue(value["conduit_token"])
	if token == "" {
		return "", fmt.Errorf("OpenAI image prepare returned no conduit token")
	}
	return token, nil
}

func (o *OpenAIImage) start(ctx context.Context, account accounts.Account, requirements openAIRequirements, conduit, prompt, model, size, quality string, references []openAIImageReference) (string, []string, error) {
	parts := make([]any, 0, len(references)+1)
	attachments := make([]any, 0, len(references))
	for _, ref := range references {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "file-service://" + ref.FileID,
			"width":         ref.Width, "height": ref.Height, "size_bytes": ref.FileSize,
		})
		attachments = append(attachments, map[string]any{
			"id": ref.FileID, "mimeType": ref.MIME, "name": ref.FileName,
			"size": ref.FileSize, "width": ref.Width, "height": ref.Height,
		})
	}
	parts = append(parts, prompt)
	contentType := "text"
	if len(references) > 0 {
		contentType = "multimodal_text"
	}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{},
		"selected_github_repos":        []any{},
		"selected_all_github_repos":    false,
		"system_hints":                 []any{"picture_v2"},
		"serialization_metadata":       map[string]any{"custom_symbol_offsets": []any{}},
	}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{
			"id": firstUUID(), "author": map[string]any{"role": "user"},
			"create_time": float64(time.Now().UnixNano()) / 1e9,
			"content":     map[string]any{"content_type": contentType, "parts": parts},
			"metadata":    metadata,
		}},
		"parent_message_id":                    firstUUID(),
		"model":                                openAIImageModel(model),
		"client_prepare_state":                 "sent",
		"timezone_offset_min":                  -480,
		"timezone":                             "Asia/Shanghai",
		"conversation_mode":                    map[string]any{"kind": "primary_assistant"},
		"enable_message_followups":             true,
		"system_hints":                         []any{"picture_v2"},
		"supports_buffering":                   true,
		"supported_encodings":                  []any{"v1"},
		"client_contextual_info":               map[string]any{"app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"image_generation_size":                size,
		"image_generation_quality":             quality,
	}
	headers := o.requirementHeaders(requirements)
	headers["X-Conduit-Token"] = conduit
	headers["Accept"] = "text/event-stream"
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation", account, payload, headers, true)
	if err != nil {
		return "", nil, err
	}
	defer response.Body.Close()
	conversationID := ""
	fileIDs := []string{}
	err = scanOpenAISSE(response.Body, func(raw []byte) bool {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return false
		}
		collectOpenAIImageRefs(value, &conversationID, &fileIDs)
		return false
	})
	if err != nil {
		return "", nil, err
	}
	return conversationID, uniqueStrings(fileIDs), nil
}

func (o *OpenAIImage) pollConversation(ctx context.Context, account accounts.Account, conversationID string) ([]string, error) {
	timeout := o.RequestTimeout
	if timeout <= 0 {
		timeout = openAIImageDefaultPollTimeout
	}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 && remaining < timeout {
			timeout = remaining
		}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, err := o.do(ctx, http.MethodGet, "/backend-api/conversation/"+url.PathEscape(conversationID), account, nil, nil, false)
		if err == nil {
			var value any
			if decodeErr := decodeOpenAIJSON(response, &value, "image conversation"); decodeErr == nil {
				id := ""
				ids := []string{}
				collectOpenAIImageRefs(value, &id, &ids)
				if len(ids) > 0 {
					return uniqueStrings(ids), nil
				}
			}
		} else if !isRetryableOpenAIError(err) {
			return nil, err
		}
		wait := 5 * time.Second
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
	return nil, &protocol.UpstreamError{Status: http.StatusGatewayTimeout, Message: "OpenAI image result polling timed out", Body: "OpenAI image result polling timed out"}
}

func (o *OpenAIImage) uploadInput(ctx context.Context, account accounts.Account, input OpenAIImageInput, index int) (openAIImageReference, error) {
	if len(input.Data) == 0 {
		return openAIImageReference{}, fmt.Errorf("image input %d is empty", index)
	}
	mimeType := strings.TrimSpace(input.MIME)
	if mimeType == "" {
		mimeType = "image/png"
	}
	width, height := imageDimensions(input.Data)
	name := input.Name
	if name == "" {
		name = fmt.Sprintf("image_%d.png", index)
	}
	payload := map[string]any{
		"file_name": name, "file_size": len(input.Data), "use_case": "multimodal",
		"width": width, "height": height,
	}
	response, err := o.do(ctx, http.MethodPost, "/backend-api/files", account, payload, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, false)
	if err != nil {
		return openAIImageReference{}, err
	}
	var meta map[string]any
	if err := decodeOpenAIJSON(response, &meta, "image upload prepare"); err != nil {
		return openAIImageReference{}, err
	}
	fileID := stringValue(meta["file_id"])
	uploadURL := stringValue(meta["upload_url"])
	if fileID == "" || uploadURL == "" {
		return openAIImageReference{}, fmt.Errorf("OpenAI image upload returned incomplete metadata")
	}
	err = o.uploadInputBlob(ctx, account, uploadURL, input.Data, map[string]string{
		"Content-Type": mimeType, "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08",
		"Accept": "application/json, text/plain, */*",
	})
	if err != nil {
		return openAIImageReference{}, err
	}
	response, err = o.do(ctx, http.MethodPost, "/backend-api/files/"+url.PathEscape(fileID)+"/uploaded", account, map[string]any{}, map[string]string{"Content-Type": "application/json", "Accept": "application/json"}, false)
	if err != nil {
		return openAIImageReference{}, err
	}
	_ = response.Body.Close()
	return openAIImageReference{FileID: fileID, FileName: name, FileSize: len(input.Data), MIME: mimeType, Width: width, Height: height}, nil
}

// uploadInputBlob retries only the replayable signed-storage PUT. The normal
// ChatGPT flow stays pinned to one proxy; retry egress is restricted to a
// different group node with multiple successful real image requests.
func (o *OpenAIImage) uploadInputBlob(ctx context.Context, account accounts.Account, endpoint string, data []byte, headers map[string]string) error {
	proxyURL, selected := proxyruntime.URLSelectionFromContext(ctx)
	if !selected && o.Proxy != nil {
		proxyURL = o.Proxy.Resolve(account.Fields, false)
	}
	routes := []struct {
		url   string
		lease *proxyruntime.Lease
	}{{url: proxyURL}}
	if proxyURL != "" && o.Proxy != nil {
		if stable := o.Proxy.AcquireStableImage(account.Fields, proxyURL); stable != nil {
			routes = append(routes, struct {
				url   string
				lease *proxyruntime.Lease
			}{url: stable.URL, lease: stable})
		}
	}
	if len(routes) == 1 {
		routes = append(routes, routes[0])
	}
	var lastErr error
	for index, route := range routes {
		attemptTimeout := minDuration(o.RequestTimeout, openAIImageUploadAttemptTimeout)
		requestCtx, cancel := context.WithTimeout(proxyruntime.WithURL(ctx, route.url), attemptTimeout)
		response, err := o.doAbsolute(requestCtx, http.MethodPut, endpoint, account, bytes.NewReader(data), headers, false)
		cancel()
		if route.lease != nil {
			route.lease.Release(openAIImageProxyFailure(err))
		}
		if err == nil {
			_ = response.Body.Close()
			return nil
		}
		lastErr = err
		if !isRetryableOpenAIUploadError(err) {
			return err
		}
		if index+1 < len(routes) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return fmt.Errorf("OpenAI image upload failed after retry: %w", lastErr)
}

func isRetryableOpenAIUploadError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var upstream *protocol.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == http.StatusForbidden || upstream.Status == http.StatusRequestTimeout ||
			upstream.Status == http.StatusTooManyRequests || upstream.Status >= http.StatusInternalServerError
	}
	return true
}

func (o *OpenAIImage) downloadFile(ctx context.Context, account accounts.Account, fileID string) ([]byte, string, error) {
	response, err := o.do(ctx, http.MethodGet, "/backend-api/files/"+url.PathEscape(fileID)+"/download", account, nil, nil, false)
	if err != nil {
		return nil, "", err
	}
	return o.readImageDownloadResponse(ctx, account, response)
}

func (o *OpenAIImage) downloadImageRef(ctx context.Context, account accounts.Account, conversationID, imageRef string) ([]byte, string, error) {
	if strings.HasPrefix(imageRef, "sediment://") {
		attachmentID := strings.TrimPrefix(imageRef, "sediment://")
		if conversationID == "" || attachmentID == "" {
			return nil, "", fmt.Errorf("OpenAI sediment image reference is incomplete")
		}
		path := "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(attachmentID) + "/download"
		response, err := o.do(ctx, http.MethodGet, path, account, nil, map[string]string{
			"Accept":                "application/json, image/*, */*",
			"Referer":               o.BaseURL + "/c/" + conversationID,
			"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download",
		}, false)
		if err != nil {
			return nil, "", err
		}
		return o.readImageDownloadResponse(ctx, account, response)
	}
	return o.downloadFile(ctx, account, strings.TrimPrefix(imageRef, "file-service://"))
}

func (o *OpenAIImage) readImageDownloadResponse(ctx context.Context, account accounts.Account, response *http.Response) ([]byte, string, error) {
	var value map[string]any
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	_ = response.Body.Close()
	if err != nil {
		return nil, "", err
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "application/json") || json.Valid(raw) {
		if json.Unmarshal(raw, &value) != nil {
			return nil, "", fmt.Errorf("decode OpenAI image download metadata")
		}
		downloadURL := firstStringValue(value, "download_url", "url")
		if downloadURL == "" {
			return nil, "", fmt.Errorf("OpenAI image download returned no URL")
		}
		response, err = o.doAbsolute(ctx, http.MethodGet, downloadURL, account, nil, nil, false)
		if err != nil {
			return nil, "", err
		}
		raw, err = io.ReadAll(io.LimitReader(response.Body, 64<<20))
		_ = response.Body.Close()
		if err != nil {
			return nil, "", err
		}
	}
	if len(raw) == 0 {
		return nil, "", fmt.Errorf("OpenAI image download returned empty content")
	}
	mimeType := response.Header.Get("Content-Type")
	if semi := strings.IndexByte(mimeType, ';'); semi >= 0 {
		mimeType = mimeType[:semi]
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return raw, mimeType, nil
}

func (o *OpenAIImage) requirementHeaders(requirements openAIRequirements) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
		"OpenAI-Sentinel-Chat-Requirements-Token": requirements.Token,
	}
	if requirements.ProofToken != "" {
		headers["OpenAI-Sentinel-Proof-Token"] = requirements.ProofToken
	}
	if requirements.Turnstile != "" {
		headers["OpenAI-Sentinel-Turnstile-Token"] = requirements.Turnstile
	}
	if requirements.SOToken != "" {
		headers["OpenAI-Sentinel-SO-Token"] = requirements.SOToken
	}
	return headers
}

func (o *OpenAIImage) do(ctx context.Context, method, path string, account accounts.Account, body any, extra map[string]string, stream bool) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if body != nil {
		if err != nil {
			return nil, err
		}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(raw)
	}
	return o.doRequest(ctx, method, o.BaseURL+path, path, account, reader, extra, stream, true)
}

func (o *OpenAIImage) doAbsolute(ctx context.Context, method, endpoint string, account accounts.Account, body io.Reader, extra map[string]string, stream bool) (*http.Response, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	return o.doRequest(ctx, method, endpoint, parsed.Path, account, body, extra, stream, parsed.Hostname() == "chatgpt.com")
}

func (o *OpenAIImage) doRequest(ctx context.Context, method, endpoint, targetPath string, account accounts.Account, body io.Reader, extra map[string]string, stream, authenticated bool) (*http.Response, error) {
	proxyURL, selected := proxyruntime.URLSelectionFromContext(ctx)
	if !selected && o.Proxy != nil {
		proxyURL = o.Proxy.Resolve(account.Fields, false)
		ctx = proxyruntime.WithURL(ctx, proxyURL)
	}
	notifyOpenAIImageEgress(ctx, proxyURL)
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if authenticated {
		setOpenAIHeaders(request, targetPath, account.Token, account.Fields)
	}
	if extra != nil {
		for key, value := range extra {
			request.Header.Set(key, value)
		}
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	client := o.Client
	if browserEligible(o.BaseURL) && authenticated {
		browser, browserErr := o.browserFor(proxyURL)
		if browserErr != nil {
			return nil, browserErr
		}
		return browser.Do(request)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstreamErr := readOpenAIHTTPError(response, targetPath)
		_ = response.Body.Close()
		return nil, upstreamErr
	}
	return response, nil
}

func openAIImageProxyFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var upstream *protocol.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == http.StatusForbidden
	}
	return true
}

func (o *OpenAIImage) browserFor(proxyURL string) (*browserHTTP, error) {
	o.browserMu.Lock()
	defer o.browserMu.Unlock()
	if browser := o.browsers[proxyURL]; browser != nil {
		return browser, nil
	}
	timeout := o.RequestTimeout
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	browser, err := newBrowserHTTP(proxyURL, timeout)
	if err != nil {
		return nil, err
	}
	o.browsers[proxyURL] = browser
	return browser, nil
}

func decodeOpenAIJSON(response *http.Response, value any, target string) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return readOpenAIHTTPError(response, target)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(value); err != nil {
		return fmt.Errorf("decode %s response: %w", target, err)
	}
	return nil
}

func readOpenAIHTTPError(response *http.Response, target string) error {
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	message := strings.TrimSpace(string(raw))
	if message == "" {
		message = fmt.Sprintf("OpenAI endpoint %s returned HTTP %d", target, response.StatusCode)
	}
	return &protocol.UpstreamError{Status: response.StatusCode, Message: message, Body: message}
}

func scanOpenAISSE(reader io.Reader, onPayload func([]byte) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	lines := []string{}
	flush := func() bool {
		if len(lines) == 0 {
			return false
		}
		payload := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = nil
		if payload == "" || payload == "[DONE]" {
			return payload == "[DONE]"
		}
		return onPayload([]byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if line == "" {
			if flush() {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	_ = flush()
	return nil
}

func collectOpenAIImageRefs(value any, conversationID *string, fileIDs *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "conversation_id", "conversationid", "conversation-id":
				if *conversationID == "" {
					*conversationID = stringValue(item)
				}
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "asset_pointer", "assetpointer":
				pointer := stringValue(item)
				if strings.HasPrefix(pointer, "file-service://") {
					*fileIDs = append(*fileIDs, strings.TrimPrefix(pointer, "file-service://"))
				} else if strings.HasPrefix(pointer, "sediment://") {
					*fileIDs = append(*fileIDs, pointer)
				}
			case "file_id", "fileid":
				id := stringValue(item)
				if isOpenAIImageFileID(id) {
					*fileIDs = append(*fileIDs, id)
				}
			}
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case []any:
		for _, item := range typed {
			collectOpenAIImageRefs(item, conversationID, fileIDs)
		}
	case string:
		if *conversationID == "" {
			if match := regexp.MustCompile(`(?i)"conversation[_-]?id"\s*:\s*"([^"]+)"`).FindStringSubmatch(typed); len(match) == 2 {
				*conversationID = strings.TrimSpace(match[1])
			}
		}
		for _, match := range regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
		for _, match := range regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, "sediment://"+match[1])
			}
		}
		for _, match := range regexp.MustCompile(`(?i)\b(file_[a-z0-9][a-z0-9_-]{5,})\b`).FindAllStringSubmatch(typed, -1) {
			if len(match) == 2 {
				*fileIDs = append(*fileIDs, match[1])
			}
		}
	}
}

func isOpenAIImageFileID(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "file-service://") {
		value = strings.TrimPrefix(value, "file-service://")
	}
	return openAIImageFileIDPattern.MatchString(value)
}

func openAIImageModel(model string) string {
	if strings.EqualFold(strings.TrimSpace(model), "gpt-image-2") {
		return "gpt-5-3"
	}
	return strings.TrimSpace(model)
}

func buildLegacyRequirementsToken(userAgent string, scripts []string, build string) string {
	config := []any{
		"1920x1080", time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)"),
		4294705152, 1, userAgent, firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, "en-US", "en-US,es-US,en,es", mathrand.Float64(),
		"webdriver-false", "location", "navigator", mathrand.Float64() * 1000, firstUUID(), "",
		[]int{8, 16, 24, 32}[mathrand.Intn(4)], time.Now().UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	raw, _ := json.Marshal(config)
	return "gAAAAAC" + base64.StdEncoding.EncodeToString(raw)
}

func buildProofToken(seed, difficulty, userAgent string, scripts []string, build string) string {
	target, err := hexBytes(difficulty)
	if err != nil || len(target) == 0 {
		return ""
	}
	config := []any{
		"1920x1080", time.Now().UTC().Format(time.RFC3339), 4294705152, 0, userAgent,
		firstStringValue(map[string]any{"script": firstNonEmptyString(scripts, "https://chatgpt.com/backend-api/sentinel/sdk.js")}, "script"),
		build, "en-US", "en-US,es-US,en,es", 0.5, "webdriver-false", "location", "navigator",
		0, firstUUID(), "", 8, time.Now().UnixMilli(), 0, 0, 0, 0, 0, 0, 0, 0,
	}
	for i := 0; i < 500000; i++ {
		config[3] = i
		config[9] = i >> 1
		raw, _ := json.Marshal(config)
		encoded := base64.StdEncoding.EncodeToString(raw)
		sum := sha3.Sum512(append([]byte(seed), []byte(encoded)...))
		if len(target) <= len(sum) && bytes.Compare(sum[:len(target)], target) <= 0 {
			return "gAAAAAB" + encoded
		}
	}
	return ""
}

func hexBytes(value string) ([]byte, error) {
	if len(value)%2 != 0 {
		return nil, fmt.Errorf("invalid hex difficulty")
	}
	result := make([]byte, len(value)/2)
	for i := range result {
		var parsed uint8
		if _, err := fmt.Sscanf(value[i*2:i*2+2], "%02x", &parsed); err != nil {
			return nil, err
		}
		result[i] = parsed
	}
	return result, nil
}

func imageDimensions(raw []byte) (int, int) {
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 1024, 1024
	}
	return config.Width, config.Height
}

func randomMediaID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return fmt.Sprintf("%x", raw[:])
	}
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

func writeMediaFile(dir, id, ext string, raw []byte) error {
	return os.WriteFile(filepath.Join(dir, id+ext), raw, 0o644)
}

func firstStringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := stringValue(value[key]); text != "" {
			return text
		}
	}
	return ""
}

func firstNonEmptyString(values []string, fallback string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return fallback
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

func isRetryableOpenAIError(err error) bool {
	var upstream *protocol.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status == 404 || upstream.Status == 409 || upstream.Status == 429 || upstream.Status >= 500
	}
	return true
}
