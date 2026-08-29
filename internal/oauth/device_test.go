package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestDeviceServicePendingThenAuthorized(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/device" {
			if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != XAIClientID || r.Form.Get("scope") != XAIScope {
				t.Fatalf("unexpected device form: %#v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "device-code", "user_code": "ABCD-EFGH", "expires_in": 300, "interval": 1, "verification_uri": "https://accounts.x.ai/oauth2/device"})
			return
		}
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("device_code") != "device-code" {
			t.Fatalf("unexpected token form: %#v", r.Form)
		}
		if polls.Add(1) == 2 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "e30.eyJlbWFpbCI6InBlcnNvbkBleGFtcGxlLmNvbSJ9.signature",
				"refresh_token": "refresh-token",
				"expires_in":    3600,
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "oauth.enc")
	service := NewDeviceService(server.Client(), server.URL+"/device", server.URL+"/token", NewStore(path, "secret"))
	started, err := service.Start(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if started["status"] != "pending" || started["user_code"] != "ABCD-EFGH" {
		t.Fatalf("unexpected start response: %#v", started)
	}
	pending, err := service.Poll(context.Background(), started["id"].(string))
	if err != nil || pending["status"] != "pending" {
		t.Fatalf("unexpected pending response: %#v %v", pending, err)
	}
	authorized, err := service.Poll(context.Background(), started["id"].(string))
	if err != nil || authorized["status"] != "authorized" {
		t.Fatalf("unexpected authorized response: %#v %v", authorized, err)
	}
}

func TestTokenEmail(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{"email": "person@example.com"})
	token := fmt.Sprintf("e30.%s.signature", base64.RawURLEncoding.EncodeToString(payload))
	if got := tokenEmail(token); got != "person@example.com" {
		t.Fatalf("unexpected token email: %q", got)
	}
}
