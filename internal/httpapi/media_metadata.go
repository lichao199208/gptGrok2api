package httpapi

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var generatedMediaMetaMu sync.Mutex

func (s *Server) recordGeneratedMedia(ctx context.Context, result map[string]string) {
	rawURL := strings.TrimSpace(result["url"])
	if rawURL == "" {
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return
	}
	id := strings.TrimSpace(parsed.Query().Get("id"))
	if id == "" {
		id = strings.TrimSuffix(filepath.Base(parsed.Path), filepath.Ext(parsed.Path))
	}
	entries, _ := os.ReadDir(s.cfg.ImageDataDir)
	filename := ""
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), id+".") && !strings.HasSuffix(entry.Name(), ".meta.json") {
			filename = entry.Name()
			break
		}
	}
	if filename == "" {
		return
	}
	callID, _ := ctx.Value(monitorCallIDKey{}).(string)
	endpoint, model := "", ""
	if callID != "" {
		if record, ok := s.monitor.detail(callID); ok {
			endpoint = record.Endpoint
			model = record.Model
		}
	}
	meta := map[string]any{"call_id": callID, "endpoint": endpoint, "model": model, "generated_at": time.Now().UTC().Format(time.RFC3339), "source_type": "generated_output", "role": "output"}
	b, _ := json.Marshal(meta)
	generatedMediaMetaMu.Lock()
	defer generatedMediaMetaMu.Unlock()
	_ = os.WriteFile(filepath.Join(s.cfg.ImageDataDir, filename+".meta.json"), b, 0o600)
}

func mediaMetadata(path string) map[string]any {
	b, err := os.ReadFile(path + ".meta.json")
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
