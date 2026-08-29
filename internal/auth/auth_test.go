package auth

import (
	"net/http"
	"testing"
)

func TestValidatorAcceptsBearerAndAPIKey(t *testing.T) {
	validator := New("api-secret", "admin-secret", "", false, nil)
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer api-secret")
	if !validator.ValidAPIRequest(request) {
		t.Fatal("expected bearer API key to be accepted")
	}
	request.Header.Set("X-API-Key", "admin-secret")
	request.Header.Del("Authorization")
	if !validator.ValidAdminRequest(request) {
		t.Fatal("expected admin API key to be accepted")
	}
}

func TestLegacyAPIKeyIsAdminWithoutSeparateAdminKey(t *testing.T) {
	validator := New("legacy-secret", "", "", false, nil)
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer legacy-secret")
	if !validator.ValidAdminRequest(request) {
		t.Fatal("expected legacy API key to retain admin access")
	}
	identity, ok := validator.Identity("legacy-secret")
	if !ok || identity.Role != "admin" {
		t.Fatalf("expected admin identity, got %#v", identity)
	}
}

func TestSeparateAPIKeyDoesNotAuthorizeAdminRoutes(t *testing.T) {
	validator := New("api-secret", "admin-secret", "", false, nil)
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Authorization", "Bearer api-secret")
	if validator.ValidAdminRequest(request) {
		t.Fatal("expected separate API key to be rejected for admin access")
	}
	identity, ok := validator.Identity("api-secret")
	if !ok || identity.Role != "user" {
		t.Fatalf("expected user identity, got %#v", identity)
	}
}
