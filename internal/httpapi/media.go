package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

var cookieUserIDPattern = regexp.MustCompile(`(?:^|;\s*)x-userid=([^;]+)`)

const maxImageEditReferenceBytes = 50 << 20

var imageEditReferenceFields = []string{"image", "image[]", "images", "images[]", "image_url", "image_url[]"}
var imageEditMaskFields = []string{"mask", "mask[]"}

type imageEditRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
	Inputs         []provider.OpenAIImageInput
	HasMask        bool
}

type imageEditParseError struct {
	Message string
}

func (e imageEditParseError) Error() string { return e.Message }

func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": s.requestMonitorElapsed(r)})
	var request struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		ResponseFormat string `json:"response_format"`
	}
	if !decodeJSON(w, r, &request) {
		return
	}
	s.enrichRequestMonitor(r, map[string]any{"model": request.Model})
	if strings.TrimSpace(request.Prompt) == "" {
		writeError(w, http.StatusBadRequest, "prompt cannot be empty", "invalid_request_error")
		return
	}
	if request.N == 0 {
		request.N = 1
	}
	if request.Size == "" {
		request.Size = "1024x1024"
	}
	if isOpenAIImageModel(request.Model) {
		request.Size = provider.NormalizeOpenAIImageSize(request.Size)
	}
	if request.ResponseFormat == "" {
		request.ResponseFormat = "url"
	}
	if request.Quality == "" {
		request.Quality = "auto"
	}
	if request.N < 1 || request.N > 10 {
		writeError(w, http.StatusBadRequest, "n must be between 1 and 10", "invalid_request_error")
		return
	}
	spec, ok := model.Find(s.catalog, request.Model)
	if !ok || spec.Capability&model.Image == 0 {
		writeError(w, http.StatusBadRequest, "model is not an image model", "invalid_request_error")
		return
	}
	if !isOpenAIImageModel(request.Model) {
		if _, ok := protocol.AspectRatio(request.Size); !ok {
			writeError(w, http.StatusBadRequest, "invalid image size", "invalid_request_error")
			return
		}
	} else if !validOpenAIImageSize(request.Size) {
		writeError(w, http.StatusBadRequest, "invalid image size", "invalid_request_error")
		return
	}
	if isOpenAIImageModel(request.Model) {
		data, err := s.generateOpenAIImageData(r, r.Context(), request.Prompt, request.Model, request.Size, request.Quality, nil, request.ResponseFormat, requestPublicBase(r), request.N)
		if err != nil {
			writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
			return
		}
		s.stageRequestMonitor(r, "image_response_ready", 99, map[string]any{"response_ms": s.requestMonitorElapsed(r)})
		writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
		return
	}
	s.stageRequestMonitor(r, "image_egress_waiting", 30, map[string]any{"egress_wait_ms": 0})
	accountStarted := time.Now()
	lease, err := s.accountPool.Reserve(r.Context(), mediaPools(request.Model), nil)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error")
		return
	}
	defer s.accountPool.Release(lease)
	s.enrichMonitorAccount(r, lease.Account)
	s.stageRequestMonitor(r, "image_getting_account", 35, map[string]any{"account_wait_ms": time.Since(accountStarted).Milliseconds()})
	s.stageRequestMonitor(r, "image_egress_ready", 40, map[string]any{"egress_acquire_ms": time.Since(accountStarted).Milliseconds()})
	generationStarted := time.Now()
	var images []provider.ImageResult
	if request.Model == "grok-imagine-image-lite" {
		images, err = s.mediaProvider.GenerateLite(r.Context(), lease.Account, request.Prompt, "fast", request.N)
	} else {
		aspect, _ := protocol.AspectRatio(request.Size)
		images, err = s.mediaProvider.GenerateImagine(r.Context(), lease.Account, request.Prompt, aspect, request.N, true, request.Model == "grok-imagine-image-pro", s.cfg.ImagineWSURL)
	}
	if err != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
		writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
		return
	}
	s.stageRequestMonitor(r, "image_generating", 70, map[string]any{"conversation_stream_ms": time.Since(generationStarted).Milliseconds(), "generation_start_ms": time.Since(generationStarted).Milliseconds()})
	if len(images) > request.N {
		images = images[:request.N]
	}
	data := make([]map[string]string, 0, len(images))
	for _, image := range images {
		resolveStarted := time.Now()
		value, resolveErr := s.mediaProvider.ResolveImage(r.Context(), lease.Account, image, request.ResponseFormat, s.cfg.ImageDataDir, requestPublicBase(r))
		if resolveErr != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(resolveErr), resolveErr)
			writeError(w, upstreamStatus(resolveErr), resolveErr.Error(), "upstream_error")
			return
		}
		s.stageRequestMonitor(r, "image_resolving", 85, map[string]any{"resolve_ms": time.Since(resolveStarted).Milliseconds()})
		s.stageRequestMonitor(r, "image_download_done", 95, map[string]any{"download_ms": time.Since(resolveStarted).Milliseconds()})
		s.recordGeneratedMedia(r.Context(), value)
		data = append(data, value)
	}
	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
	s.stageRequestMonitor(r, "image_single_done", 99, map[string]any{"total_ms": s.requestMonitorElapsed(r), "response_ms": s.requestMonitorElapsed(r)})
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func (s *Server) generateOpenAIImageData(r *http.Request, ctx context.Context, prompt, model, size, quality string, inputs []provider.OpenAIImageInput, responseFormat, publicBase string, count int) ([]map[string]string, error) {
	if count < 1 {
		count = 1
	}
	results := make([][]map[string]string, count)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = s.monitorOpenAIImageContext(r, ctx)
	var (
		wg    sync.WaitGroup
		errCh = make(chan error, 1)
	)
	sendErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}
	for index := 0; index < count; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			excluded := map[string]bool{}
			for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
				accountStarted := time.Now()
				s.stageRequestMonitor(r, "image_egress_waiting", 30, map[string]any{"egress_wait_ms": 0})
				lease, reserveErr := s.accountPool.ReserveMatching(ctx, []string{"basic", "super", "heavy"}, excluded, isOpenAIAccount)
				if reserveErr != nil {
					sendErr(reserveErr)
					cancel()
					return
				}
				s.enrichMonitorAccount(r, lease.Account)
				s.stageRequestMonitor(r, "image_getting_account", 35, map[string]any{"account_wait_ms": time.Since(accountStarted).Milliseconds()})
				s.stageRequestMonitor(r, "image_egress_ready", 40, map[string]any{"egress_acquire_ms": time.Since(accountStarted).Milliseconds()})

				generated, generateErr := s.openAIImage.Generate(ctx, lease.Account, prompt, model, size, quality, inputs)
				if generateErr != nil {
					s.accountPool.Release(lease)
					s.accountPool.Feedback(lease.Account, upstreamStatus(generateErr), generateErr)
					excluded[lease.Account.Token] = true
					if s.shouldRetry(upstreamStatus(generateErr), attempt) {
						continue
					}
					sendErr(generateErr)
					cancel()
					return
				}

				items := make([]map[string]string, 0, len(generated))
				var resolveErr error
				for _, image := range generated {
					value, localURL, err := s.openAIImage.Resolve(ctx, lease.Account, image, responseFormat, s.cfg.ImageDataDir, publicBase)
					if err != nil {
						resolveErr = err
						break
					}
					s.recordGeneratedMedia(ctx, map[string]string{"url": localURL})
					items = append(items, value)
					// Each worker represents exactly one requested output. Upstream can
					// expose that output through multiple references, so resolving the
					// remaining entries would only persist files that are later discarded.
					break
				}
				if resolveErr != nil {
					s.accountPool.Release(lease)
					s.accountPool.Feedback(lease.Account, upstreamStatus(resolveErr), resolveErr)
					excluded[lease.Account.Token] = true
					if s.shouldRetry(upstreamStatus(resolveErr), attempt) {
						continue
					}
					sendErr(resolveErr)
					cancel()
					return
				}
				s.accountPool.Release(lease)
				s.stageRequestMonitor(r, "image_response_ready", 95, map[string]any{"response_ms": time.Since(accountStarted).Milliseconds()})
				s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
				results[index] = items
				return
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		return nil, err
	default:
	}
	data := make([]map[string]string, 0)
	for _, items := range results {
		data = append(data, items...)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("OpenAI image generation returned no downloadable files")
	}
	if len(data) > count {
		data = data[:count]
	}
	return data, nil
}

