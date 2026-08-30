package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Identity struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
}

type Store struct {
	accountsPath string
	authKeysPath string
	configPath   string
	mu           sync.RWMutex
}

func New(accountsPath, authKeysPath, configPath string) *Store {
	return &Store{
		accountsPath: accountsPath,
		authKeysPath: authKeysPath,
		configPath:   configPath,
	}
}

func (s *Store) AccountsPath() string {
	return s.accountsPath
}

func (s *Store) AuthKeysPath() string {
	return s.authKeysPath
}

func (s *Store) LoadAccounts() ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return loadList(s.accountsPath)
}

func (s *Store) SaveAccounts(items []map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.accountsPath, items)
}

func (s *Store) LoadAuthKeys() ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return loadItems(s.authKeysPath)
}

func (s *Store) SaveAuthKeys(items []map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.authKeysPath, map[string]any{"items": items})
}

func (s *Store) Config() (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return loadMap(s.configPath)
}

func (s *Store) UpdateConfig(key string, value any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := loadMap(s.configPath)
	if err != nil {
		return nil, err
	}
	current[key] = value
	if err := writeJSON(s.configPath, current); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) MutateConfig(key string, mutate func(any) (any, error)) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := loadMap(s.configPath)
	if err != nil {
		return nil, err
	}
	next, err := mutate(current[key])
	if err != nil {
		return nil, err
	}
	current[key] = next
	if err := writeJSON(s.configPath, current); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) ReplaceConfig(value map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSON(s.configPath, value)
}

func (s *Store) Authenticate(token string) (Identity, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Identity{}, false
	}
	sum := sha256.Sum256([]byte(token))
	candidate := hex.EncodeToString(sum[:])

	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := loadItems(s.authKeysPath)
	if err != nil {
		return Identity{}, false
	}
	for index, item := range items {
		if enabled, ok := item["enabled"].(bool); ok && !enabled {
			continue
		}
		stored, _ := item["key_hash"].(string)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(strings.TrimSpace(stored))) != 1 {
			continue
		}
		now := time.Now().UTC().Format(time.RFC3339)
		item["last_used_at"] = now
		items[index] = item
		_ = writeJSON(s.authKeysPath, map[string]any{"items": items})
		return identityFromItem(item), true
	}
	return Identity{}, false
}

func (s *Store) ListPublicKeys(role string) ([]map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items, err := loadItems(s.authKeysPath)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if role != "" && strings.ToLower(stringValue(item["role"])) != strings.ToLower(role) {
			continue
		}
		result = append(result, publicKey(item))
	}
	return result, nil
}

func (s *Store) CreateKey(role, name, adminKey string) (map[string]any, string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role != "admin" && role != "user" {
		return nil, "", errors.New("invalid key role")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := loadItems(s.authKeysPath)
	if err != nil {
		return nil, "", err
	}
	name = uniqueName(items, name, role)
	rawKey, err := randomKey()
	if err != nil {
		return nil, "", err
	}
	hash := hashKey(rawKey)
	if strings.TrimSpace(adminKey) != "" && subtle.ConstantTimeCompare([]byte(rawKey), []byte(strings.TrimSpace(adminKey))) == 1 {
		return nil, "", errors.New("key conflicts with admin key")
	}
	for _, item := range items {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(stringValue(item["key_hash"]))) == 1 {
			return nil, "", errors.New("key already exists")
		}
	}
	item := map[string]any{
		"id":           randomID(),
		"name":         name,
		"role":         role,
		"key_hash":     hash,
		"enabled":      true,
		"created_at":   time.Now().UTC().Format(time.RFC3339),
		"last_used_at": nil,
	}
	items = append(items, item)
	if err := writeJSON(s.authKeysPath, map[string]any{"items": items}); err != nil {
		return nil, "", err
	}
	return publicKey(item), rawKey, nil
}

func (s *Store) UpdateKey(id, role string, updates map[string]any, adminKey string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := loadItems(s.authKeysPath)
	if err != nil {
		return nil, err
	}
	for index, item := range items {
		if stringValue(item["id"]) != strings.TrimSpace(id) {
			continue
		}
		if role != "" && strings.ToLower(stringValue(item["role"])) != strings.ToLower(role) {
			return nil, os.ErrNotExist
		}
		next := cloneMap(item)
		if value, ok := updates["name"]; ok {
			next["name"] = uniqueNameExcluding(items, stringValue(value), stringValue(item["role"]), id)
		}
		if value, ok := updates["enabled"]; ok {
			next["enabled"] = boolValue(value, true)
		}
		if value, ok := updates["key"]; ok {
			rawKey := strings.TrimSpace(stringValue(value))
			if rawKey == "" {
				return nil, errors.New("key cannot be empty")
			}
			if subtle.ConstantTimeCompare([]byte(rawKey), []byte(strings.TrimSpace(adminKey))) == 1 {
				return nil, errors.New("key conflicts with admin key")
			}
			next["key_hash"] = hashKey(rawKey)
		}
		items[index] = next
		if err := writeJSON(s.authKeysPath, map[string]any{"items": items}); err != nil {
			return nil, err
		}
		return publicKey(next), nil
	}
	return nil, os.ErrNotExist
}

