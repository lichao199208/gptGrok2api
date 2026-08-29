package httpapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) consoleChatCompletions(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	if len(request.Tools) > 0 || request.ToolChoice != nil {
		writeError(w, http.StatusBadRequest, "Console models do not support user-defined function tools yet", "invalid_request_error")
		return
	}
	if request.Stream {
		s.streamConsoleChat(w, r, request)
		return
	}
	s.completeConsoleChat(w, r, request)
}

func (s *Server) completeConsoleChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	message := protocol.ExtractMessage(request.Messages)
	if strings.TrimSpace(message) == "" {
		writeError(w, http.StatusBadRequest, "messages contain no text", "invalid_request_error")
		return
	}
	responseID := newChatID()
	excluded := map[string]bool{}
	var text string
	var usage map[string]any
	var lastErr error
	for attempt := 0; attempt <= s.cfg.ChatMaxRetries; attempt++ {
		lease, err := s.accountPool.Reserve(r.Context(), []string{"basic"}, excluded)
		if err != nil {
			lastErr = err
			break
		}
		payload, err := protocol.BuildConsolePayload(
			request.Messages, request.Model, request.ReasoningEffort,
			request.Temperature, request.TopP, request.MaxTokens, true,
		)
		if err != nil {
			s.accountPool.Release(lease)
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
			return
		}
		response, err := s.consoleProvider.Do(r.Context(), lease.Account, payload)
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
			upstreamErr := provider.ReadConsoleError(response)
			s.accountPool.Release(lease)
			s.accountPool.Feedback(lease.Account, upstreamErr.Status, upstreamErr)
			excluded[lease.Account.Token] = true
			lastErr = upstreamErr
			if s.shouldRetry(upstreamErr.Status, attempt) {
				continue
			}
			break
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		scanErr := protocol.ScanConsole(scanner, func(event protocol.ConsoleEvent) error {
			if event.Err != nil {
				return event.Err
			}
			text += event.Delta
			if event.Usage != nil {
				usage = event.Usage
			}
			return nil
		})
		response.Body.Close()
		s.accountPool.Release(lease)
		if scanErr != nil {
			s.accountPool.Feedback(lease.Account, http.StatusBadGateway, scanErr)
			excluded[lease.Account.Token] = true
			lastErr = scanErr
			text, usage = "", nil
			if attempt < s.cfg.ChatMaxRetries {
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
	if usage == nil {
		usage = usageFor(message, text, "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": responseID, "object": "chat.completion", "created": time.Now().Unix(), "model": request.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usage,
	})
}

func (s *Server) streamConsoleChat(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
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
		lease, err := s.accountPool.Reserve(r.Context(), []string{"basic"}, excluded)
		if err != nil {
			lastErr = err
			break
		}
		payload, err := protocol.BuildConsolePayload(
			request.Messages, request.Model, request.ReasoningEffort,
			request.Temperature, request.TopP, request.MaxTokens, true,
		)
		if err != nil {
			s.accountPool.Release(lease)
			writeSSE(w, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}})
			return
		}
		response, err := s.consoleProvider.Do(r.Context(), lease.Account, payload)
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
			upstreamErr := provider.ReadConsoleError(response)
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
		scanErr := protocol.ScanConsole(scanner, func(event protocol.ConsoleEvent) error {
			if event.Err != nil {
				return event.Err
			}
			if event.Delta == "" {
				return nil
			}
			if !emitted {
				writeSSE(w, map[string]any{
					"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}},
				})
				emitted = true
			}
			writeSSE(w, map[string]any{
				"id": responseID, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": request.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": event.Delta}}},
			})
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
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}},
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

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var request protocol.ResponsesRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	route, ok := model.ResolveChat(request.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "model not found", "invalid_request_error")
		return
	}
	if !route.Console {
		messages := protocol.ResponsesInputMessages(request.Input, request.Instructions)
		if len(messages) == 0 {
			writeError(w, http.StatusBadRequest, "input cannot be empty", "invalid_request_error")
			return
		}
		chatRequest := protocol.ChatRequest{
			Model: request.Model, Messages: messages, Stream: false,
			Temperature: request.Temperature, TopP: request.TopP,
			MaxTokens: request.MaxOutputTokens, Tools: request.Tools,
			ToolChoice: request.ToolChoice,
		}
		chatRequest.ReasoningEffort = stringValue(request.Reasoning["effort"])
		if request.Stream {
			s.streamWebResponses(w, r, chatRequest, route)
		} else {
			s.completeWebResponses(w, r, chatRequest, route)
		}
		return
	}
	if len(request.Tools) > 0 || request.ToolChoice != nil {
		writeError(w, http.StatusBadRequest, "Console models do not support user-defined function tools yet", "invalid_request_error")
		return
	}
	messages := protocol.ResponsesInputMessages(request.Input, request.Instructions)
	if len(messages) == 0 {
		writeError(w, http.StatusBadRequest, "input cannot be empty", "invalid_request_error")
		return
	}
	chatRequest := protocol.ChatRequest{
		Model: request.Model, Messages: messages, Stream: request.Stream,
		Temperature: request.Temperature, TopP: request.TopP,
		MaxTokens: request.MaxOutputTokens,
	}
	chatRequest.ReasoningEffort = stringValue(request.Reasoning["effort"])
	if request.Stream {
		s.streamConsoleResponses(w, r, chatRequest)
		return
	}
	s.completeConsoleResponses(w, r, chatRequest)
}

func (s *Server) completeWebResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	recorder := &responseCapture{header: make(http.Header)}
	s.completeChat(recorder, r, request, route)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &chat); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	writeJSON(w, http.StatusOK, responseFromChat(chat, request.Model))
}