func (s *Server) parseImageEditRequest(r *http.Request) (imageEditRequest, error) {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType == "application/json" {
		return s.parseJSONImageEditRequest(r)
	}
	if contentType != "multipart/form-data" && contentType != "" {
		return imageEditRequest{}, imageEditParseError{Message: "Content-Type must be multipart/form-data or application/json"}
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		message := "invalid multipart form"
		if strings.Contains(strings.ToLower(err.Error()), "boundary") {
			message = "invalid multipart form: missing boundary in Content-Type"
		}
		return imageEditRequest{}, imageEditParseError{Message: message}
	}
	values := r.MultipartForm.Value
	request := imageEditRequest{
		Model:          firstFormValue(values, "model"),
		Prompt:         strings.TrimSpace(firstFormValue(values, "prompt")),
		N:              positiveInt(firstFormValue(values, "n"), 1),
		Size:           firstFormValue(values, "size"),
		Quality:        firstFormValue(values, "quality"),
		ResponseFormat: firstFormValue(values, "response_format"),
	}
	request.HasMask = hasMultipartField(r.MultipartForm, imageEditMaskFields)
	for _, field := range imageEditReferenceFields {
		for _, header := range r.MultipartForm.File[field] {
			input, err := imageInputFromFileHeader(header)
			if err != nil {
				return imageEditRequest{}, err
			}
			request.Inputs = append(request.Inputs, input)
		}
		for _, value := range values[field] {
			inputs, err := s.imageInputsFromValue(r.Context(), value)
			if err != nil {
				return imageEditRequest{}, err
			}
			request.Inputs = append(request.Inputs, inputs...)
		}
	}
	return request, nil
}

