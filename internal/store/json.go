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
	accountsPath     string
	deletedPath      string
	authKeysPath     string
	configPath       string
	mu               sync.RWMutex
	accountsWriteMu  sync.Mutex
	accountsCache    []map[string]any
	accountsIndex    map[string]int
	accountsLoaded   bool
	accountsRevision uint64
	accountsDirty    bool
	accountsTimer    *time.Timer
	// tokenAliases retains the in-process lineage for rotated OAuth access
	// tokens. The verified reference implementation resolves an old lease
	// through this alias chain before it records task feedback.
	tokenAliases map[string]string
}

// CredentialGeneration identifies one concrete OAuth credential set. It
// mirrors the reference account service: stale refresh responses/errors may
// only update storage when all three values still match.
type CredentialGeneration struct {
	AccessToken        string
	RefreshToken       string
	LastTokenRefreshAt string
}

const accountRuntimeFlushDelay = time.Second

func New(accountsPath, authKeysPath, configPath string) *Store {
	return &Store{
		accountsPath: accountsPath,
		deletedPath:  filepath.Join(filepath.Dir(accountsPath), "deleted_accounts.json"),
		authKeysPath: authKeysPath,
		configPath:   configPath,
		tokenAliases: map[string]string{},
	}
}

func (s *Store) AccountsPath() string {
	return s.accountsPath
}

func (s *Store) AuthKeysPath() string {
	return s.authKeysPath
}

func (s *Store) LoadAccounts() ([]map[string]any, error) {
	items, _, err := s.AccountSnapshot()
	if err != nil {
		return nil, err
	}
	return cloneAccountList(items), nil
}

func (s *Store) SaveAccounts(items []map[string]any) error {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := normalizeAccountListJSON(items)
	if err := writeJSON(s.accountsPath, next); err != nil {
		return err
	}
	s.replaceAccountsLocked(next)
	s.accountsDirty = false
	return nil
}

// AccountSnapshot returns an immutable in-memory account view. Callers must
// not mutate the returned slice or maps. Store mutations always use copy-on-write.
func (s *Store) AccountSnapshot() ([]map[string]any, uint64, error) {
	s.mu.RLock()
	if s.accountsLoaded {
		items, revision := s.accountsCache, s.accountsRevision
		s.mu.RUnlock()
		return items, revision, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, 0, err
	}
	return s.accountsCache, s.accountsRevision, nil
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

// ResolveAccount returns the current account for token. A token from an
// in-flight lease may have rotated while the request was running; aliases make
// that lease resolve to the current persisted account rather than silently
// losing counters or quota updates.
func (s *Store) ResolveAccount(token string) (string, map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return "", nil, err
	}
	resolved := s.resolveAccountTokenLocked(token)
	index, ok := s.accountsIndex[resolved]
	if !ok || index < 0 || index >= len(s.accountsCache) {
		return resolved, nil, os.ErrNotExist
	}
	return resolved, cloneMap(s.accountsCache[index]), nil
}

// CredentialSnapshot returns the active account and the generation used for a
// refresh attempt. Callers must use the same generation when applying the
// outcome so an old refresh token cannot overwrite newer credentials.
func (s *Store) CredentialSnapshot(token string) (string, map[string]any, CredentialGeneration, error) {
	resolved, account, err := s.ResolveAccount(token)
	if err != nil {
		return resolved, nil, CredentialGeneration{}, err
	}
	return resolved, account, credentialGeneration(resolved, account), nil
}

