package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAISurvivalRunUsesRealAccountEndpoints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer survival-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "survival@example.test", "id": "user-survival"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_model_slug": "gpt-5", "limits_progress": []any{}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := adminTestConfig(root)
	cfg.OpenAIBaseURL = upstream.URL
	cfg.OpenAIOAuthURL = upstream.URL + "/oauth/token"
	if err := os.MkdirAll(filepath.Dir(cfg.RegisterPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RegisterPath, []byte(`{"openai_survival":{"enabled":true,"concurrency":1,"refresh_codex_rt":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(cfg)
	token := "survival-token"
	if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": token, "source_type": "chatgpt_web"}}); err != nil {
		t.Fatal(err)
	}
	response := adminRequest(server.Handler(), http.MethodPost, "/api/register/openai/survival/run", nil)
	if response.Code != http.StatusAccepted {
		t.Fatalf("survival run returned %d: %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := server.survivalSnapshot()
		if running, _ := status["running"].(bool); !running {
			if summary, ok := status["last_summary"].(map[string]any); ok && intValue(summary["confirmed"]) == 1 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("survival did not confirm account: %#v", server.survivalSnapshot())
}