func (s *Server) parseJSONImageEditRequest(r *http.Request) (imageEditRequest, error) {
	var body map[string]any
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxImageEditReferenceBytes+1<<20))
	if err := decoder.Decode(&body); err != nil {
		return imageEditRequest{}, imageEditParseError{Message: "invalid JSON body"}
	}
	request := imageEditRequest{
		Model:          stringValue(body["model"]),
		Prompt:         strings.TrimSpace(stringValue(body["prompt"])),
		N:              positiveInt(stringValue(body["n"]), 1),
		Size:           stringValue(body["size"]),
		Quality:        stringValue(body["quality"]),
		ResponseFormat: stringValue(body["response_format"]),
	}
	for _, field := range []string{"mask", "mask[]"} {
		if value, ok := body[field]; ok && value != nil && stringValue(value) != "" {
			request.HasMask = true
		}
	}
	for _, field := range imageEditReferenceFields {
		value, ok := body[field]
		if !ok {
			continue
		}
		inputs, err := s.imageInputsFromValue(r.Context(), value)
		if err != nil {
			return imageEditRequest{}, err
		}
		request.Inputs = append(request.Inputs, inputs...)
	}
	return request, nil
}

func firstFormValue(values map[string][]string, key string) string {
	for _, value := range values[key] {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func hasMultipartField(form *multipart.Form, fields []string) bool {
	if form == nil {
		return false
	}
	for _, field := range fields {
		if len(form.File[field]) > 0 {
			return true
		}
		for _, value := range form.Value[field] {
			if strings.TrimSpace(value) != "" {
				return true
			}
		}
	}
	return false
}

func imageInputFromFileHeader(header *multipart.FileHeader) (provider.OpenAIImageInput, error) {
	file, err := header.Open()
	if err != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: err.Error()}
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxImageEditReferenceBytes+1))
	_ = file.Close()
	if readErr != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "invalid image upload"}
	}
	if len(raw) == 0 {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image file is empty"}
	}
	if len(raw) > maxImageEditReferenceBytes {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image exceeds 50MB limit"}
	}
	mimeType := header.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = mime.TypeByExtension(filepath.Ext(header.Filename))
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return provider.OpenAIImageInput{Name: safeImageInputName(header.Filename, mimeType, "image.png"), MIME: mimeType, Data: raw}, nil
}