func (s *Store) AddAccounts(tokens []string, payloads []map[string]any) (int, int, []map[string]any, error) {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return 0, 0, nil, err
	}
	deleted, err := loadDeletedTokens(s.deletedPath)
	if err != nil {
		return 0, 0, nil, err
	}
	accounts := cloneAccountList(s.accountsCache)
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
		if deleted[tokenHash(token)] {
			skipped++
			continue
		}
		if s.resolveAccountTokenLocked(token) != token {
			// Do not recreate a former access token after an OAuth rotation.
			skipped++
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
		if deleted[tokenHash(token)] {
			skipped++
			continue
		}
		if s.resolveAccountTokenLocked(token) != token {
			skipped++
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
		accounts = normalizeAccountListJSON(accounts)
		s.replaceAccountsLocked(accounts)
		s.accountsDirty = false
	}
	return added, skipped, cloneAccountList(accounts), nil
}

func (s *Store) DeleteAccounts(tokens []string) (int, []map[string]any, error) {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return 0, nil, err
	}
	deleted, err := loadDeletedTokens(s.deletedPath)
	if err != nil {
		return 0, nil, err
	}
	accounts := s.accountsCache
	targets := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if clean := normalizeToken(token); clean != "" {
			targets[clean] = struct{}{}
			if resolved := s.resolveAccountTokenLocked(clean); resolved != "" {
				targets[resolved] = struct{}{}
			}
		}
	}
	for token := range targets {
		deleted[tokenHash(token)] = true
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
		s.replaceAccountsLocked(filtered)
		s.removeAccountTokenAliasesLocked(targets)
		s.accountsDirty = false
	}
	if len(targets) > 0 {
		if err := writeDeletedTokens(s.deletedPath, deleted); err != nil {
			return 0, nil, err
		}
	}
	return removed, cloneAccountList(filtered), nil
}

func normalizeToken(token string) string {
	token = strings.TrimSpace(token)
	if len(token) >= 4 && strings.EqualFold(token[:4], "sso=") {
		return strings.TrimSpace(token[4:])
	}
	return token
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(normalizeToken(token)))
	return hex.EncodeToString(sum[:])
}

func loadDeletedTokens(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var hashes []string
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &hashes); err != nil {
			return nil, err
		}
	}
	result := make(map[string]bool, len(hashes))
	for _, hash := range hashes {
		if strings.TrimSpace(hash) != "" {
			result[strings.TrimSpace(hash)] = true
		}
	}
	return result, nil
}

func writeDeletedTokens(path string, deleted map[string]bool) error {
	hashes := make([]string, 0, len(deleted))
	for hash := range deleted {
		hashes = append(hashes, hash)
	}
	return writeJSON(path, hashes)
}

func (s *Store) UpdateAccount(token string, updates map[string]any) (map[string]any, []map[string]any, error) {
	updated, items, _, err := s.updateAccount(token, updates, nil)
	return updated, items, err
}

// UpdateAccountIfCredentials applies updates only if the account still owns
// expected. It is the compare-and-swap counterpart used for OAuth refresh
// errors, copied from the verified reference service's credential-generation
// guard. A false applied result means another refresh already won the race.
func (s *Store) UpdateAccountIfCredentials(token string, expected CredentialGeneration, updates map[string]any) (map[string]any, bool, error) {
	updated, _, applied, err := s.updateAccount(token, updates, &expected)
	return updated, applied, err
}

func (s *Store) updateAccount(token string, updates map[string]any, expected *CredentialGeneration) (map[string]any, []map[string]any, bool, error) {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, nil, false, err
	}
	resolved := s.resolveAccountTokenLocked(token)
	index, ok := s.accountsIndex[resolved]
	if !ok || index < 0 || index >= len(s.accountsCache) {
		return nil, cloneAccountList(s.accountsCache), false, os.ErrNotExist
	}
	if expected != nil && credentialGeneration(resolved, s.accountsCache[index]) != *expected {
		return cloneMap(s.accountsCache[index]), cloneAccountList(s.accountsCache), false, nil
	}
	accounts, next, ok := s.updatedAccountsLocked(resolved, updates)
	if !ok {
		return nil, cloneAccountList(s.accountsCache), false, os.ErrNotExist
	}
	if err := writeJSON(s.accountsPath, accounts); err != nil {
		return nil, nil, false, err
	}
	s.replaceAccountsLocked(accounts)
	s.accountsDirty = false
	result := cloneMap(next)
	for key, value := range updates {
		result[key] = value
	}
	return result, cloneAccountList(accounts), true, nil
}

