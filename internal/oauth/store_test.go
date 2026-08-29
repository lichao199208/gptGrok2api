package oauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreEncryptsAndManagesAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.enc")
	store := NewStore(path, "test-secret")
	item, err := store.Import(Account{Email: "person@example.com", AccessToken: "access-secret", RefreshToken: "refresh-secret"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "access-secret") || strings.Contains(string(raw), "refresh-secret") {
		t.Fatal("OAuth token appeared in encrypted store")
	}
	items, err := store.List()
	if err != nil || len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("unexpected stored accounts: %#v %v", items, err)
	}
	if count, err := store.SetDisabled([]string{item.ID}, true); err != nil || count != 1 {
		t.Fatalf("disable failed: count=%d err=%v", count, err)
	}
	if count, err := store.Delete([]string{item.ID}); err != nil || count != 1 {
		t.Fatalf("delete failed: count=%d err=%v", count, err)
	}
	if _, err := NewStore(path, "wrong-secret").List(); err == nil {
		t.Fatal("wrong encryption secret unexpectedly decrypted store")
	}
}