func (s *Server) imageInputsFromValue(ctx context.Context, value any) ([]provider.OpenAIImageInput, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return nil, nil
		}
		if (strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{")) && json.Valid([]byte(text)) {
			var nested any
			if json.Unmarshal([]byte(text), &nested) == nil {
				return s.imageInputsFromValue(ctx, nested)
			}
		}
		input, err := s.imageInputFromString(ctx, text)
		if err != nil {
			return nil, err
		}
		return []provider.OpenAIImageInput{input}, nil
	case []any:
		result := []provider.OpenAIImageInput{}
		for _, item := range typed {
			inputs, err := s.imageInputsFromValue(ctx, item)
			if err != nil {
				return nil, err
			}
			result = append(result, inputs...)
		}
		return result, nil
	case map[string]any:
		if typed["file_id"] != nil && stringValue(typed["file_id"]) != "" {
			return nil, imageEditParseError{Message: "file_id image references are not supported; use image_url instead"}
		}
		if inline := firstNonEmpty(stringValue(typed["b64_json"]), stringValue(typed["base64"])); inline != "" {
			filename := firstNonEmpty(stringValue(typed["filename"]), stringValue(typed["file_name"]), "image.png")
			mimeType := firstNonEmpty(stringValue(typed["mime_type"]), stringValue(typed["mimeType"]), "image/png")
			input, err := decodeBase64Image(inline, filename, mimeType)
			if err != nil {
				return nil, err
			}
			return []provider.OpenAIImageInput{input}, nil
		}
		imageURL := typed["image_url"]
		if imageURL == nil {
			imageURL = typed["url"]
		}
		if nested, ok := imageURL.(map[string]any); ok {
			imageURL = nested["url"]
		}
		return s.imageInputsFromValue(ctx, imageURL)
	default:
		return nil, imageEditParseError{Message: "invalid image reference"}
	}
}

func (s *Server) imageInputFromString(ctx context.Context, value string) (provider.OpenAIImageInput, error) {
	lower := strings.ToLower(value)
	switch {
	case strings.HasPrefix(lower, "data:"):
		return decodeDataImageURL(value)
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return s.downloadImageInput(ctx, value)
	default:
		return decodeBase64Image(value, "image.png", "image/png")
	}
}

func decodeDataImageURL(value string) (provider.OpenAIImageInput, error) {
	header, payload, ok := strings.Cut(value, ",")
	if !ok {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "invalid data image URL"}
	}
	mimeType := strings.TrimPrefix(strings.Split(header, ";")[0], "data:")
	if mimeType == "" {
		mimeType = "image/png"
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url must point to an image"}
	}
	var raw []byte
	var err error
	if strings.Contains(strings.ToLower(header), ";base64") {
		raw, err = base64.StdEncoding.DecodeString(payload)
	} else {
		var text string
		text, err = url.PathUnescape(payload)
		raw = []byte(text)
	}
	if err != nil || len(raw) == 0 {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "invalid data image URL"}
	}
	if len(raw) > maxImageEditReferenceBytes {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image URL exceeds 50MB limit"}
	}
	return provider.OpenAIImageInput{Name: "image_url" + imageEditExtensionForMIME(mimeType), MIME: mimeType, Data: raw}, nil
}

