package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func (s *Server) grokRuntimeAdminAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/grok/runtime/admin/"), "/")
	switch {
	case path == "verify" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"status": "success"})
	case path == "status" && r.Method == http.MethodGet:
		accounts, _ := s.store.AccountList()
		writeJSON(w, 200, map[string]any{"status": "ok", "size": len(accounts), "revision": fileRevision(s.cfg.AccountsPath), "selection_strategy": "least_busy"})
	case path == "storage" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"type": "json"})
	case path == "config" && r.Method == http.MethodGet:
		cfg, _ := s.store.Config()
		writeJSON(w, 200, cfg)
	case path == "config" && r.Method == http.MethodPost:
		var updates map[string]any
		if !decodeJSON(w, r, &updates) {
			return
		}
		if _, err := s.store.UpdateConfig("grok_runtime", updates); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"status": "success", "message": "配置已更新"})
	case path == "tokens" && r.Method == http.MethodGet:
		s.grokRuntimeTokens(w)
	case path == "tokens" && r.Method == http.MethodDelete:
		s.grokRuntimeDeleteTokens(w, r)
	case path == "tokens/disabled" && r.Method == http.MethodPost:
		s.grokRuntimeDisabled(w, r)
	case path == "batch/refresh" && r.Method == http.MethodPost:
		var body struct {
			Tokens []string `json:"tokens"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if len(uniqueStrings(body.Tokens)) == 0 {
			writeError(w, http.StatusBadRequest, "tokens is required", "invalid_request_error")
			return
		}
		if strings.TrimSpace(s.cfg.GrokRateLimitsURL) == "" {
			writeError(w, http.StatusServiceUnavailable, "Grok runtime quota endpoint is not configured", "not_configured")
			return
		}
		writeJSON(w, http.StatusOK, s.refreshGrokRuntimeTokens(r.Context(), uniqueStrings(body.Tokens)))
	default:
		writeError(w, 404, "Grok runtime admin endpoint not found", "not_found")
	}
}

func (s *Server) refreshGrokRuntimeTokens(parent context.Context, tokens []string) map[string]any {
	results := make([]map[string]any, 0, len(tokens))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 10)
	for _, token := range uniqueStrings(tokens) {
		token := token
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(parent, 90*time.Second)
			defer cancel()
			quotas, err := s.grokQuota.RefreshToken(ctx, token, map[string]any{"sso": token})
			item := map[string]any{"token": maskRuntimeToken(token), "token_id": runtimeTokenID(token), "ok": err == nil}
			if err != nil {
				item["error"] = safeRefreshError(err)
				if errors.Is(err, provider.ErrGrokInvalidCredentials) {
					item["status"] = "invalid"
				}
			} else {
				item["status"] = "active"
				item["quota"] = quotaMap(quotas)
				s.persistRuntimeToken(token, quotas, "active", "")
			}
			mu.Lock()
			results = append(results, item)
			mu.Unlock()
		}()
	}
	wg.Wait()
	ok, failed := 0, 0
	for _, item := range results {
		if item["ok"] == true {
			ok++
		} else {
			failed++
		}
	}
	return map[string]any{"summary": map[string]int{"total": len(tokens), "ok": ok, "fail": failed}, "results": results}
}

func (s *Server) persistRuntimeToken(token string, quotas map[string]provider.QuotaWindow, status, errorText string) {
	fields := map[string]any{"access_token": token}
	for mode, window := range quotas {
		fields[mode+"_quota"] = map[string]any{"remaining": window.Remaining, "total": window.Total, "window_seconds": window.WindowSeconds, "reset_at": window.ResetAt}
	}
	_, _, _ = s.store.UpdateAccount(token, fields)
	items, err := s.registerStore.ExportAccounts(nil)
	if err != nil {
		return
	}
	for _, item := range items {
		itemToken := firstNonEmpty(stringValue(item["sso"]), stringValue(item["access_token"]), stringValue(item["token"]))
		if strings.TrimSpace(itemToken) != strings.TrimSpace(token) {
			continue
		}
		runtime := map[string]any{"present": true, "status": status, "quota": quotaMap(quotas), "synced_at": time.Now().UTC().Format(time.RFC3339)}
		_, _ = s.registerStore.UpdateRuntime(stringValue(item["id"]), runtime, nil, status, errorText)
	}
}

func quotaMap(quotas map[string]provider.QuotaWindow) map[string]any {
	result := map[string]any{}
	for mode, window := range quotas {
		result[mode] = map[string]any{"remaining": window.Remaining, "total": window.Total, "window_seconds": window.WindowSeconds, "reset_at": window.ResetAt}
	}
	return result
}

func fileRevision(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func (s *Server) grokRuntimeTokens(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	accounts, _ := s.store.AccountList()
	tokens := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		token := firstNonEmpty(stringValue(account["sso"]), stringValue(account["access_token"]), stringValue(account["token"]))
		if token == "" {
			continue
		}
		status := stringValue(account["status"])
		if status == "" {
			status = "active"
		}
		quota := map[string]any{}
		for _, mode := range []string{"auto", "fast", "expert", "heavy", "console"} {
			if value, ok := account[mode+"_quota"].(map[string]any); ok {
				quota[mode] = map[string]any{"remaining": intValue(value["remaining"]), "total": intValue(value["total"])}
			}
		}
		tokens = append(tokens, map[string]any{"token": maskRuntimeToken(token), "token_id": runtimeTokenID(token), "pool": firstNonEmpty(stringValue(account["pool"]), "basic"), "status": status, "quota": quota, "use_count": intValue(account["use_count"]), "fail_count": intValue(account["fail_count"]), "last_used_at": account["last_used_at"], "tags": account["tags"]})
	}
	writeJSON(w, 200, map[string]any{"tokens": tokens})
}

func (s *Server) grokRuntimeDeleteTokens(w http.ResponseWriter, r *http.Request) {
	var tokens []string
	if !decodeJSON(w, r, &tokens) {
		return
	}
	accounts, _ := s.store.AccountList()
	remove := map[string]bool{}
	for _, token := range tokens {
		remove[strings.TrimSpace(token)] = true
	}
	filtered := make([]map[string]any, 0, len(accounts))
	deleted := 0
	for _, account := range accounts {
		token := firstNonEmpty(stringValue(account["sso"]), stringValue(account["access_token"]), stringValue(account["token"]))
		if remove[token] || remove[runtimeTokenID(token)] {
			deleted++
			continue
		}
		filtered = append(filtered, account)
	}
	if err := s.store.SaveAccounts(filtered); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted})
}

func (s *Server) grokRuntimeDisabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		TokenID  string `json:"token_id"`
		Disabled bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	accounts, _ := s.store.AccountList()
	found := false
	for _, account := range accounts {
		token := firstNonEmpty(stringValue(account["sso"]), stringValue(account["access_token"]), stringValue(account["token"]))
		if !runtimeTokenMatches(token, body.Token, body.TokenID) {
			continue
		}
		found = true
		account["enabled"] = !body.Disabled
		if body.Disabled {
			account["status"] = "disabled"
		} else {
			account["status"] = "正常"
		}
	}
	if !found {
		writeError(w, 404, "token not found", "not_found")
		return
	}
	if err := s.store.SaveAccounts(accounts); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "success", "token_id": firstNonEmpty(strings.TrimSpace(body.TokenID), runtimeTokenID(strings.TrimSpace(body.Token))), "disabled": body.Disabled})
}

// runtimeTokenID is a stable opaque selector. It lets the admin UI operate on
// a credential without placing the credential itself in a list response.
func runtimeTokenID(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return "tok_" + hex.EncodeToString(sum[:])
}

func runtimeTokenMatches(token, legacyToken, tokenID string) bool {
	return strings.TrimSpace(token) == strings.TrimSpace(legacyToken) ||
		runtimeTokenID(token) == strings.TrimSpace(tokenID) ||
		runtimeTokenID(token) == strings.TrimSpace(legacyToken)
}

func maskRuntimeToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) <= 12 {
		return "***"
	}
	return token[:6] + "..." + token[len(token)-4:]
}