// UpdateAccountRuntime updates request-derived status immediately in memory
// and coalesces disk writes. This avoids rewriting a large account file for
// every concurrent success, timeout, or proxy failure.
func (s *Store) UpdateAccountRuntime(token string, updates map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, err
	}
	accounts, next, ok := s.updatedAccountsLocked(token, updates)
	if !ok {
		return nil, os.ErrNotExist
	}
	s.replaceAccountsLocked(accounts)
	s.accountsDirty = true
	s.scheduleAccountFlushLocked(accountRuntimeFlushDelay)
	result := cloneMap(next)
	for key, value := range updates {
		result[key] = value
	}
	return result, nil
}

// RecordAccountRequestResult records one completed upstream task for an account
// together with optional runtime state updates. The counter increment happens
// while holding the account cache lock so concurrent task completions cannot
// lose each other's success/failure counts.
func (s *Store) RecordAccountRequestResult(token string, successful bool, updates map[string]any) (map[string]any, error) {
	updated, _, err := s.recordAccountResultIfCredentials(token, CredentialGeneration{}, successful, updates, false)
	return updated, err
}

// RecordAccountRequestResultIfCredentials records task counters for the current
// account, but applies health updates only if the request used the account's
// current credential generation. This lets an in-flight old token report its
// outcome without poisoning a newer token that replaced it meanwhile.
func (s *Store) RecordAccountRequestResultIfCredentials(token string, expected CredentialGeneration, successful bool, updates map[string]any) (map[string]any, bool, error) {
	return s.recordAccountResultIfCredentials(token, expected, successful, updates, false)
}

// RecordAccountImageResult records one final image outcome. It intentionally
// does not change quota: upstream quota is consumed as soon as image generation
// completes, which can be before the local download/resolve step succeeds.
func (s *Store) RecordAccountImageResult(token string, successful bool, updates map[string]any) (map[string]any, error) {
	updated, _, err := s.recordAccountResultIfCredentials(token, CredentialGeneration{}, successful, updates, false)
	return updated, err
}

// RecordAccountImageResultIfCredentials is the image counterpart to
// RecordAccountRequestResultIfCredentials.
func (s *Store) RecordAccountImageResultIfCredentials(token string, expected CredentialGeneration, successful bool, updates map[string]any) (map[string]any, bool, error) {
	return s.recordAccountResultIfCredentials(token, expected, successful, updates, false)
}

// RecordAccountImageConsumption synchronously persists image quota before the
// lease is released. Unlike ordinary runtime counters this accounting cannot be
// delayed: a process restart must not put an already-consumed image slot back
// into service.
func (s *Store) RecordAccountImageConsumption(token string, updates map[string]any) (map[string]any, error) {
	updated, _, err := s.recordAccountMutation(token, updates, true, true, CredentialGeneration{})
	return updated, err
}

func (s *Store) recordAccountResultIfCredentials(token string, expected CredentialGeneration, successful bool, updates map[string]any, consumeImageQuota bool) (map[string]any, bool, error) {
	runtimeUpdates := cloneMap(updates)
	counter := "fail"
	if successful {
		counter = "success"
	}
	return s.recordAccountMutation(token, runtimeUpdates, consumeImageQuota, false, expected, counter)
}