func decodeBase64Image(value, filename, mimeType string) (provider.OpenAIImageInput, error) {
	clean := strings.TrimSpace(value)
	clean = strings.ReplaceAll(clean, "\r", "")
	clean = strings.ReplaceAll(clean, "\n", "")
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(clean, "="))
	}
	if err != nil || len(raw) == 0 {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "invalid base64 image data"}
	}
	if len(raw) > maxImageEditReferenceBytes {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image exceeds 50MB limit"}
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "invalid base64 image data"}
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return provider.OpenAIImageInput{Name: safeImageInputName(filename, mimeType, "image.png"), MIME: mimeType, Data: raw}, nil
}

func (s *Server) downloadImageInput(ctx context.Context, source string) (provider.OpenAIImageInput, error) {
	if err := validateRemoteImageURL(ctx, source); err != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: err.Error()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url must be an http or https URL"}
	}
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	request.Header.Set("User-Agent", "chatgpt2api image fetcher")
	client := s.requestClient
	if client == nil {
		client = http.DefaultClient
	}
	guardedClient := *client
	originalRedirect := guardedClient.CheckRedirect
	guardedClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return fmt.Errorf("image_url has too many redirects")
		}
		if err := validateRemoteImageURL(req.Context(), req.URL.String()); err != nil {
			return err
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	response, err := guardedClient.Do(request)
	if err != nil {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url fetch failed: " + err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: fmt.Sprintf("image_url fetch failed: HTTP %d", response.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxImageEditReferenceBytes+1))
	if err != nil || len(raw) == 0 {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url returned empty content"}
	}
	if len(raw) > maxImageEditReferenceBytes {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url exceeds 50MB limit"}
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	parsed, _ := url.Parse(source)
	if mimeType == "" || mimeType == "application/octet-stream" || mimeType == "binary/octet-stream" {
		mimeType = mime.TypeByExtension(filepath.Ext(parsed.Path))
		if mimeType == "" {
			mimeType = "image/png"
		}
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return provider.OpenAIImageInput{}, imageEditParseError{Message: "image_url must point to an image"}
	}
	return provider.OpenAIImageInput{Name: safeImageInputName(filepath.Base(parsed.Path), mimeType, "image_url.png"), MIME: mimeType, Data: raw}, nil
}

func validateRemoteImageURL(ctx context.Context, source string) error {
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return fmt.Errorf("image_url must be an http or https URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("image_url must not include credentials")
	}
	if allowPrivateImageURLs() {
		return nil
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") {
		return fmt.Errorf("image_url points to a private network address")
	}
	addresses := []net.IP{}
	if literal := net.ParseIP(hostname); literal != nil {
		addresses = append(addresses, literal)
	} else {
		resolved, resolveErr := net.DefaultResolver.LookupIPAddr(ctx, hostname)
		if resolveErr != nil || len(resolved) == 0 {
			return fmt.Errorf("image_url hostname could not be resolved")
		}
		for _, item := range resolved {
			addresses = append(addresses, item.IP)
		}
	}
	for _, address := range addresses {
		if !isPublicImageAddress(address) {
			return fmt.Errorf("image_url points to a private network address")
		}
	}
	return nil
}

func allowPrivateImageURLs() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GO_ALLOW_PRIVATE_IMAGE_URLS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func isPublicImageAddress(address net.IP) bool {
	if address == nil || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() || address.IsMulticast() {
		return false
	}
	_, carrierGradeNAT, _ := net.ParseCIDR("100.64.0.0/10")
	return !carrierGradeNAT.Contains(address)
}

func safeImageInputName(name, mimeType, fallback string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = fallback
	}
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, name)
	name = strings.Trim(name, "._")
	if name == "" {
		name = fallback
	}
	if filepath.Ext(name) == "" {
		name += imageEditExtensionForMIME(mimeType)
	}
	return name
}

func imageEditExtensionForMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".png"
	}
}

func (s *Server) imageEdits(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": s.requestMonitorElapsed(r)})
	request, err := s.parseImageEditRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	modelName := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if modelName == "" {
		modelName = "grok-imagine-image-edit"
	}
	s.enrichRequestMonitor(r, map[string]any{"model": modelName})
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt cannot be empty", "invalid_request_error")
		return
	}
	n := request.N
	if n == 0 {
		n = 1
	}
	if n > 2 {
		writeError(w, http.StatusBadRequest, "n must be between 1 and 2", "invalid_request_error")
		return
	}
	if !isOpenAIImageModel(modelName) && strings.TrimSpace(request.Size) != "" && strings.TrimSpace(request.Size) != "1024x1024" {
		writeError(w, http.StatusBadRequest, "image edit only supports size 1024x1024", "invalid_request_error")
		return
	}
	if _, ok := model.Find(s.catalog, modelName); !ok || (modelName != "grok-imagine-image-edit" && !isOpenAIImageModel(modelName)) {
		writeError(w, http.StatusBadRequest, "model is not an image-edit model", "invalid_request_error")
		return
	}
	if request.HasMask {
		writeError(w, http.StatusBadRequest, "mask is not supported yet", "invalid_request_error")
		return
	}
	inputs := request.Inputs
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "at least one image is required", "invalid_request_error")
		return
	}
	if len(inputs) > 7 {
		inputs = inputs[len(inputs)-7:]
	}
	if isOpenAIImageModel(modelName) {
		size := strings.TrimSpace(request.Size)
		if size == "" {
			size = "1024x1024"
		}
		format := imageEditResponseFormat(request.ResponseFormat)
		data, err := s.generateOpenAIImageData(r, r.Context(), prompt, modelName, size, request.Quality, inputs, format, requestPublicBase(r), n)
		if err != nil {
			writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
		return
	}
	lease, err := s.accountPool.Reserve(r.Context(), []string{"super", "heavy"}, nil)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error(), "rate_limit_error")
		return
	}
	defer s.accountPool.Release(lease)
	refs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		fileID, fileURI, uploadErr := s.mediaProvider.Upload(r.Context(), lease.Account, input.Name, input.MIME, base64.StdEncoding.EncodeToString(input.Data))
		if uploadErr != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(uploadErr), uploadErr)
			writeError(w, upstreamStatus(uploadErr), uploadErr.Error(), "upstream_error")
			return
		}
		ref := protocol.ResolveAssetReference(fileID, fileURI, cookieUserID(lease.Account))
		if ref == "" {
			writeError(w, http.StatusBadGateway, "uploaded image has no resolvable asset URL", "upstream_error")
			return
		}
		refs = append(refs, ref)
	}
	post, err := s.mediaProvider.CreatePost(r.Context(), lease.Account, protocol.ImagePostMediaType, "", prompt)
	if err != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
		writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
		return
	}
	postObject, _ := post["post"].(map[string]any)
	parentID := stringValue(postObject["id"])
	if parentID == "" {
		writeError(w, http.StatusBadGateway, "image edit create-post returned no post id", "upstream_error")
		return
	}
	response, err := s.mediaProvider.StreamChat(r.Context(), lease.Account, protocol.BuildImageEditPayload(prompt, refs, parentID))
	if err != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
		writeError(w, upstreamStatus(err), err.Error(), "upstream_error")
		return
	}
	defer response.Body.Close()
	images := collectImageEvents(response.Body)
	if len(images) == 0 {
		writeError(w, http.StatusBadGateway, "image edit returned no images", "upstream_error")
		return
	}
	data := make([]map[string]string, 0, minInt(n, len(images)))
	format := imageEditResponseFormat(request.ResponseFormat)
	for _, image := range images[:minInt(n, len(images))] {
		value, resolveErr := s.mediaProvider.ResolveImage(r.Context(), lease.Account, image, format, s.cfg.ImageDataDir, requestPublicBase(r))
		if resolveErr != nil {
			writeError(w, upstreamStatus(resolveErr), resolveErr.Error(), "upstream_error")
			return
		}
		s.recordGeneratedMedia(r.Context(), value)
		data = append(data, value)
	}
	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