func (s *Store) DeleteKey(id, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := loadItems(s.authKeysPath)
	if err != nil {
		return err
	}
	filtered := make([]map[string]any, 0, len(items))
	removed := false
	for _, item := range items {
		matches := stringValue(item["id"]) == strings.TrimSpace(id)
		if role != "" {
			matches = matches && strings.EqualFold(stringValue(item["role"]), role)
		}
		if matches {
			removed = true
			continue
		}
		filtered = append(filtered, item)
	}
	if !removed {
		return os.ErrNotExist
	}
	return writeJSON(s.authKeysPath, map[string]any{"items": filtered})
}

func (s *Store) AccountList() ([]map[string]any, error) {
	return s.LoadAccounts()
}

func (s *Store) AddAccounts(tokens []string, payloads []map[string]any) (int, int, []map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := loadList(s.accountsPath)
	if err != nil {
		return 0, 0, nil, err
	}
	byToken := make(map[string]int, len(accounts))
	for index, item := range accounts {
		if token := accountToken(item); token != "" {
			byToken[token] = index
		}
	}
	added, skipped := 0, 0
	for _, payload := range payloads {
		token := accountToken(payload)
		if token == "" {
			continue
		}
		if _, ok := byToken[token]; ok {
			skipped++
			continue
		}
		item := cloneMap(payload)
		normalizeAccount(item)
		accounts = append(accounts, item)
		byToken[token] = len(accounts) - 1
		added++
	}
	for _, rawToken := range tokens {
		token := strings.TrimSpace(rawToken)
		if token == "" {
			continue
		}
		if _, ok := byToken[token]; ok {
			skipped++
			continue
		}
		item := map[string]any{
			"access_token": token,
			"status":       "正常",
			"source_type":  "web",
			"enabled":      true,
			"created_at":   time.Now().UTC().Format(time.RFC3339),
		}
		accounts = append(accounts, item)
		byToken[token] = len(accounts) - 1
		added++
	}
	if added > 0 {
		if err := writeJSON(s.accountsPath, accounts); err != nil {
			return 0, 0, nil, err
		}
	}
	return added, skipped, accounts, nil
}

func (s *Store) DeleteAccounts(tokens []string) (int, []map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := loadList(s.accountsPath)
	if err != nil {
		return 0, nil, err
	}
	targets := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if clean := strings.TrimSpace(token); clean != "" {
			targets[clean] = struct{}{}
		}
	}
	filtered := make([]map[string]any, 0, len(accounts))
	removed := 0
	for _, item := range accounts {
		if _, ok := targets[accountToken(item)]; ok {
			removed++
			continue
		}
		filtered = append(filtered, item)
	}
	if removed > 0 {
		if err := writeJSON(s.accountsPath, filtered); err != nil {
			return 0, nil, err
		}
	}
	return removed, filtered, nil
}

func (s *Store) UpdateAccount(token string, updates map[string]any) (map[string]any, []map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts, err := loadList(s.accountsPath)
	if err != nil {
		return nil, nil, err
	}
	for index, item := range accounts {
		if accountToken(item) != strings.TrimSpace(token) {
			continue
		}
		next := cloneMap(item)
		for key, value := range updates {
			next[key] = value
		}
		accounts[index] = next
		if err := writeJSON(s.accountsPath, accounts); err != nil {
			return nil, nil, err
		}
		return next, accounts, nil
	}
	return nil, accounts, os.ErrNotExist
}