func (s *Store) recordAccountMutation(token string, updates map[string]any, consumeImageQuota, persist bool, expected CredentialGeneration, counters ...string) (map[string]any, bool, error) {
	if persist {
		s.accountsWriteMu.Lock()
		defer s.accountsWriteMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, false, err
	}
	resolved := s.resolveAccountTokenLocked(token)
	index, ok := s.accountsIndex[resolved]
	if !ok || index < 0 || index >= len(s.accountsCache) {
		return nil, false, os.ErrNotExist
	}
	credentialUpdatesApplied := expected == (CredentialGeneration{}) || credentialGeneration(resolved, s.accountsCache[index]) == expected

	runtimeUpdates := map[string]any{}
	if credentialUpdatesApplied {
		runtimeUpdates = cloneMap(updates)
	}
	if len(counters) > 0 {
		counter := counters[0]
		if counter != "" {
			runtimeUpdates[counter] = nonNegativeAccountCount(s.accountsCache[index][counter]) + 1
		}
	}
	if consumeImageQuota && !boolValue(s.accountsCache[index]["image_quota_unknown"], false) {
		quota := nonNegativeAccountCount(s.accountsCache[index]["quota"])
		if quota > 0 {
			quota--
			runtimeUpdates["quota"] = quota
			if quota == 0 {
				runtimeUpdates["status"] = "限流"
				runtimeUpdates["status_reason_code"] = "image_quota_pending_confirmation"
				runtimeUpdates["image_quota_pending_confirmation"] = true
			}
		}
	}

	accounts, next, ok := s.updatedAccountsLocked(resolved, runtimeUpdates)
	if !ok {
		return nil, false, os.ErrNotExist
	}
	if persist {
		if err := writeJSON(s.accountsPath, accounts); err != nil {
			return nil, false, err
		}
		s.replaceAccountsLocked(accounts)
		s.accountsDirty = false
		return cloneMap(next), credentialUpdatesApplied, nil
	}
	s.replaceAccountsLocked(accounts)
	s.accountsDirty = true
	s.scheduleAccountFlushLocked(accountRuntimeFlushDelay)
	return cloneMap(next), credentialUpdatesApplied, nil
}

func nonNegativeAccountCount(value any) int {
	var count int
	_, _ = fmt.Sscanf(strings.TrimSpace(fmt.Sprint(value)), "%d", &count)
	if count < 0 {
		return 0
	}
	return count
}

// FlushAccounts persists pending runtime feedback. It is primarily useful for
// orderly shutdowns and deterministic tests; normal requests use the timer.
func (s *Store) FlushAccounts() error {
	return s.persistRuntimeAccounts()
}

// RotateAccountTokens updates an account using its previous access token as
// the lookup key. Keeping this operation in Store avoids a race between a
// refresh-token rotation and another concurrent account update.
func (s *Store) RotateAccountTokens(oldToken, newToken, refreshToken, idToken string, fields map[string]any) (map[string]any, []map[string]any, error) {
	updated, items, _, err := s.rotateAccountTokens(oldToken, newToken, refreshToken, idToken, fields, nil)
	return updated, items, err
}

// RotateAccountTokensIfCredentials is the credential-generation guarded form
// of token rotation. If another request has already rotated this account, the
// stale OAuth result is discarded and current account data is returned.
func (s *Store) RotateAccountTokensIfCredentials(oldToken, newToken, refreshToken, idToken string, fields map[string]any, expected CredentialGeneration) (map[string]any, bool, error) {
	updated, _, applied, err := s.rotateAccountTokens(oldToken, newToken, refreshToken, idToken, fields, &expected)
	return updated, applied, err
}