func imageEditResponseFormat(string) string {
	return "url"
}

func (s *Server) imageFile(w http.ResponseWriter, r *http.Request) {
	s.serveMediaFile(w, r, s.cfg.ImageDataDir, "image")
}

func (s *Server) monitorOpenAIImageContext(r *http.Request, ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	ctx = provider.WithOpenAIImageStage(ctx, func(metric string, elapsed time.Duration) {
		stages := map[string]struct {
			name     string
			progress int
		}{
			"upload_ms":               {"image_uploading", 25},
			"bootstrap_ms":            {"image_bootstrapping", 43},
			"requirements_ms":         {"image_getting_token", 45},
			"prepare_conversation_ms": {"image_preparing_conversation", 50},
			"generation_start_ms":     {"image_starting_generation", 60},
			"conversation_stream_ms":  {"image_generating", 70},
			"resolve_ms":              {"image_resolving", 85},
			"download_ms":             {"image_download_done", 95},
			"total_ms":                {"image_single_done", 99},
		}
		stage, ok := stages[metric]
		if !ok {
			return
		}
		s.stageRequestMonitor(r, stage.name, stage.progress, map[string]any{metric: elapsed.Milliseconds()})
	})
	return provider.WithOpenAIImageEgress(ctx, func(proxyURL string) {
		label := sanitizedEgressLabel(proxyURL)
		meta := map[string]any{"has_proxy": label != ""}
		if label == "" {
			meta["proxy_source"] = "direct"
			meta["egress_label"] = "direct"
		} else {
			meta["egress_label"] = label
		}
		s.enrichRequestMonitor(r, meta)
	})
}

func sanitizedEgressLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	if parsed.Port() != "" {
		return parsed.Scheme + "://" + parsed.Hostname() + ":" + parsed.Port()
	}
	return parsed.Scheme + "://" + parsed.Hostname()
}

func (s *Server) videoFile(w http.ResponseWriter, r *http.Request) {
	s.serveMediaFile(w, r, s.cfg.VideoDataDir, "video")
}

func (s *Server) serveMediaFile(w http.ResponseWriter, r *http.Request, root, kind string) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if !protocol.ValidMediaFileID(id) {
		writeError(w, http.StatusBadRequest, "invalid file ID", "invalid_request_error")
		return
	}
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), id+".") {
			path := filepath.Join(root, entry.Name())
			if !isWithin(root, path) {
				continue
			}
			if kind == "video" {
				w.Header().Set("Content-Type", "video/mp4")
			} else {
				w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(path)))
			}
			http.ServeFile(w, r, path)
			return
		}
	}
	writeError(w, http.StatusNotFound, kind+" file not found", "not_found")
}

func mediaPools(modelName string) []string {
	if modelName == "grok-imagine-image-lite" {
		return []string{"basic", "super", "heavy"}
	}
	return []string{"super", "heavy"}
}

func isOpenAIImageModel(modelName string) bool {
	return strings.EqualFold(strings.TrimSpace(modelName), "gpt-image-2")
}

func validOpenAIImageSize(size string) bool {
	value := strings.TrimSpace(strings.ToLower(size))
	if value == "" || value == "auto" {
		return true
	}
	parts := strings.Split(value, "x")
	if len(parts) != 2 {
		return false
	}
	w, e1 := strconv.Atoi(parts[0])
	h, e2 := strconv.Atoi(parts[1])
	return e1 == nil && e2 == nil && w > 0 && h > 0
}

