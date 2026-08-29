package httpapi

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type externalManager struct {
	mu         sync.RWMutex
	cpaPath    string
	subPath    string
	cpaPools   []externalCPAPool
	subServers []externalSubServer
}

type externalCPAPool struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	BaseURL   string         `json:"base_url"`
	SecretKey string         `json:"secret_key"`
	ImportJob map[string]any `json:"import_job,omitempty"`
}

type externalSubServer struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	BaseURL   string         `json:"base_url"`
	Email     string         `json:"email"`
	Password  string         `json:"password"`
	APIKey    string         `json:"api_key"`
	GroupID   string         `json:"group_id"`
	VerifyTLS bool           `json:"verify_tls"`
	ImportJob map[string]any `json:"import_job,omitempty"`
}

func newExternalManager(dataDir string) *externalManager {
	m := &externalManager{cpaPath: filepath.Join(dataDir, "cpa_config.json"), subPath: filepath.Join(dataDir, "sub2api_config.json")}
	m.load()
	return m
}

func (m *externalManager) load() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if raw, err := os.ReadFile(m.cpaPath); err == nil {
		var list []externalCPAPool
		if json.Unmarshal(raw, &list) == nil {
			for i := range list {
				normalizeCPAPool(&list[i])
			}
			m.cpaPools = list
		} else {
			var legacy externalCPAPool
			if json.Unmarshal(raw, &legacy) == nil && strings.TrimSpace(legacy.BaseURL) != "" {
				normalizeCPAPool(&legacy)
				m.cpaPools = []externalCPAPool{legacy}
			}
		}
	}
	if raw, err := os.ReadFile(m.subPath); err == nil {
		var list []externalSubServer
		if json.Unmarshal(raw, &list) == nil {
			for i := range list {
				normalizeSubServer(&list[i])
			}
			m.subServers = list
		}
	}
}

func normalizeCPAPool(item *externalCPAPool) {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = externalID("cpa")
	}
	item.Name = strings.TrimSpace(item.Name)
	item.BaseURL = strings.TrimRight(strings.TrimSpace(item.BaseURL), "/")
}

func normalizeSubServer(item *externalSubServer) {
	if strings.TrimSpace(item.ID) == "" {
		item.ID = externalID("sub")
	}
	item.Name = strings.TrimSpace(item.Name)
	item.BaseURL = strings.TrimRight(strings.TrimSpace(item.BaseURL), "/")
	item.Email = strings.TrimSpace(item.Email)
	item.GroupID = strings.TrimSpace(item.GroupID)
}

func externalID(prefix string) string { return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()) }

func (m *externalManager) saveLocked(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".external-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(append(raw, '\n')); err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (m *externalManager) listCPA() []externalCPAPool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]externalCPAPool, len(m.cpaPools))
	copy(result, m.cpaPools)
	return result
}

func (m *externalManager) getCPA(id string) (externalCPAPool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.cpaPools {
		if item.ID == id {
			return item, true
		}
	}
	return externalCPAPool{}, false
}

func (m *externalManager) saveCPAList(list []externalCPAPool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cpaPools = list
	return m.saveLocked(m.cpaPath, list)
}

func (m *externalManager) listSub() []externalSubServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]externalSubServer, len(m.subServers))
	copy(result, m.subServers)
	return result
}

func (m *externalManager) getSub(id string) (externalSubServer, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.subServers {
		if item.ID == id {
			return item, true
		}
	}
	return externalSubServer{}, false
}

func (m *externalManager) saveSubList(list []externalSubServer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subServers = list
	return m.saveLocked(m.subPath, list)
}

func publicCPAPool(item externalCPAPool) map[string]any {
	result := map[string]any{"id": item.ID, "name": item.Name, "base_url": item.BaseURL}
	if item.ImportJob != nil {
		result["import_job"] = item.ImportJob
	}
	return result
}

func publicSubServer(item externalSubServer) map[string]any {
	result := map[string]any{"id": item.ID, "name": item.Name, "base_url": item.BaseURL, "email": item.Email, "group_id": item.GroupID, "verify_tls": item.VerifyTLS, "has_api_key": strings.TrimSpace(item.APIKey) != ""}
	if item.ImportJob != nil {
		result["import_job"] = item.ImportJob
	}
	return result
}

func jobFor(total int) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{"job_id": externalID("job"), "status": "pending", "created_at": now, "updated_at": now, "total": total, "completed": 0, "added": 0, "skipped": 0, "refreshed": 0, "failed": 0, "errors": []any{}}
}

func updateJob(job map[string]any, updates map[string]any) {
	for key, value := range updates {
		job[key] = value
	}
	job["updated_at"] = time.Now().UTC().Format(time.RFC3339)
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	case string:
		n, _ := strconv.Atoi(typed)
		return n
	}
	return 0
}

func appendJobError(job map[string]any, name, message string) {
	errors, _ := job["errors"].([]any)
	errors = append(errors, map[string]any{"name": name, "error": message})
	job["errors"] = errors
	job["failed"] = len(errors)
}

func remoteClient(verifyTLS bool) *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !verifyTLS} // #nosec G402 - explicitly configured for private Sub2API endpoints.
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}
}

func remoteJSON(client *http.Client, method, endpoint string, headers map[string]string, query url.Values, body any) (map[string]any, int, error) {
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, 0, err
	}
	requestURL.RawQuery = query.Encode()
	var reader io.Reader
	if body != nil {
		raw, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, marshalErr
		}
		reader = strings.NewReader(string(raw))
	}
	req, err := http.NewRequest(method, requestURL.String(), reader)
	if err != nil {
		return nil, 0, err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("remote HTTP %d", resp.StatusCode)
	}
	if len(raw) == 0 {
		return map[string]any{}, resp.StatusCode, nil
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, resp.StatusCode, err
	}
	return result, resp.StatusCode, nil
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
