package httpapi

import (
	"archive/zip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

func (s *Server) accountOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		EmailHint string `json:"email_hint"`
	}
	if r.Body != nil && r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	result, err := s.openAILogin.Start(body.EmailHint)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error(), "not_configured")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) accountOAuthFinish(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Callback  string `json:"callback"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	tokens, err := s.openAILogin.Finish(r.Context(), body.SessionID, body.Callback)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	_, _, _, err = s.store.AddAccounts(nil, []map[string]any{{"access_token": tokens["access_token"], "refresh_token": tokens["refresh_token"], "id_token": tokens["id_token"], "source_type": "oauth_login", "status": "正常", "enabled": true}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	result, refreshErr := s.refreshImportedOAuth(r.Context(), tokens["access_token"])
	if refreshErr != nil {
		writeError(w, http.StatusBadGateway, refreshErr.Error(), "upstream_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": 1, "refreshed": 1, "errors": []any{}, "items": accountsForAPI([]map[string]any{result})})
}

func (s *Server) refreshImportedOAuth(ctx context.Context, token string) (map[string]any, error) {
	account, err := s.accountByRef(token)
	if err != nil {
		return nil, err
	}
	result, err := s.openAIAccountClient().RefreshAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	updated, _, err := s.store.RotateAccountTokens(token, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Server) accountExport(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		AccessTokens []string `json:"access_tokens"`
		Format       string   `json:"format"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	format := strings.ToLower(strings.TrimSpace(body.Format))
	if format == "" {
		format = "json"
	}
	if format == "agent_identity" {
		s.exportAgentIdentities(w, body.AccessTokens)
		return
	}
	accounts, err := s.exportAccounts(body.AccessTokens)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	stamp := time.Now().Format("20060102-150405")
	w.Header().Set("Cache-Control", "no-store")
	if format == "zip" || format == "cpa" {
		data, err := accountExportZip(accounts)
		if err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="codex-accounts-cpa-`+stamp+`.zip"`)
		_, _ = w.Write(data)
		return
	}
	if format == "sub2api" {
		payload := map[string]any{"exported_at": time.Now().UTC().Format(time.RFC3339), "proxies": []any{}, "accounts": make([]map[string]any, 0, len(accounts))}
		items := payload["accounts"].([]map[string]any)
		for _, account := range accounts {
			items = append(items, sub2APIAccount(account))
		}
		payload["accounts"] = items
		writeDownloadJSON(w, fmt.Sprintf("openai-accounts-sub2api-%s.json", stamp), payload)
		return
	}
	var payload any = accounts[0]
	if len(accounts) != 1 {
		payload = accounts
	}
	writeDownloadJSON(w, fmt.Sprintf("codex-accounts-%s.json", stamp), payload)
}

func (s *Server) exportAccounts(tokens []string) ([]map[string]string, error) {
	items, err := s.store.AccountList()
	if err != nil {
		return nil, err
	}
	resolved, _ := resolveAccountRefTokens(items, tokens)
	targets := map[string]struct{}{}
	for _, token := range resolved {
		targets[token] = struct{}{}
	}
	result := make([]map[string]string, 0, len(items))
	for _, account := range items {
		if len(tokens) > 0 {
			if _, ok := targets[accountToken(account)]; !ok {
				continue
			}
		}
		item, ok := buildAccountExportItem(account)
		if ok {
			result = append(result, item)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("没有可导出的完整账号，需要同时有 access_token、refresh_token 和 id_token")
	}
	return result, nil
}

func buildAccountExportItem(account map[string]any) (map[string]string, bool) {
	access, refresh, id := stringValue(account["access_token"]), stringValue(account["refresh_token"]), stringValue(account["id_token"])
	if access == "" || refresh == "" || id == "" {
		return nil, false
	}
	claims, _ := jwtClaims(access)
	idClaims, _ := jwtClaims(id)
	auth := mapValue(claims["https://api.openai.com/auth"])
	profile := mapValue(claims["https://api.openai.com/profile"])
	email := firstNonEmpty(stringValue(account["email"]), stringValue(profile["email"]), stringValue(idClaims["email"]))
	accountID := firstNonEmpty(stringValue(account["account_id"]), stringValue(auth["chatgpt_account_id"]), stringValue(account["user_id"]))
	item := map[string]string{"type": firstNonEmpty(stringValue(account["export_type"]), "codex"), "email": email, "account_id": accountID, "access_token": access, "refresh_token": refresh, "id_token": id, "expired": jwtTime(claims["exp"]), "last_refresh": jwtTime(claims["iat"])}
	if password := stringValue(account["password"]); password != "" {
		item["password"] = password
	}
	return item, true
}

func sub2APIAccount(account map[string]string) map[string]any {
	credentials := map[string]string{"access_token": account["access_token"]}
	for source, target := range map[string]string{"refresh_token": "refresh_token", "id_token": "id_token", "email": "email", "account_id": "chatgpt_account_id", "expired": "expires_at"} {
		if account[source] != "" {
			credentials[target] = account[source]
		}
	}
	return map[string]any{"name": firstNonEmpty(account["email"], account["account_id"], "OpenAI OAuth Account"), "platform": "openai", "type": "oauth", "credentials": credentials, "extra": map[string]any{"import_source": "chatgpt2api_openai_export", "synced_at": time.Now().UTC().Format(time.RFC3339)}, "concurrency": 1, "priority": 0, "rate_multiplier": 1, "auto_pause_on_expired": true}
}

func accountExportZip(items []map[string]string) ([]byte, error) {
	var output []byte
	writer := newByteWriter(&output)
	archive := zip.NewWriter(writer)
	used := map[string]bool{}
	for index, item := range items {
		base := safeExportName(firstNonEmpty(item["email"], item["account_id"], fmt.Sprintf("account-%03d", index+1)), fmt.Sprintf("account-%03d", index+1))
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		entry, err := archive.Create(name + ".json")
		if err != nil {
			return nil, err
		}
		raw, _ := json.MarshalIndent(item, "", "  ")
		if _, err = entry.Write(append(raw, '\n')); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return output, nil
}

type byteWriter struct{ target *[]byte }

func newByteWriter(target *[]byte) *byteWriter { return &byteWriter{target: target} }
func (w *byteWriter) Write(p []byte) (int, error) {
	*w.target = append(*w.target, p...)
	return len(p), nil
}

func (s *Server) exportAgentIdentities(w http.ResponseWriter, tokens []string) {
	items, err := s.store.AccountList()
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	resolved, _ := resolveAccountRefTokens(items, tokens)
	targets := map[string]struct{}{}
	for _, token := range resolved {
		targets[token] = struct{}{}
	}
	authItems := []map[string]any{}
	errorsFound := []string{}
	for _, account := range items {
		if len(tokens) > 0 {
			if _, ok := targets[accountToken(account)]; !ok {
				continue
			}
		}
		auth, err := s.agentIdentityStore.Ensure(context.Background(), account)
		if err != nil {
			errorsFound = append(errorsFound, firstNonEmpty(stringValue(account["email"]), "unknown account")+": "+err.Error())
			continue
		}
		authItems = append(authItems, auth)
	}
	if len(authItems) == 0 {
		writeError(w, 400, strings.Join(errorsFound, "; "), "invalid_request_error")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	stamp := time.Now().Format("20060102-150405")
	if len(authItems) == 1 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="auth.json"`)
		raw, _ := json.MarshalIndent(authItems[0], "", "  ")
		_, _ = w.Write(append(raw, '\n'))
		return
	}
	var output []byte
	archive := zip.NewWriter(newByteWriter(&output))
	used := map[string]bool{}
	for index, auth := range authItems {
		identity, _ := auth["agent_identity"].(map[string]any)
		base := safeExportName(stringValue(identity["email"]), fmt.Sprintf("account-%d", index+1))
		name := base
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		entry, _ := archive.Create(name + "/auth.json")
		raw, _ := json.MarshalIndent(auth, "", "  ")
		_, _ = entry.Write(append(raw, '\n'))
	}
	_ = archive.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="openai-agent-identities-`+stamp+`.zip"`)
	_, _ = w.Write(output)
}

func (s *Server) agentIdentities(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	items, err := s.agentIdentityStore.Summary()
	if err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"total": len(items), "items": items})
}

func writeDownloadJSON(w http.ResponseWriter, filename string, payload any) {
	raw, _ := json.MarshalIndent(payload, "", "  ")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(append(raw, '\n'))
}

var exportNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeExportName(value, fallback string) string {
	result := strings.Trim(exportNamePattern.ReplaceAllString(strings.TrimSpace(value), "-"), "-._")
	if result == "" {
		result = fallback
	}
	if len(result) > 80 {
		result = result[:80]
	}
	return result
}
func jwtClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var value map[string]any
	err = json.Unmarshal(raw, &value)
	return value, err
}
func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	if result == nil {
		return map[string]any{}
	}
	return result
}
func jwtTime(value any) string {
	number, ok := value.(float64)
	if !ok || number <= 0 {
		return ""
	}
	return time.Unix(int64(number), 0).UTC().Format(time.RFC3339)
}
