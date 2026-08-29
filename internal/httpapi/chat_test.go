package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func chatTestConfig(root, upstream string) config.Config {
	return config.Config{
		RootDir: root, DataDir: filepath.Join(root, "data"),
		StaticDir:    filepath.Join(root, "web_dist"),
		ConfigPath:   filepath.Join(root, "config.json"),
		AccountsPath: filepath.Join(root, "data", "accounts.json"),
		AuthKeysPath: filepath.Join(root, "data", "auth_keys.json"),
		APIKey:       "api-secret", AdminKey: "admin-secret",
		GrokChatURL: upstream, ChatMaxRetries: 2,
		ChatRetryCodes: map[int]bool{401: true, 429: true, 500: true, 502: true, 503: true},
	}
}

func TestChatCompletionJSONAndSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "sso=chat-token") {
			t.Errorf("missing SSO cookie: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"hello ","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"world","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := chatTestConfig(root, upstream.URL)
	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "chat-token", "source_type": "grok_sso", "type": "grok"}}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"content":"hello world"`) {
		t.Fatalf("unexpected JSON response: %d %s", response.Code, response.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	streamRequest.Header.Set("Authorization", "Bearer api-secret")
	streamResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(streamResponse, streamRequest)
	body := streamResponse.Body.String()
	if streamResponse.Code != http.StatusOK || !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected SSE response: %d %s", streamResponse.Code, body)
	}
}

func TestChatRetriesAnotherAccount(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"recovered","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	root := t.TempDir()
	server := New(chatTestConfig(root, upstream.URL))
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "first", "source_type": "grok_sso", "type": "grok"}, {"access_token": "second", "source_type": "grok_sso", "type": "grok"}}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.20-fast","messages":[{"role":"user","content":"retry"}]}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "recovered") || calls != 2 {
		t.Fatalf("retry failed: code=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
}

func TestConsoleChatAndResponses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anonymous" {
			t.Errorf("unexpected console authorization: %q", r.Header.Get("Authorization"))
		}
		if !strings.Contains(r.Header.Get("Cookie"), "sso=console-token") {
			t.Errorf("missing console cookie: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `event: response.output_text.delta`)
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"console "}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `event: response.output_text.delta`)
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"reply"}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `event: response.completed`)
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := chatTestConfig(root, upstream.URL)
	cfg.ConsoleURL = upstream.URL
	server := New(cfg)
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "console-token", "source_type": "grok_sso", "type": "grok"}}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.3-console","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"content":"console reply"`) {
		t.Fatalf("unexpected console response: %d %s", response.Code, response.Body.String())
	}

	streamRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.3-console","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	streamRequest.Header.Set("Authorization", "Bearer api-secret")
	streamResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(streamResponse, streamRequest)
	if streamResponse.Code != http.StatusOK || !strings.Contains(streamResponse.Body.String(), "console ") {
		t.Fatalf("unexpected console stream: %d %s", streamResponse.Code, streamResponse.Body.String())
	}

	responsesRequest := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.3-console","input":"hi"}`))
	responsesRequest.Header.Set("Authorization", "Bearer api-secret")
	responsesResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(responsesResponse, responsesRequest)
	if responsesResponse.Code != http.StatusOK || !strings.Contains(responsesResponse.Body.String(), `"object":"response"`) || !strings.Contains(responsesResponse.Body.String(), "console reply") {
		t.Fatalf("unexpected responses result: %d %s", responsesResponse.Code, responsesResponse.Body.String())
	}
}

func TestChatToolCallsJSONAndStream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"result":{"response":{"token":"<tool_calls><tool_call><tool_name>search</tool_name><parameters>{\"query\":\"go\"}</parameters></tool_call></tool_calls>","messageTag":"final"}}}`)
		fmt.Fprintln(w, `data: [DONE]`)
	}))
	defer upstream.Close()
	root := t.TempDir()
	server := New(chatTestConfig(root, upstream.URL))
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "tool-token", "source_type": "grok_sso", "type": "grok"}}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"grok-4.20-fast","messages":[{"role":"user","content":"find"}],"tools":[{"type":"function","function":{"name":"search","parameters":{"type":"object"}}}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tool_calls"`) || !strings.Contains(response.Body.String(), `"name":"search"`) {
		t.Fatalf("unexpected tool response: %d %s", response.Code, response.Body.String())
	}
}
