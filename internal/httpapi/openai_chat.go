package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) completeOpenAIChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	message := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(message) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	responseID := newChatID()
	excluded := map[string]bool{}
	var text, thinking string
	var lastErr error
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		lease, err := s.accountPool.ReserveMatching(r.Context(), route.PoolCandidates, excluded, isOpenAIAccount)
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		text, thinking, err = s.openAIChat.Complete(r.Context(), lease.Account, request)
		s.accountPool.Release(lease)
		if err != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
			excluded[lease.Account.Token] = true
			lastErr = err
			if attempt < s.cfg.ChatMaxRetries {
				continue
			}
			break
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		writeOpenAIChatError(w, lastErr)
		return
	}
	messagePayload := map[string]any{"role": "assistant", "content": text}
	if thinking != "" && request.ReasoningEffort != "none" {
		messagePayload["reasoning_content"] = thinking
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": responseID, "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": messagePayload, "finish_reason": "stop"}},
		"usage":   usageFor(message, text, thinking),
	})
}

func (s *Server) streamOpenAIChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
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
		lease, err := s.accountPool.ReserveMatching(r.Context(), route.PoolCandidates, excluded, isOpenAIAccount)
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		err = s.openAIChat.Stream(r.Context(), lease.Account, request, func(event provider.OpenAIChatEvent) error {
			if event.Text == "" && event.Thinking == "" {
				return nil
			}
			if !emitted {
				writeSSE(w, map[string]any{"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}}})
				emitted = true
			}
			delta := map[string]any{}
			if event.Text != "" {
				delta["content"] = event.Text
			}
			if event.Thinking != "" && request.ReasoningEffort != "none" {
				delta["reasoning_content"] = event.Thinking
			}
			writeSSE(w, map[string]any{"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": delta}}})
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		})
		s.accountPool.Release(lease)
		if err != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
			excluded[lease.Account.Token] = true
			lastErr = err
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
		writeSSE(w, map[string]any{"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}}})
	}
	writeSSE(w, map[string]any{"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (s *Server) completeOpenAIImageChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	started := time.Now()
	imageContext := s.monitorOpenAIImageContext(r, r.Context())
	s.enrichRequestMonitor(r, map[string]any{"model": request.Model})
	prompt := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(prompt) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	excluded := map[string]bool{}
	var images []provider.ImageResult
	var selected accounts.Account
	var lastErr error
	inputs := make([]provider.OpenAIImageInput, 0)
	for _, message := range request.Messages {
		parsed, err := s.imageInputsFromChatContent(r.Context(), message.Content)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		inputs = append(inputs, parsed...)
	}
	s.stageRequestMonitor(r, "handler_queue_done", 10, map[string]any{"handler_queue_ms": time.Since(started).Milliseconds()})
	s.stageRequestMonitor(r, "image_uploading", 25, map[string]any{"upload_ms": time.Since(started).Milliseconds()})
	size := strings.TrimSpace(request.Size)
	if size == "" {
		size = "1024x1024"
	}
	s.enrichRequestMonitor(r, map[string]any{"size": size, "image_url_parts": len(inputs), "data_url_images": len(inputs)})
	s.stageRequestMonitor(r, "image_egress_waiting", 30, map[string]any{"egress_wait_ms": 0})
	s.stageRequestMonitor(r, "image_getting_account", 35, nil)
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		lease, err := s.accountPool.ReserveMatching(r.Context(), []string{"basic", "super", "heavy"}, excluded, isOpenAIAccount)
		if err != nil {
			lastErr = err
			break
		}
		s.enrichMonitorAccount(r, lease.Account)
		s.stageRequestMonitor(r, "image_egress_ready", 40, map[string]any{"egress_acquire_ms": time.Since(started).Milliseconds()})
		s.stageRequestMonitor(r, "image_bootstrapping", 43, map[string]any{"bootstrap_ms": time.Since(started).Milliseconds()})
		s.stageRequestMonitor(r, "image_preparing_conversation", 46, map[string]any{"prepare_conversation_ms": time.Since(started).Milliseconds()})
		s.stageRequestMonitor(r, "image_starting_generation", 50, map[string]any{"account_wait_ms": time.Since(started).Milliseconds()})
		s.stageRequestMonitor(r, "image_generating", 60, map[string]any{"generation_start_ms": time.Since(started).Milliseconds()})
		images, err = s.openAIImage.Generate(imageContext, lease.Account, prompt, request.Model, size, "auto", inputs)
		selected = lease.Account
		s.accountPool.Release(lease)
		if err != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
			excluded[lease.Account.Token] = true
			lastErr = err
			if s.shouldRetry(upstreamStatus(err), attempt) {
				continue
			}
			break
		}
		s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
		lastErr = nil
		break
	}
	s.stageRequestMonitor(r, "image_stream_resolve_start", 80, map[string]any{"stream_resolve_ms": time.Since(started).Milliseconds()})
	s.stageRequestMonitor(r, "image_resolving", 85, map[string]any{"generation_ms": time.Since(started).Milliseconds(), "resolve_ms": time.Since(started).Milliseconds()})
	if lastErr != nil {
		writeOpenAIChatError(w, lastErr)
		return
	}
	parts := make([]string, 0, len(images))
	for _, image := range images {
		item, resolveErr := s.openAIImage.Resolve(r.Context(), selected, image, "url", s.cfg.ImageDataDir, requestPublicBase(r))
		if resolveErr != nil {
			s.accountPool.Feedback(selected, upstreamStatus(resolveErr), resolveErr)
			writeError(w, upstreamStatus(resolveErr), resolveErr.Error(), "upstream_error")
			return
		}
		s.recordGeneratedMedia(r.Context(), item)
		parts = append(parts, fmt.Sprintf("![image](%s)", item["url"]))
	}
	s.stageRequestMonitor(r, "image_download_done", 95, map[string]any{"total_ms": time.Since(started).Milliseconds()})
	s.accountPool.Feedback(selected, http.StatusOK, nil)
	content := strings.Join(parts, "\n")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": newChatID(), "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   usageFor(prompt, content, ""),
	})
}