func (s *Server) streamWebResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest, route model.ChatRoute) {
	recorder := &responseCapture{header: make(http.Header)}
	s.completeChat(recorder, r, request, route)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if err := json.Unmarshal(recorder.body.Bytes(), &chat); err != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	response := responseFromChat(chat, request.Model)
	responseID := stringValue(response["id"])
	writeEventSSE(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	writeEventSSE(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	output := response["output"]
	writeEventSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": output})
	if items, ok := output.([]any); ok && len(items) > 0 {
		if item, ok := items[0].(map[string]any); ok {
			if content, ok := item["content"].([]any); ok && len(content) > 0 {
				if part, ok := content[0].(map[string]any); ok {
					writeEventSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": part["text"]})
				}
				writeEventSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			}
		}
	}
	response["status"] = "completed"
	writeEventSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": response})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func responseFromChat(chat map[string]any, modelName string) map[string]any {
	responseID := "resp_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-")
	outputID := "msg_" + strings.TrimPrefix(stringValue(chat["id"]), "chatcmpl-")
	content := ""
	output := []any{}
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, raw := range calls {
						call, _ := raw.(map[string]any)
						fn, _ := call["function"].(map[string]any)
						output = append(output, map[string]any{"id": call["id"], "type": "function_call", "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
					}
				} else {
					content = stringValue(message["content"])
				}
			}
		}
	}
	if len(output) == 0 {
		output = []any{map[string]any{"id": outputID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}}}}
	}
	return map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": modelName, "output": output, "usage": chat["usage"]}
}

func (s *Server) completeConsoleResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	// Aggregate the same upstream stream as Chat Completions, then wrap it in
	// the Responses API object shape.
	recorder := &responseCapture{header: make(http.Header)}
	s.completeConsoleChat(recorder, r, request)
	if recorder.status >= 400 {
		w.WriteHeader(recorder.status)
		_, _ = w.Write(recorder.body.Bytes())
		return
	}
	var chat map[string]any
	if json.Unmarshal(recorder.body.Bytes(), &chat) != nil {
		writeError(w, http.StatusBadGateway, "invalid internal response", "server_error")
		return
	}
	content := ""
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				content = stringValue(message["content"])
			}
		}
	}
	responseID := newChatID()
	outputID := "msg_" + strings.TrimPrefix(responseID, "chatcmpl-")
	writeJSON(w, http.StatusOK, map[string]any{
		"id": responseID, "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": request.Model,
		"output": []any{map[string]any{
			"id": outputID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": content, "annotations": []any{}}},
		}},
		"usage": chat["usage"],
	})
}

func (s *Server) streamConsoleResponses(w http.ResponseWriter, r *http.Request, request protocol.ChatRequest) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	responseID := "resp_" + strings.TrimPrefix(newChatID(), "chatcmpl-")
	messageID := "msg_" + strings.TrimPrefix(responseID, "resp_")
	writeEventSSE(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	writeEventSSE(w, "response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": request.Model}})
	writeEventSSE(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	writeEventSSE(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	if flusher != nil {
		flusher.Flush()
	}

	lease, err := s.accountPool.Reserve(r.Context(), []string{"basic"}, nil)
	if err != nil {
		writeEventSSE(w, "error", map[string]any{"type": "error", "message": err.Error()})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	payload, err := protocol.BuildConsolePayload(request.Messages, request.Model, request.ReasoningEffort, request.Temperature, request.TopP, request.MaxTokens, true)
	if err != nil {
		s.accountPool.Release(lease)
		writeEventSSE(w, "error", map[string]any{"type": "error", "message": err.Error()})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	response, err := s.consoleProvider.Do(r.Context(), lease.Account, payload)
	if err != nil {
		s.accountPool.Release(lease)
		writeEventSSE(w, "error", map[string]any{"type": "error", "message": err.Error()})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		upstreamErr := provider.ReadConsoleError(response)
		s.accountPool.Release(lease)
		s.accountPool.Feedback(lease.Account, upstreamErr.Status, upstreamErr)
		writeEventSSE(w, "error", map[string]any{"type": "error", "message": upstreamErr.Error()})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	var full strings.Builder
	var usage map[string]any
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	scanErr := protocol.ScanConsole(scanner, func(event protocol.ConsoleEvent) error {
		if event.Err != nil {
			return event.Err
		}
		if event.Delta != "" {
			full.WriteString(event.Delta)
			writeEventSSE(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": messageID, "output_index": 0, "content_index": 0, "delta": event.Delta})
			if flusher != nil {
				flusher.Flush()
			}
		}
		if event.Usage != nil {
			usage = event.Usage
		}
		return nil
	})
	response.Body.Close()
	s.accountPool.Release(lease)
	if scanErr != nil {
		s.accountPool.Feedback(lease.Account, http.StatusBadGateway, scanErr)
		writeEventSSE(w, "error", map[string]any{"type": "error", "message": scanErr.Error()})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return
	}
	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
	content := full.String()
	writeEventSSE(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": messageID, "output_index": 0, "content_index": 0, "text": content})
	writeEventSSE(w, "response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": messageID, "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": content, "annotations": []any{}}})
	writeEventSSE(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content}}}})
	if usage == nil {
		usage = usageFor(protocol.ExtractMessage(request.Messages), content, "")
	}
	writeEventSSE(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "object": "response", "status": "completed", "model": request.Model, "output": []any{map[string]any{"id": messageID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content}}}}, "usage": usage}})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *responseCapture) Header() http.Header    { return r.header }
func (r *responseCapture) WriteHeader(status int) { r.status = status }
func (r *responseCapture) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

func writeEventSSE(w http.ResponseWriter, event string, value any) {
	raw, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
}
