package agentidentity

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	dataPath    string
	keyPath     string
	registerURL string
	client      *http.Client
	mu          sync.Mutex
}

type record struct {
	AccountID  string `json:"account_id"`
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	PlanType   string `json:"plan_type"`
	RuntimeID  string `json:"agent_runtime_id"`
	PrivateKey string `json:"private_key"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

func NewStore(dataDir, registerURL string, client *http.Client) *Store {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Store{dataPath: filepath.Join(dataDir, "openai_agent_identities.json.enc"), keyPath: filepath.Join(dataDir, "openai_agent_identities.key"), registerURL: strings.TrimSpace(registerURL), client: client}
}

func (s *Store) Ensure(ctx context.Context, account map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	access := value(account, "access_token", "accessToken")
	if access == "" {
		return nil, errors.New("本地账号缺少 Access Token，无法创建 Agent Identity")
	}
	claims, err := jwtPayload(access)
	if err != nil {
		return nil, errors.New("本地账号 Access Token 不是有效 JWT")
	}
	auth := nested(claims, "https://api.openai.com/auth")
	profile := nested(claims, "https://api.openai.com/profile")
	accountID := first(value(account, "account_id", "chatgpt_account_id"), value(auth, "chatgpt_account_id"), value(claims, "sub"))
	userID := first(value(account, "user_id", "chatgpt_user_id"), value(auth, "chatgpt_user_id", "user_id"), value(claims, "sub"))
	if accountID == "" || userID == "" {
		return nil, errors.New("本地账号 JWT 缺少 ChatGPT 账号标识")
	}
	items, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.AccountID == accountID {
			return s.public(item), nil
		}
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	keyB64 := base64.StdEncoding.EncodeToString(privateKey)
	sshKey := sshPublicKey(publicKey)
	body := map[string]any{"abom": map[string]string{"agent_version": "0.138.0-alpha.6", "agent_harness_id": "codex-cli", "running_location": "local"}, "agent_public_key": sshKey}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.registerURL, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OpenAI Agent Identity 注册请求失败: %w", err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload map[string]any
	_ = json.Unmarshal(responseBody, &payload)
	runtimeID := value(payload, "agent_runtime_id")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || runtimeID == "" {
		detail := value(payload, "error", "message", "errorCode")
		if detail == "" {
			detail = strings.TrimSpace(string(responseBody)[:min(len(responseBody), 200)])
		}
		return nil, fmt.Errorf("OpenAI Agent Identity 注册失败（HTTP %d）: %s", resp.StatusCode, detail)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	item := record{AccountID: accountID, UserID: userID, Email: first(value(account, "email"), value(profile, "email")), PlanType: first(value(account, "plan_type"), value(auth, "chatgpt_plan_type"), "free"), RuntimeID: runtimeID, PrivateKey: keyB64, CreatedAt: now, UpdatedAt: now}
	items = append(items, item)
	if err := s.save(items); err != nil {
		return nil, err
	}
	return s.auth(item), nil
}

func (s *Store) Summary() ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, map[string]any{"account_id": item.AccountID, "user_id": item.UserID, "email": item.Email, "plan_type": item.PlanType, "agent_runtime_id": item.RuntimeID, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt})
	}
	return result, nil
}

func (s *Store) AuthJSON(accountID string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.AccountID == strings.TrimSpace(accountID) {
			return s.auth(item), nil
		}
	}
	return nil, os.ErrNotExist
}

func (s *Store) public(item record) map[string]any {
	return map[string]any{"auth_mode": "agent_identity", "agent_identity": map[string]any{"agent_runtime_id": item.RuntimeID, "account_id": item.AccountID, "chatgpt_user_id": item.UserID, "email": item.Email, "plan_type": item.PlanType, "chatgpt_account_is_fedramp": false}}
}
func (s *Store) auth(item record) map[string]any {
	result := s.public(item)
	identity := result["agent_identity"].(map[string]any)
	identity["agent_private_key"] = item.PrivateKey
	result["auth_mode"] = "agent_identity"
	return result
}

func (s *Store) load() ([]record, error) {
	raw, err := os.ReadFile(s.dataPath)
	if os.IsNotExist(err) {
		return []record{}, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(raw)
	if err != nil {
		return nil, err
	}
	var items []record
	if err := json.Unmarshal(plain, &items); err != nil {
		return nil, err
	}
	return items, nil
}
func (s *Store) save(items []record) error {
	if err := os.MkdirAll(filepath.Dir(s.dataPath), 0o700); err != nil {
		return err
	}
	plain, _ := json.Marshal(items)
	raw, err := s.encrypt(plain)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.dataPath), ".agent-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(raw)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, s.dataPath)
}
func (s *Store) cipher() (cipher.AEAD, error) {
	if err := os.MkdirAll(filepath.Dir(s.keyPath), 0o700); err != nil {
		return nil, err
	}
	key, err := os.ReadFile(s.keyPath)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		if err = os.WriteFile(s.keyPath, key, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func (s *Store) encrypt(plain []byte) ([]byte, error) {
	aead, err := s.cipher()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, nil), nil
}
func (s *Store) decrypt(raw []byte) ([]byte, error) {
	aead, err := s.cipher()
	if err != nil {
		return nil, err
	}
	if len(raw) < aead.NonceSize() {
		return nil, errors.New("invalid Agent Identity archive")
	}
	nonce, ciphertext := raw[:aead.NonceSize()], raw[aead.NonceSize():]
	return aead.Open(nil, nonce, ciphertext, nil)
}

func jwtPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}
func nested(value map[string]any, key string) map[string]any {
	result, _ := value[key].(map[string]any)
	return result
}
func value(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func sshPublicKey(key ed25519.PublicKey) string {
	header := []byte("ssh-ed25519")
	blob := make([]byte, 0, 4+len(header)+4+len(key))
	blob = append(blob, byte(0), byte(0), byte(0), byte(len(header)))
	blob = append(blob, header...)
	blob = append(blob, byte(0), byte(0), byte(0), byte(len(key)))
	blob = append(blob, key...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
}