func isOpenAIAccount(account accounts.Account) bool {
	source := strings.ToLower(strings.TrimSpace(stringValue(account.Fields["source_type"])))
	switch source {
	case "chatgpt_web", "oauth_login", "openai", "codex", "openai_oauth":
		return true
	}
	// Older imports did not persist source_type. ChatGPT access tokens are JWTs;
	// Grok SSO tokens are opaque values.
	return strings.Count(strings.TrimSpace(account.Token), ".") == 2
}

func collectImageEvents(reader io.Reader) []provider.ImageResult {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	results := make([]provider.ImageResult, 0, 2)
	seen := map[string]bool{}
	for scanner.Scan() {
		kind, events := protocol.ParseSSELine(scanner.Text())
		if kind == "done" {
			break
		}
		for _, event := range events {
			if event.Kind != "image" || event.Progress < 100 || event.Moderated {
				continue
			}
			value := event.URL
			if value == "" && event.AssetID != "" {
				value = protocol.ResolveAssetReference(event.AssetID, "", "")
			}
			if value != "" && !seen[value] {
				seen[value] = true
				results = append(results, provider.ImageResult{URL: value})
			}
		}
	}
	return results
}

func requestPublicBase(r *http.Request) string {
	for _, key := range []string{"GO_PUBLIC_BASE_URL", "CHATGPT2API_BASE_URL"} {
		if value := normalizePublicBaseURL(os.Getenv(key)); value != "" {
			return value
		}
	}
	trustRequestHeaders := trustRequestPublicBase()
	if trustRequestHeaders {
		if value := normalizePublicBaseURL(r.Header.Get("X-Public-Base-URL")); value != "" {
			return value
		}
	}
	host := ""
	if trustRequestHeaders {
		host = firstHeaderValue(r, "X-Forwarded-Host", "X-Host")
	}
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host != "" {
		scheme := ""
		if trustRequestHeaders {
			scheme = firstHeaderValue(r, "X-Forwarded-Proto", "X-Scheme")
		}
		if scheme == "" {
			scheme = "http"
			if r.TLS != nil {
				scheme = "https"
			}
		}
		prefix := ""
		if trustRequestHeaders {
			prefix = cleanForwardedPrefix(firstHeaderValue(r, "X-Forwarded-Prefix", "X-Script-Name"))
		}
		candidate := normalizePublicBaseURL(scheme + "://" + host + prefix)
		if candidate == "" {
			return ""
		}
		parsed, _ := url.Parse(candidate)
		if trustRequestHeaders || isLocalPublicHostname(parsed.Hostname()) {
			return candidate
		}
	}
	return ""
}

func normalizePublicBaseURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.TrimRight(parsed.String(), "/")
}

func trustRequestPublicBase() bool {
	for _, key := range []string{"GO_TRUST_REQUEST_PUBLIC_BASE", "CHATGPT2API_TRUST_REQUEST_HOST"} {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return false
}

func isLocalPublicHostname(hostname string) bool {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return true
	}
	if address := net.ParseIP(hostname); address != nil {
		return !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast()
	}
	return false
}

func firstHeaderValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		value := strings.TrimSpace(r.Header.Get(key))
		if value == "" {
			continue
		}
		if strings.Contains(value, ",") {
			value = strings.TrimSpace(strings.Split(value, ",")[0])
		}
		return value
	}
	return ""
}

func cleanForwardedPrefix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value == "/" {
		return ""
	}
	if strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return strings.TrimRight(value, "/")
}

func cookieUserID(account accounts.Account) string {
	cookie := stringValue(account.Fields["cookie_header"])
	if cookie == "" {
		return ""
	}
	match := cookieUserIDPattern.FindStringSubmatch(cookie)
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

func upstreamStatus(err error) int {
	var upstream *protocol.UpstreamError
	if errors.As(err, &upstream) && upstream.Status >= 400 {
		return upstream.Status
	}
	return http.StatusBadGateway
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