// RotateAccountTokens updates an account using its previous access token as
// the lookup key. Keeping this operation in Store avoids a race between a
// refresh-token rotation and another concurrent account update.
func (s *Store) RotateAccountTokens(oldToken, newToken, refreshToken, idToken string, fields map[string]any) (map[string]any, []map[string]any, error) {
	updates := cloneMap(fields)
	if strings.TrimSpace(newToken) != "" {
		updates["access_token"] = strings.TrimSpace(newToken)
	}
	if strings.TrimSpace(refreshToken) != "" {
		updates["refresh_token"] = strings.TrimSpace(refreshToken)
	}
	if strings.TrimSpace(idToken) != "" {
		updates["id_token"] = strings.TrimSpace(idToken)
	}
	updates["last_token_refresh_at"] = time.Now().UTC().Format(time.RFC3339)
	updates["last_token_refresh_error"] = nil
	updates["last_token_refresh_error_at"] = nil
	updates["invalid_count"] = 0
	updates["cooldown_until"] = nil
	updates["next_retry_at"] = nil
	// A successful remote refresh supersedes every error marker left by an
	// earlier refresh or request attempt. Keeping any of these fields makes the
	// admin status endpoint classify an otherwise healthy account as abnormal.
	for _, key := range []string{
		"last_refresh_error",
		"last_refresh_error_at",
		"last_refresh_warning",
		"last_refresh_warning_at",
		"last_error_kind",
		"last_error_status",
		"last_error_message",
		"last_error_at",
		"status_reason_code",
	} {
		updates[key] = nil
	}
	return s.UpdateAccount(oldToken, updates)
}

func loadList(path string) ([]map[string]any, error) {
	raw, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return []map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if list, ok := value.([]any); ok {
		return objectList(list), nil
	}
	return []map[string]any{}, nil
}

func loadItems(path string) ([]map[string]any, error) {
	raw, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []map[string]any{}, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return []map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		if nested, ok := object["items"].([]any); ok {
			return objectList(nested), nil
		}
	}
	if list, ok := value.([]any); ok {
		return objectList(list), nil
	}
	return []map[string]any{}, nil
}

func loadMap(path string) (map[string]any, error) {
	raw, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func writeJSON(path string, value any) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("empty storage path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func objectList(items []any) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if object, ok := item.(map[string]any); ok {
			result = append(result, object)
		}
	}
	return result
}

func identityFromItem(item map[string]any) Identity {
	return Identity{
		ID:         stringValue(item["id"]),
		Name:       stringValue(item["name"]),
		Role:       stringValue(item["role"]),
		Enabled:    boolValue(item["enabled"], true),
		CreatedAt:  stringValue(item["created_at"]),
		LastUsedAt: stringValue(item["last_used_at"]),
	}
}

func publicKey(item map[string]any) map[string]any {
	return map[string]any{
		"id":           stringValue(item["id"]),
		"name":         stringValue(item["name"]),
		"role":         stringValue(item["role"]),
		"enabled":      boolValue(item["enabled"], true),
		"created_at":   stringValue(item["created_at"]),
		"last_used_at": item["last_used_at"],
	}
}

func uniqueName(items []map[string]any, requested, role string) string {
	return uniqueNameExcluding(items, requested, role, "")
}

func uniqueNameExcluding(items []map[string]any, requested, role, excludedID string) string {
	name := strings.TrimSpace(requested)
	if name == "" {
		if strings.EqualFold(role, "admin") {
			name = "管理员密钥"
		} else {
			name = "普通用户"
		}
	}
	if !nameTaken(items, name, role, excludedID) {
		return name
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s %d", name, suffix)
		if !nameTaken(items, candidate, role, excludedID) {
			return candidate
		}
	}
}

func nameTaken(items []map[string]any, name, role, excludedID string) bool {
	for _, item := range items {
		if stringValue(item["id"]) == excludedID {
			continue
		}
		if !strings.EqualFold(stringValue(item["role"]), role) {
			continue
		}
		if stringValue(item["name"]) == strings.TrimSpace(name) {
			return true
		}
	}
	return false
}

func randomKey() (string, error) {
	buffer := make([]byte, 18)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buffer), nil
}

func randomID() string {
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func accountToken(item map[string]any) string {
	if token := stringValue(item["access_token"]); token != "" {
		return token
	}
	return stringValue(item["accessToken"])
}

func normalizeAccount(item map[string]any) {
	if _, ok := item["access_token"]; !ok {
		if token := stringValue(item["accessToken"]); token != "" {
			item["access_token"] = token
		}
	}
	if _, ok := item["status"]; !ok {
		item["status"] = "正常"
	}
	if _, ok := item["source_type"]; !ok {
		item["source_type"] = "web"
	}
	if _, ok := item["enabled"]; !ok {
		item["enabled"] = true
	}
	if _, ok := item["created_at"]; !ok {
		item["created_at"] = time.Now().UTC().Format(time.RFC3339)
	}
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func boolValue(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}
