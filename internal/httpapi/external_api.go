package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func (s *Server) cpaPoolsAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.URL.Path != "/api/cpa/pools" {
		writeError(w, http.StatusNotFound, "pool not found", "not_found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items := s.external.listCPA()
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			result = append(result, publicCPAPool(item))
		}
		writeJSON(w, http.StatusOK, map[string]any{"pools": result})
	case http.MethodPost:
		var body struct {
			Name      string `json:"name"`
			BaseURL   string `json:"base_url"`
			SecretKey string `json:"secret_key"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if strings.TrimSpace(body.BaseURL) == "" || strings.TrimSpace(body.SecretKey) == "" {
			writeError(w, 400, "base_url and secret_key are required", "invalid_request_error")
			return
		}
		item := externalCPAPool{ID: externalID("cpa"), Name: strings.TrimSpace(body.Name), BaseURL: strings.TrimRight(strings.TrimSpace(body.BaseURL), "/"), SecretKey: strings.TrimSpace(body.SecretKey)}
		items := s.external.listCPA()
		items = append(items, item)
		if err := s.external.saveCPAList(items); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"pool": publicCPAPool(item), "pools": publicCPAPools(items)})
	default:
		writeError(w, 405, "method not allowed", "invalid_request_error")
	}
}

func publicCPAPools(items []externalCPAPool) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, publicCPAPool(item))
	}
	return result
}

func (s *Server) cpaPoolAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, suffix := resourcePath(r.URL.Path, "/api/cpa/pools/")
	if id == "" {
		writeError(w, 404, "pool not found", "not_found")
		return
	}
	item, found := s.external.getCPA(id)
	if !found {
		writeError(w, 404, "pool not found", "not_found")
		return
	}
	if suffix == "files" && r.Method == http.MethodGet {
		files, err := s.cpaRemoteFiles(item)
		if err != nil {
			writeError(w, 502, err.Error(), "upstream_error")
			return
		}
		writeJSON(w, 200, map[string]any{"pool_id": id, "files": files})
		return
	}
	if suffix == "import" {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"import_job": item.ImportJob})
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Names []string `json:"names"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			names := uniqueStrings(body.Names)
			if len(names) == 0 {
				writeError(w, 400, "selected files is required", "invalid_request_error")
				return
			}
			job := jobFor(len(names))
			if err := s.updateCPAJob(id, job); err != nil {
				writeError(w, 500, err.Error(), "server_error")
				return
			}
			go s.runCPAImport(item, names, job)
			writeJSON(w, 200, map[string]any{"import_job": job})
			return
		}
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if suffix != "" {
		writeError(w, 404, "pool endpoint not found", "not_found")
		return
	}
	if r.Method == http.MethodPost {
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		if value, ok := body["name"].(string); ok {
			item.Name = strings.TrimSpace(value)
		}
		if value, ok := body["base_url"].(string); ok && strings.TrimSpace(value) != "" {
			item.BaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		}
		if value, ok := body["secret_key"].(string); ok && strings.TrimSpace(value) != "" {
			item.SecretKey = strings.TrimSpace(value)
		}
		items := s.external.listCPA()
		for i := range items {
			if items[i].ID == id {
				items[i] = item
			}
		}
		if err := s.external.saveCPAList(items); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"pool": publicCPAPool(item), "pools": publicCPAPools(items)})
		return
	}
	if r.Method == http.MethodDelete {
		items := s.external.listCPA()
		filtered := items[:0]
		for _, value := range items {
			if value.ID != id {
				filtered = append(filtered, value)
			}
		}
		if len(filtered) == len(items) {
			writeError(w, 404, "pool not found", "not_found")
			return
		}
		if err := s.external.saveCPAList(filtered); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"pools": publicCPAPools(filtered)})
		return
	}
	writeError(w, 405, "method not allowed", "invalid_request_error")
}

func (s *Server) updateCPAJob(id string, job map[string]any) error {
	items := s.external.listCPA()
	for i := range items {
		if items[i].ID == id {
			items[i].ImportJob = job
			return s.external.saveCPAList(items)
		}
	}
	return os.ErrNotExist
}

func (s *Server) cpaRemoteFiles(pool externalCPAPool) ([]map[string]any, error) {
	payload, _, err := remoteJSON(remoteClient(true), http.MethodGet, pool.BaseURL+"/v0/management/auth-files", map[string]string{"Authorization": "Bearer " + pool.SecretKey, "Accept": "application/json"}, url.Values{}, nil)
	if err != nil {
		return nil, err
	}
	files, _ := payload["files"].([]any)
	result := make([]map[string]any, 0, len(files))
	for _, value := range files {
		if item, ok := value.(map[string]any); ok {
			name := stringValue(item["name"])
			if name != "" {
				result = append(result, map[string]any{"name": name, "email": firstNonEmpty(stringValue(item["email"]), stringValue(item["account"]))})
			}
		}
	}
	return result, nil
}