func (s *Store) rotateAccountTokens(oldToken, newToken, refreshToken, idToken string, fields map[string]any, expected *CredentialGeneration) (map[string]any, []map[string]any, bool, error) {
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
	updates["next_token_refresh_at"] = nil
	updates["image_quota_pending_confirmation"] = nil
	updates["invalid_count"] = 0
	updates["cooldown_until"] = nil
	updates["next_retry_at"] = nil
	for _, key := range []string{
		"last_refresh_error", "last_refresh_error_at", "last_refresh_warning",
		"last_refresh_warning_at", "last_error_kind", "last_error_status",
		"last_error_message", "last_error_at", "status_reason_code",
	} {
		updates[key] = nil
	}

	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadAccountsLocked(); err != nil {
		return nil, nil, false, err
	}
	resolved := s.resolveAccountTokenLocked(oldToken)
	index, ok := s.accountsIndex[resolved]
	if !ok || index < 0 || index >= len(s.accountsCache) {
		return nil, cloneAccountList(s.accountsCache), false, os.ErrNotExist
	}
	if expected != nil && credentialGeneration(resolved, s.accountsCache[index]) != *expected {
		return cloneMap(s.accountsCache[index]), cloneAccountList(s.accountsCache), false, nil
	}
	aliasSources := s.tokenAliasSourcesLocked(resolved)
	accounts, next, ok := s.updatedAccountsLocked(resolved, updates)
	if !ok {
		return nil, cloneAccountList(s.accountsCache), false, os.ErrNotExist
	}
	if err := writeJSON(s.accountsPath, accounts); err != nil {
		return nil, nil, false, err
	}
	s.replaceAccountsLocked(accounts)
	activeToken := accountToken(next)
	if activeToken != "" && activeToken != resolved {
		s.moveAccountTokenAliasesLocked(activeToken, aliasSources)
	}
	s.accountsDirty = false
	result := cloneMap(next)
	for key, value := range updates {
		result[key] = value
	}
	return result, cloneAccountList(accounts), true, nil
}

func (s *Store) loadAccountsLocked() error {
	if s.accountsLoaded {
		return nil
	}
	items, err := loadList(s.accountsPath)
	if err != nil {
		return err
	}
	s.accountsCache = cloneAccountList(items)
	s.accountsIndex = accountIndex(s.accountsCache)
	s.accountsLoaded = true
	s.accountsRevision++
	return nil
}

func (s *Store) replaceAccountsLocked(items []map[string]any) {
	s.accountsCache = items
	s.accountsIndex = accountIndex(items)
	s.accountsLoaded = true
	s.accountsRevision++
	s.pruneTokenAliasesLocked()
}

func credentialAccessToken(token string, account map[string]any) string {
	if active := accountToken(account); active != "" {
		return active
	}
	return strings.TrimSpace(token)
}

// CredentialGenerationForAccount captures the credential generation represented
// by an account snapshot. Pool leases retain it so stale request feedback cannot
// modify health state after an OAuth rotation.
func CredentialGenerationForAccount(token string, account map[string]any) CredentialGeneration {
	return CredentialGeneration{
		AccessToken:        credentialAccessToken(token, account),
		RefreshToken:       strings.TrimSpace(stringValue(account["refresh_token"])),
		LastTokenRefreshAt: strings.TrimSpace(stringValue(account["last_token_refresh_at"])),
	}
}

func credentialGeneration(token string, account map[string]any) CredentialGeneration {
	return CredentialGenerationForAccount(token, account)
}

func (s *Store) resolveAccountTokenLocked(token string) string {
	current := strings.TrimSpace(token)
	seen := map[string]struct{}{}
	for current != "" {
		if _, ok := s.accountsIndex[current]; ok {
			return current
		}
		next, ok := s.tokenAliases[current]
		if !ok {
			break
		}
		if _, loop := seen[current]; loop {
			break
		}
		seen[current] = struct{}{}
		current = next
	}
	return current
}

func (s *Store) tokenAliasSourcesLocked(token string) map[string]struct{} {
	sources := map[string]struct{}{token: {}}
	for source := range s.tokenAliases {
		if s.resolveAccountTokenLocked(source) == token {
			sources[source] = struct{}{}
		}
	}
	return sources
}

func (s *Store) moveAccountTokenAliasesLocked(newToken string, sources map[string]struct{}) {
	if s.tokenAliases == nil {
		s.tokenAliases = map[string]string{}
	}
	for source := range sources {
		if source != newToken {
			s.tokenAliases[source] = newToken
		}
	}
	s.pruneTokenAliasesLocked()
}

