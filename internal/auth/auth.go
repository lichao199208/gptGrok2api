package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/store"
)

type Validator struct {
	apiKey         string
	adminKey       string
	authKeysPath   string
	allowAnonymous bool
	repository     *store.Store
}

func New(apiKey, adminKey, authKeysPath string, allowAnonymous bool, repository *store.Store) *Validator {
	return &Validator{
		apiKey:         strings.TrimSpace(apiKey),
		adminKey:       strings.TrimSpace(adminKey),
		authKeysPath:   authKeysPath,
		allowAnonymous: allowAnonymous,
		repository:     repository,
	}
}

func (v *Validator) APIKey(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("Authorization")); value != "" {
		scheme, token, ok := strings.Cut(value, " ")
		if ok && strings.EqualFold(strings.TrimSpace(scheme), "bearer") {
			return strings.TrimSpace(token)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func (v *Validator) AdminKey(r *http.Request) string {
	if token := v.APIKey(r); token != "" {
		return token
	}
	return strings.TrimSpace(r.URL.Query().Get("app_key"))
}

func (v *Validator) ValidAPIRequest(r *http.Request) bool {
	token := v.APIKey(r)
	if token == "" {
		return v.allowAnonymous
	}
	if v.matches(token, v.apiKey) || v.matches(token, v.adminKey) {
		return true
	}
	if v.repository != nil {
		if _, ok := v.repository.Authenticate(token); ok {
			return true
		}
	}
	return v.matchesStoredHash(token)
}

func (v *Validator) ValidAdminRequest(r *http.Request) bool {
	token := v.AdminKey(r)
	if token == "" {
		return false
	}
	if v.matches(token, v.adminKey) || (v.adminKey == "" && v.matches(token, v.apiKey)) {
		return true
	}
	if v.repository != nil {
		identity, ok := v.repository.Authenticate(token)
		return ok && strings.EqualFold(identity.Role, "admin")
	}
	return false
}

func (v *Validator) Identity(token string) (store.Identity, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return store.Identity{}, false
	}
	if v.matches(token, v.adminKey) {
		return store.Identity{ID: "admin", Name: "管理员", Role: "admin", Enabled: true}, true
	}
	if v.matches(token, v.apiKey) {
		if v.adminKey == "" || v.matches(v.apiKey, v.adminKey) {
			return store.Identity{ID: "admin", Name: "管理员", Role: "admin", Enabled: true}, true
		}
		return store.Identity{ID: "api", Name: "API", Role: "user", Enabled: true}, true
	}
	if v.repository != nil {
		return v.repository.Authenticate(token)
	}
	return store.Identity{}, false
}

func (v *Validator) HasConfiguredKey() bool {
	return v.apiKey != "" || v.adminKey != "" || v.authKeysPath != ""
}

func (v *Validator) matches(candidate, expected string) bool {
	if candidate == "" || expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(candidate), []byte(expected)) == 1
}

func (v *Validator) matchesStoredHash(token string) bool {
	raw, err := os.ReadFile(v.authKeysPath)
	if err != nil {
		return false
	}
	var document any
	if json.Unmarshal(raw, &document) != nil {
		return false
	}
	items := extractItems(document)
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	for _, item := range items {
		enabled, ok := item["enabled"].(bool)
		if !ok {
			enabled = true
		}
		if !enabled {
			continue
		}
		if stored, ok := item["key_hash"].(string); ok && v.matches(hash, strings.TrimSpace(stored)) {
			return true
		}
		if stored, ok := item["key"].(string); ok && v.matches(token, strings.TrimSpace(stored)) {
			return true
		}
	}
	return false
}

func extractItems(value any) []map[string]any {
	switch item := value.(type) {
	case []any:
		result := make([]map[string]any, 0, len(item))
		for _, entry := range item {
			if object, ok := entry.(map[string]any); ok {
				result = append(result, object)
			}
		}
		return result
	case map[string]any:
		if nested, ok := item["items"]; ok {
			return extractItems(nested)
		}
	}
	return nil
}

var ErrUnauthorized = errors.New("invalid or missing API key")