func (s *Server) runCPAImport(pool externalCPAPool, names []string, job map[string]any) {
	update := func(values map[string]any) { updateJob(job, values); _ = s.updateCPAJob(pool.ID, job) }
	update(map[string]any{"status": "running"})
	for _, name := range names {
		payload, _, err := remoteJSON(remoteClient(true), http.MethodGet, pool.BaseURL+"/v0/management/auth-files/download", map[string]string{"Authorization": "Bearer " + pool.SecretKey, "Accept": "application/json"}, url.Values{"name": []string{name}}, nil)
		if err != nil {
			appendJobError(job, name, err.Error())
		} else if token := stringValue(payload["access_token"]); token != "" {
			added, skipped, _, addErr := s.store.AddAccounts([]string{token}, nil)
			if addErr != nil {
				appendJobError(job, name, addErr.Error())
			} else {
				job["added"] = intValue(job["added"]) + added
				job["skipped"] = intValue(job["skipped"]) + skipped
			}
		} else {
			appendJobError(job, name, "missing access_token")
		}
		job["completed"] = intValue(job["completed"]) + 1
		update(map[string]any{})
	}
	status := "completed"
	if intValue(job["added"]) == 0 && intValue(job["skipped"]) == 0 {
		status = "failed"
	}
	update(map[string]any{"status": status})
}

