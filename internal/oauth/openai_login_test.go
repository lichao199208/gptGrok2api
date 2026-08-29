package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOpenAILoginStartFinishUsesPKCEAndConsumesSession(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"id_token":      "id-token",
		})
	}))
	defer upstream.Close()

	login := NewOpenAILogin(upstream.URL, "https://platform.example.test", upstream.URL+"/token", upstream.Client())
	started, err := login.Start("person@example.test")
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(started["authorize_url"])
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	for _, key := range []string{"client_id", "state", "nonce", "code_challenge", "code_challenge_method", "redirect_uri"} {
		if strings.TrimSpace(query.Get(key)) == "" {
			t.Fatalf("authorize URL missing %s: %s", key, authorize.String())
		}
	}
	if query.Get("code_challenge_method") != "S256" || query.Get("login_hint") != "person@example.test" {
		t.Fatalf("unexpected PKCE query: %s", authorize.RawQuery)
	}
	state := query.Get("state")
	callback := "https://platform.example.test/auth/callback?code=one-time-code&state=" + url.QueryEscape(state)
	tokens, err := login.Finish(context.Background(), started["session_id"], callback)
	if err != nil {
		t.Fatal(err)
	}
	if tokens["access_token"] != "access-token" || tokens["refresh_token"] != "refresh-token" {
		t.Fatalf("unexpected tokens: %#v", tokens)
	}
	if received["code_verifier"] == nil || received["client_id"] != OpenAIOAuthClientID {
		t.Fatalf("token exchange did not include PKCE fields: %#v", received)
	}
	if _, err := login.Finish(context.Background(), started["session_id"], "one-time-code"); err == nil {
		t.Fatal("expected consumed session to be rejected")
	}
}

func TestOpenAILoginRejectsMismatchedStateWithoutConsumingSession(t *testing.T) {
	login := NewOpenAILogin("https://auth.example.test", "https://platform.example.test", "https://token.example.test", http.DefaultClient)
	started, err := login.Start("")
	if err != nil {
		t.Fatal(err)
	}
	callback := "https://platform.example.test/auth/callback?code=code&state=" + url.QueryEscape(started["session_id"]+".wrong")
	if _, err := login.Finish(context.Background(), started["session_id"], callback); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("expected state mismatch, got %v", err)
	}
}
