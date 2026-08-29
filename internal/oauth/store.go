package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Account struct {
	ID           string         `json:"id"`
	Email        string         `json:"email,omitempty"`
	Subject      string         `json:"subject,omitempty"`
	AccessToken  string         `json:"access_token,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	IDToken      string         `json:"id_token,omitempty"`
	ExpiresAt    int64          `json:"expires_at,omitempty"`
	Status       string         `json:"status"`
	SourceType   string         `json:"source_type"`
	Models       []string       `json:"models,omitempty"`
	CreatedAt    int64          `json:"created_at"`
	UpdatedAt    int64          `json:"updated_at"`
	LastError    string         `json:"last_error,omitempty"`
	Probe        map[string]any `json:"probe,omitempty"`
}

type Store struct {
	path string
	key  []byte
	mu   sync.Mutex
}

func NewStore(path, secret string) *Store {
	sum := sha256.Sum256([]byte(secret))
	return &Store{path: path, key: sum[:]}
}

func (s *Store) List() ([]Account, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.load() }

func (s *Store) PublicList() ([]map[string]any, error) {
	items, err := s.List()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, public(item))
	}
	return result, nil
}

func (s *Store) Import(item Account) (Account, error) {
	if strings.TrimSpace(item.AccessToken) == "" && strings.TrimSpace(item.RefreshToken) == "" {
		return Account{}, errors.New("access_token or refresh_token is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return Account{}, err
	}
	if item.ID == "" {
		item.ID = randomID()
	}
	if item.Status == "" {
		item.Status = "active"
	}
	if item.SourceType == "" {
		item.SourceType = "oauth_import"
	}
	now := time.Now().Unix()
	item.CreatedAt = now
	item.UpdatedAt = now
	updated := false
	for index, current := range items {
		if current.ID == item.ID || (item.Subject != "" && current.Subject == item.Subject) || (item.Email != "" && strings.EqualFold(current.Email, item.Email)) {
			item.CreatedAt = current.CreatedAt
			items[index] = item
			updated = true
			break
		}
	}
	if !updated {
		items = append(items, item)
	}
	if err := s.save(items); err != nil {
		return Account{}, err
	}
	return item, nil
}

func (s *Store) SetDisabled(ids []string, disabled bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return 0, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = true
	}
	count := 0
	for index, item := range items {
		if set[item.ID] {
			if disabled {
				item.Status = "disabled"
			} else {
				item.Status = "active"
			}
			item.UpdatedAt = time.Now().Unix()
			items[index] = item
			count++
		}
	}
	if count > 0 {
		err = s.save(items)
	}
	return count, err
}

func (s *Store) Delete(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return 0, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[strings.TrimSpace(id)] = true
	}
	next := items[:0]
	count := 0
	for _, item := range items {
		if set[item.ID] {
			count++
			continue
		}
		next = append(next, item)
	}
	if count > 0 {
		err = s.save(next)
	}
	return count, err
}

// UpdateProbe records non-secret probe telemetry and optional rotated tokens.
// The public projection never includes the credential values themselves.
func (s *Store) UpdateProbe(id, accessToken, refreshToken, idToken string, probe map[string]any, lastError string) (Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return Account{}, false, err
	}
	for index, item := range items {
		if item.ID != strings.TrimSpace(id) {
			continue
		}
		if strings.TrimSpace(accessToken) != "" {
			item.AccessToken = strings.TrimSpace(accessToken)
		}
		if strings.TrimSpace(refreshToken) != "" {
			item.RefreshToken = strings.TrimSpace(refreshToken)
		}
		if strings.TrimSpace(idToken) != "" {
			item.IDToken = strings.TrimSpace(idToken)
		}
		item.Probe = cloneProbe(probe)
		item.LastError = strings.TrimSpace(lastError)
		item.UpdatedAt = time.Now().Unix()
		items[index] = item
		if err := s.save(items); err != nil {
			return Account{}, false, err
		}
		return item, true, nil
	}
	return Account{}, false, nil
}

func (s *Store) SetStatus(ids []string, status string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return 0, err
	}
	targets := map[string]bool{}
	for _, id := range ids {
		if clean := strings.TrimSpace(id); clean != "" {
			targets[clean] = true
		}
	}
	updated := 0
	for index, item := range items {
		if !targets[item.ID] {
			continue
		}
		item.Status = strings.TrimSpace(status)
		item.UpdatedAt = time.Now().Unix()
		items[index] = item
		updated++
	}
	if updated > 0 {
		err = s.save(items)
	}
	return updated, err
}

func (s *Store) load() ([]Account, error) {
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return []Account{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []Account{}, nil
	}
	plain, err := s.decrypt(raw)
	if err != nil {
		return nil, err
	}
	var items []Account
	if err := json.Unmarshal(plain, &items); err != nil {
		return nil, err
	}
	return items, nil
}
func (s *Store) save(items []Account) error {
	raw, _ := json.MarshalIndent(items, "", "  ")
	encrypted, err := s.encrypt(raw)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".oauth-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(encrypted); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.path)
}
func (s *Store) encrypt(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plain, nil)
	return append(nonce, sealed...), nil
}
func (s *Store) decrypt(raw []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("invalid oauth store")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
}
func public(item Account) map[string]any {
	return map[string]any{"id": item.ID, "email": item.Email, "subject": item.Subject, "has_access_token": item.AccessToken != "", "has_refresh_token": item.RefreshToken != "", "expires_at": item.ExpiresAt, "status": item.Status, "source_type": item.SourceType, "models": item.Models, "probe": cloneProbe(item.Probe), "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "last_error": item.LastError}
}

func cloneProbe(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	result := map[string]any{}
	for key, value := range input {
		result[key] = value
	}
	return result
}
func randomID() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return "oauth_" + base64.RawURLEncoding.EncodeToString(raw)
}