func (s *Store) removeAccountTokenAliasesLocked(tokens map[string]struct{}) {
	for source, target := range s.tokenAliases {
		if _, removed := tokens[source]; removed {
			delete(s.tokenAliases, source)
			continue
		}
		if _, removed := tokens[target]; removed {
			delete(s.tokenAliases, source)
		}
	}
	s.pruneTokenAliasesLocked()
}

func (s *Store) pruneTokenAliasesLocked() {
	if len(s.tokenAliases) == 0 {
		return
	}
	compacted := make(map[string]string, len(s.tokenAliases))
	for source := range s.tokenAliases {
		current := source
		seen := map[string]struct{}{}
		for {
			if _, ok := s.accountsIndex[current]; ok {
				break
			}
			next, ok := s.tokenAliases[current]
			if !ok {
				current = ""
				break
			}
			if _, loop := seen[current]; loop {
				current = ""
				break
			}
			seen[current] = struct{}{}
			current = next
		}
		if current != "" && current != source {
			compacted[source] = current
		}
	}
	s.tokenAliases = compacted
}

func (s *Store) updatedAccountsLocked(token string, updates map[string]any) ([]map[string]any, map[string]any, bool) {
	token = s.resolveAccountTokenLocked(token)
	index, ok := s.accountsIndex[strings.TrimSpace(token)]
	if !ok || index < 0 || index >= len(s.accountsCache) {
		return nil, nil, false
	}
	accounts := append([]map[string]any(nil), s.accountsCache...)
	next := cloneMap(accounts[index])
	for key, value := range updates {
		next[key] = value
	}
	next = normalizeAccountMapJSON(next)
	accounts[index] = next
	return accounts, next, true
}

func (s *Store) scheduleAccountFlushLocked(delay time.Duration) {
	if delay <= 0 {
		delay = accountRuntimeFlushDelay
	}
	if s.accountsTimer != nil {
		return
	}
	s.accountsTimer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		s.accountsTimer = nil
		s.mu.Unlock()
		_ = s.persistRuntimeAccounts()
	})
}

func (s *Store) persistRuntimeAccounts() error {
	s.accountsWriteMu.Lock()
	defer s.accountsWriteMu.Unlock()

	s.mu.RLock()
	if !s.accountsLoaded || !s.accountsDirty {
		s.mu.RUnlock()
		return nil
	}
	items := s.accountsCache
	revision := s.accountsRevision
	s.mu.RUnlock()

	if err := writeJSON(s.accountsPath, items); err != nil {
		return err
	}
	s.mu.Lock()
	if s.accountsRevision == revision {
		s.accountsDirty = false
	}
	s.mu.Unlock()
	return nil
}

func accountIndex(items []map[string]any) map[string]int {
	index := make(map[string]int, len(items))
	for i, item := range items {
		if token := accountToken(item); token != "" {
			index[token] = i
		}
	}
	return index
}

func cloneAccountList(items []map[string]any) []map[string]any {
	cloned := make([]map[string]any, len(items))
	for i, item := range items {
		cloned[i] = cloneMap(item)
	}
	return cloned
}

func normalizeAccountMapJSON(item map[string]any) map[string]any {
	raw, err := json.Marshal(item)
	if err != nil {
		return cloneMap(item)
	}
	var normalized map[string]any
	if json.Unmarshal(raw, &normalized) != nil {
		return cloneMap(item)
	}
	return normalized
}

func normalizeAccountListJSON(items []map[string]any) []map[string]any {
	normalized := make([]map[string]any, len(items))
	for i, item := range items {
		normalized[i] = normalizeAccountMapJSON(item)
	}
	return normalized
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
	for _, key := range []string{"access_token", "accessToken", "token", "sso", "session_token"} {
		if token := stringValue(item[key]); token != "" {
			return normalizeToken(token)
		}
	}
	return ""
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
