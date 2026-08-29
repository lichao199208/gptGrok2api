package agentidentity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRegistersAndEncryptsAgentIdentity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(request["agent_public_key"].(string), "ssh-ed25519 ") {
			t.Fatalf("unexpected public key: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"agent_runtime_id": "runtime-1"})
	}))
	defer upstream.Close()

	root := t.TempDir()
	store := NewStore(filepath.Join(root, "data"), upstream.URL, upstream.Client())
	accessToken := fakeJWT(map[string]any{
		"sub":                            "user-1",
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "account-1", "chatgpt_user_id": "user-1", "chatgpt_plan_type": "plus"},
		"https://api.openai.com/profile": map[string]any{"email": "person@example.test"},
	})
	auth, err := store.Ensure(context.Background(), map[string]any{"access_token": accessToken})
	if err != nil {
		t.Fatal(err)
	}
	identity := auth["agent_identity"].(map[string]any)
	if identity["agent_runtime_id"] != "runtime-1" || identity["agent_private_key"] == "" {
		t.Fatalf("unexpected auth: %#v", auth)
	}
	raw, err := os.ReadFile(filepath.Join(root, "data", "openai_agent_identities.json.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), identity["agent_private_key"].(string)) {
		t.Fatal("private key was written in plaintext")
	}
	summary, err := store.Summary()
	if err != nil || len(summary) != 1 || summary[0]["account_id"] != "account-1" {
		t.Fatalf("unexpected summary: %#v %v", summary, err)
	}
	reused, err := store.Ensure(context.Background(), map[string]any{"access_token": accessToken})
	if err != nil || reused["agent_identity"].(map[string]any)["agent_runtime_id"] != "runtime-1" {
		t.Fatalf("identity was not reused: %#v %v", reused, err)
	}
}

func fakeJWT(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}