func (s *Server) sub2APIServersAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.URL.Path != "/api/sub2api/servers" {
		writeError(w, 404, "server not found", "not_found")
		return
	}
	if r.Method == http.MethodGet {
		items := s.external.listSub()
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			result = append(result, publicSubServer(item))
		}
		writeJSON(w, 200, map[string]any{"servers": result})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	var body struct {
		Name      string `json:"name"`
		BaseURL   string `json:"base_url"`
		Email     string `json:"email"`
		Password  string `json:"password"`
		APIKey    string `json:"api_key"`
		GroupID   string `json:"group_id"`
		VerifyTLS *bool  `json:"verify_tls"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.BaseURL) == "" {
		writeError(w, 400, "base_url is required", "invalid_request_error")
		return
	}
	if strings.TrimSpace(body.APIKey) == "" && (strings.TrimSpace(body.Email) == "" || strings.TrimSpace(body.Password) == "") {
		writeError(w, 400, "email+password or api_key is required", "invalid_request_error")
		return
	}
	verify := true
	if body.VerifyTLS != nil {
		verify = *body.VerifyTLS
	}
	item := externalSubServer{ID: externalID("sub"), Name: strings.TrimSpace(body.Name), BaseURL: strings.TrimRight(strings.TrimSpace(body.BaseURL), "/"), Email: strings.TrimSpace(body.Email), Password: body.Password, APIKey: body.APIKey, GroupID: strings.TrimSpace(body.GroupID), VerifyTLS: verify}
	items := append(s.external.listSub(), item)
	if err := s.external.saveSubList(items); err != nil {
		writeError(w, 500, err.Error(), "server_error")
		return
	}
	writeJSON(w, 200, map[string]any{"server": publicSubServer(item), "servers": publicSubServers(items)})
}

func publicSubServers(items []externalSubServer) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, publicSubServer(item))
	}
	return result
}

func (s *Server) sub2APIServerAPI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, suffix := resourcePath(r.URL.Path, "/api/sub2api/servers/")
	item, found := s.external.getSub(id)
	if !found {
		writeError(w, 404, "server not found", "not_found")
		return
	}
	if suffix == "groups" && r.Method == http.MethodGet {
		groups, err := remoteSubGroups(item)
		if err != nil {
			writeError(w, 502, err.Error(), "upstream_error")
			return
		}
		writeJSON(w, 200, map[string]any{"server_id": id, "groups": groups})
		return
	}
	if suffix == "accounts" && r.Method == http.MethodGet {
		groupID := r.URL.Query().Get("group_id")
		if groupID != "" {
			item.GroupID = groupID
		}
		accounts, err := remoteSubAccounts(item)
		if err != nil {
			writeError(w, 502, err.Error(), "upstream_error")
			return
		}
		writeJSON(w, 200, map[string]any{"server_id": id, "accounts": accounts})
		return
	}
	if suffix == "import" {
		if r.Method == http.MethodGet {
			writeJSON(w, 200, map[string]any{"import_job": item.ImportJob})
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				AccountIDs []string `json:"account_ids"`
			}
			if !decodeJSON(w, r, &body) {
				return
			}
			ids := uniqueStrings(body.AccountIDs)
			if len(ids) == 0 {
				writeError(w, 400, "account ids is required", "invalid_request_error")
				return
			}
			job := jobFor(len(ids))
			if err := s.updateSubJob(id, job); err != nil {
				writeError(w, 500, err.Error(), "server_error")
				return
			}
			go s.runSubImport(item, ids, job)
			writeJSON(w, 200, map[string]any{"import_job": job})
			return
		}
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	if suffix != "" {
		writeError(w, 404, "server endpoint not found", "not_found")
		return
	}
	if r.Method == http.MethodPost {
		var body map[string]any
		if !decodeJSON(w, r, &body) {
			return
		}
		if value, ok := body["name"].(string); ok {
			item.Name = strings.TrimSpace(value)
		}
		if value, ok := body["base_url"].(string); ok && strings.TrimSpace(value) != "" {
			item.BaseURL = strings.TrimRight(strings.TrimSpace(value), "/")
		}
		if value, ok := body["email"].(string); ok {
			item.Email = strings.TrimSpace(value)
		}
		if value, ok := body["password"].(string); ok && value != "" {
			item.Password = value
		}
		if value, ok := body["api_key"].(string); ok && value != "" {
			item.APIKey = value
		}
		if value, ok := body["group_id"].(string); ok {
			item.GroupID = strings.TrimSpace(value)
		}
		if value, ok := body["verify_tls"].(bool); ok {
			item.VerifyTLS = value
		}
		items := s.external.listSub()
		for i := range items {
			if items[i].ID == id {
				items[i] = item
			}
		}
		if err := s.external.saveSubList(items); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"server": publicSubServer(item), "servers": publicSubServers(items)})
		return
	}
	if r.Method == http.MethodDelete {
		items := s.external.listSub()
		filtered := items[:0]
		for _, value := range items {
			if value.ID != id {
				filtered = append(filtered, value)
			}
		}
		if len(filtered) == len(items) {
			writeError(w, 404, "server not found", "not_found")
			return
		}
		if err := s.external.saveSubList(filtered); err != nil {
			writeError(w, 500, err.Error(), "server_error")
			return
		}
		writeJSON(w, 200, map[string]any{"servers": publicSubServers(filtered)})
		return
	}
	writeError(w, 405, "method not allowed", "invalid_request_error")
}

func (s *Server) updateSubJob(id string, job map[string]any) error {
	items := s.external.listSub()
	for i := range items {
		if items[i].ID == id {
			items[i].ImportJob = job
			return s.external.saveSubList(items)
		}
	}
	return os.ErrNotExist
}

func subHeaders(item externalSubServer) (map[string]string, error) {
	if strings.TrimSpace(item.APIKey) != "" {
		return map[string]string{"x-api-key": item.APIKey, "Accept": "application/json"}, nil
	}
	payload, _, err := remoteJSON(remoteClient(item.VerifyTLS), http.MethodPost, item.BaseURL+"/api/v1/auth/login", map[string]string{"Accept": "application/json"}, url.Values{}, map[string]string{"email": item.Email, "password": item.Password})
	if err != nil {
		return nil, err
	}
	token := recursiveString(payload, "access_token", "accessToken", "token")
	if token == "" {
		return nil, errors.New("sub2api login did not return access_token")
	}
	return map[string]string{"Authorization": "Bearer " + token, "Accept": "application/json"}, nil
}

func remoteSubGroups(item externalSubServer) ([]map[string]any, error) {
	headers, err := subHeaders(item)
	if err != nil {
		return nil, err
	}
	payload, _, err := remoteJSON(remoteClient(item.VerifyTLS), http.MethodGet, item.BaseURL+"/api/v1/admin/groups", headers, url.Values{"page": []string{"1"}, "page_size": []string{"200"}}, nil)
	if err != nil {
		return nil, err
	}
	values := pagedValues(payload, "items", "list", "groups", "records", "rows")
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if group, ok := value.(map[string]any); ok {
			result = append(result, map[string]any{"id": stringValue(group["id"]), "name": stringValue(group["name"]), "description": stringValue(group["description"]), "platform": stringValue(group["platform"]), "status": stringValue(group["status"]), "account_count": intValue(group["account_count"]), "active_account_count": intValue(group["active_account_count"])})
		}
	}
	return result, nil
}

func remoteSubAccounts(item externalSubServer) ([]map[string]any, error) {
	headers, err := subHeaders(item)
	if err != nil {
		return nil, err
	}
	query := url.Values{"platform": []string{"openai"}, "type": []string{"oauth"}, "account_type": []string{"oauth"}, "page": []string{"1"}, "page_size": []string{"200"}}
	if item.GroupID != "" {
		query.Set("group", item.GroupID)
		query.Set("group_id", item.GroupID)
	}
	payload, _, err := remoteJSON(remoteClient(item.VerifyTLS), http.MethodGet, item.BaseURL+"/api/v1/admin/accounts", headers, query, nil)
	if err != nil {
		return nil, err
	}
	values := pagedValues(payload, "items", "list", "accounts", "records", "rows")
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if account, ok := value.(map[string]any); ok {
			credentials := nestedMap(account, "credentials", "credential")
			result = append(result, map[string]any{"id": firstNonEmpty(stringValue(account["id"]), stringValue(account["account_id"]), stringValue(credentials["id"])), "name": stringValue(account["name"]), "email": firstNonEmpty(stringValue(credentials["email"]), stringValue(account["email"]), stringValue(account["name"])), "plan_type": firstNonEmpty(stringValue(credentials["plan_type"]), stringValue(account["plan_type"])), "status": stringValue(account["status"]), "expires_at": stringValue(credentials["expires_at"]), "has_access_token": recursiveString(account, "access_token", "accessToken", "token") != "" || recursiveString(credentials, "access_token", "accessToken", "token") != "", "has_refresh_token": stringValue(credentials["refresh_token"]) != "", "remote_group_id": firstNonEmpty(stringValue(account["group_id"]), item.GroupID)})
		}
	}
	return result, nil
}

func (s *Server) runSubImport(item externalSubServer, ids []string, job map[string]any) {
	update := func(values map[string]any) { updateJob(job, values); _ = s.updateSubJob(item.ID, job) }
	update(map[string]any{"status": "running"})
	headers, err := subHeaders(item)
	if err != nil {
		for _, id := range ids {
			appendJobError(job, id, err.Error())
			job["completed"] = intValue(job["completed"]) + 1
		}
		update(map[string]any{"status": "failed"})
		return
	}
	for _, id := range ids {
		query := url.Values{"ids": []string{id}, "include_proxies": []string{"false"}}
		payload, _, fetchErr := remoteJSON(remoteClient(item.VerifyTLS), http.MethodGet, item.BaseURL+"/api/v1/admin/accounts/data", headers, query, nil)
		token := ""
		if fetchErr == nil {
			token = recursiveString(payload, "access_token", "accessToken", "token")
		}
		if fetchErr != nil {
			appendJobError(job, id, fetchErr.Error())
		} else if token == "" {
			appendJobError(job, id, "data export missing access_token")
		} else if added, skipped, _, addErr := s.store.AddAccounts([]string{token}, nil); addErr != nil {
			appendJobError(job, id, addErr.Error())
		} else {
			job["added"] = intValue(job["added"]) + added
			job["skipped"] = intValue(job["skipped"]) + skipped
		}
		job["completed"] = intValue(job["completed"]) + 1
		update(map[string]any{})
	}
	status := "completed"
	if intValue(job["added"]) == 0 && intValue(job["skipped"]) == 0 {
		status = "failed"
	}
	update(map[string]any{"status": status})
}

func resourcePath(path, prefix string) (string, string) {
	value := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	parts := strings.SplitN(value, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}
func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
func nestedMap(item map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := item[key].(map[string]any); ok {
			return value
		}
	}
	return map[string]any{}
}
func deepString(item map[string]any, keys ...string) string {
	if value, ok := item[keys[0]]; ok {
		if text := stringValue(value); text != "" {
			return text
		}
		if nested, ok := value.(map[string]any); ok && len(keys) > 1 {
			return deepString(nested, keys[1:]...)
		}
	}
	return ""
}

func recursiveString(value any, keys ...string) string {
	if object, ok := value.(map[string]any); ok {
		for _, key := range keys {
			if text := stringValue(object[key]); text != "" {
				return text
			}
		}
		for _, child := range object {
			if text := recursiveString(child, keys...); text != "" {
				return text
			}
		}
	}
	if list, ok := value.([]any); ok {
		for _, child := range list {
			if text := recursiveString(child, keys...); text != "" {
				return text
			}
		}
	}
	return ""
}
func pagedValues(item map[string]any, keys ...string) []any {
	if data, ok := item["data"].(map[string]any); ok {
		if values := pagedValues(data, keys...); len(values) > 0 {
			return values
		}
	}
	for _, key := range keys {
		if values, ok := item[key].([]any); ok {
			return values
		}
	}
	if data, ok := item["data"].([]any); ok {
		return data
	}
	return nil
}