// Chat content mixes prompt text and image blocks. Only image-bearing blocks
// should be decoded; a plain prompt such as "hi" is not image data.
func (s *Server) imageInputsFromChatContent(ctx context.Context, value any) ([]provider.OpenAIImageInput, error) {
	switch typed := value.(type) {
	case nil, string:
		return nil, nil
	case []any:
		inputs := make([]provider.OpenAIImageInput, 0)
		for _, item := range typed {
			object, ok := item.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(strings.TrimSpace(stringValue(object["type"])))
			if kind != "image_url" && kind != "input_image" && kind != "image" {
				continue
			}
			parsed, err := s.imageInputsFromValue(ctx, object)
			if err != nil {
				return nil, err
			}
			inputs = append(inputs, parsed...)
		}
		return inputs, nil
	case map[string]any:
		kind := strings.ToLower(strings.TrimSpace(stringValue(typed["type"])))
		if kind == "image_url" || kind == "input_image" || kind == "image" {
			return s.imageInputsFromValue(ctx, typed)
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *Server) streamOpenAIImageChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	recorder := &responseCapture{header: make(http.Header)}
	request.Stream = false
	s.completeOpenAIImageChat(recorder, r, request)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &response); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal image response", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": responseContent(response)}}}})
	writeSSE(w, map[string]any{"id": response["id"], "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func responseContent(response map[string]any) string {
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	return stringValue(message["content"])
}

func writeOpenAIChatError(w http.ResponseWriter, err error) {
	status := upstreamStatus(err)
	if errors.Is(err, accounts.ErrUnavailable) {
		status = http.StatusTooManyRequests
	}
	writeError(w, status, err.Error(), "upstream_error")
}
