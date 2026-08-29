package proxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type upstreamEntry struct {
	URL          string
	Healthy      bool
	LastChecked  time.Time
	LastOK       time.Time
	LastError    string
	FailCount    int
	pendingProbe bool
}

// UpstreamRouter manages a file-backed list of outbound proxy upstreams.
// It keeps the hot path cheap by caching file contents and health state.
type UpstreamRouter struct {
	mu             sync.Mutex
	path           string
	entries        []upstreamEntry
	cursor         int
	lastLoad       time.Time
	lastModTime    time.Time
	refreshEvery   time.Duration
	probeEvery     time.Duration
	probeTimeout   time.Duration
	probeURL       string
	probeTransport *Transport
}

func NewUpstreamRouter(path string) *UpstreamRouter {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	return &UpstreamRouter{
		path:           path,
		refreshEvery:   20 * time.Second,
		probeEvery:     60 * time.Second,
		probeTimeout:   8 * time.Second,
		probeURL:       "https://chatgpt.com/api/auth/csrf",
		probeTransport: NewTransport(http.DefaultTransport),
	}
}

func (r *UpstreamRouter) Resolve() string {
	if r == nil {
		return ""
	}
	r.refresh()

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 0 {
		return ""
	}

	now := time.Now()
	start := r.cursor % len(r.entries)
	candidate := -1
	for i := 0; i < len(r.entries); i++ {
		idx := (start + i) % len(r.entries)
		entry := r.entries[idx]
		if entry.Healthy {
			candidate = idx
			break
		}
	}
	if candidate < 0 {
		candidate = start
	}
	r.cursor = (candidate + 1) % len(r.entries)
	entry := &r.entries[candidate]
	if r.shouldProbeLocked(entry, now) {
		entry.pendingProbe = true
		url := entry.URL
		go r.probe(url)
	}
	return entry.URL
}

func (r *UpstreamRouter) Snapshot() map[string]any {
	if r == nil {
		return map[string]any{"enabled": false, "count": 0, "healthy": 0}
	}
	r.refresh()
	r.mu.Lock()
	defer r.mu.Unlock()
	healthy := 0
	entries := make([]map[string]any, 0, len(r.entries))
	for _, entry := range r.entries {
		if entry.Healthy {
			healthy++
		}
		entries = append(entries, map[string]any{
			"url":           redactProxyURL(entry.URL),
			"healthy":       entry.Healthy,
			"last_checked":  entry.LastChecked.UTC(),
			"last_ok":       entry.LastOK.UTC(),
			"last_error":    entry.LastError,
			"fail_count":    entry.FailCount,
			"pending_probe": entry.pendingProbe,
		})
	}
	return map[string]any{
		"enabled":    true,
		"path":       r.path,
		"count":      len(r.entries),
		"healthy":    healthy,
		"last_load":  r.lastLoad.UTC(),
		"last_mtime": r.lastModTime.UTC(),
		"entries":    entries,
	}
}

func (r *UpstreamRouter) refresh() {
	if r == nil {
		return
	}
	info, err := os.Stat(r.path)
	if err != nil || info.IsDir() {
		r.mu.Lock()
		if len(r.entries) > 0 {
			r.entries = nil
		}
		r.mu.Unlock()
		return
	}
	modTime := info.ModTime()
	r.mu.Lock()
	shouldRefresh := r.entries == nil || len(r.entries) == 0 || modTime.After(r.lastModTime) || time.Since(r.lastLoad) >= r.refreshEvery
	r.mu.Unlock()
	if !shouldRefresh {
		return
	}
	raw, err := os.ReadFile(filepath.Clean(r.path))
	if err != nil {
		return
	}
	lines := strings.Split(string(raw), "\n")
	seen := map[string]struct{}{}
	nextEntries := make([]upstreamEntry, 0, len(lines))
	for _, line := range lines {
		candidate := strings.TrimSpace(line)
		if candidate == "" || strings.HasPrefix(candidate, "#") || strings.HasPrefix(candidate, "//") {
			continue
		}
		normalized := normalizeURL(candidate)
		if normalized == "" {
			continue
		}
		key := strings.ToLower(normalized)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		nextEntries = append(nextEntries, upstreamEntry{URL: normalized, Healthy: true})
	}
	r.mu.Lock()
	if len(nextEntries) > 0 {
		state := map[string]upstreamEntry{}
		for _, entry := range r.entries {
			state[strings.ToLower(entry.URL)] = entry
		}
		for index := range nextEntries {
			if existing, ok := state[strings.ToLower(nextEntries[index].URL)]; ok {
				nextEntries[index].Healthy = existing.Healthy
				nextEntries[index].LastChecked = existing.LastChecked
				nextEntries[index].LastOK = existing.LastOK
				nextEntries[index].LastError = existing.LastError
				nextEntries[index].FailCount = existing.FailCount
				nextEntries[index].pendingProbe = existing.pendingProbe
			}
		}
		r.entries = nextEntries
		if r.cursor >= len(r.entries) {
			r.cursor = 0
		}
	} else {
		r.entries = nil
		r.cursor = 0
	}
	r.lastLoad = time.Now()
	r.lastModTime = modTime
	r.mu.Unlock()
}

func (r *UpstreamRouter) shouldProbeLocked(entry *upstreamEntry, now time.Time) bool {
	if entry == nil || entry.pendingProbe {
		return false
	}
	if entry.LastChecked.IsZero() {
		return true
	}
	return now.Sub(entry.LastChecked) >= r.probeEvery
}

func (r *UpstreamRouter) probe(target string) {
	if r == nil || strings.TrimSpace(target) == "" {
		return
	}
	client := &http.Client{Transport: r.probeTransport, Timeout: r.probeTimeout}
	req, err := http.NewRequest(http.MethodGet, r.probeURL, nil)
	if err != nil {
		r.finishProbe(target, false, err.Error())
		return
	}
	req = req.WithContext(WithURL(req.Context(), target))
	resp, err := client.Do(req)
	if err != nil {
		r.finishProbe(target, false, err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		r.finishProbe(target, false, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	r.finishProbe(target, true, "")
}

func (r *UpstreamRouter) finishProbe(target string, healthy bool, errText string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lowered := strings.ToLower(strings.TrimSpace(target))
	for index := range r.entries {
		if strings.ToLower(r.entries[index].URL) != lowered {
			continue
		}
		r.entries[index].LastChecked = time.Now()
		r.entries[index].pendingProbe = false
		r.entries[index].Healthy = healthy
		if healthy {
			r.entries[index].LastOK = time.Now()
			r.entries[index].LastError = ""
			r.entries[index].FailCount = 0
		} else {
			r.entries[index].FailCount++
			r.entries[index].LastError = errText
		}
		return
	}
}

func (r *UpstreamRouter) markFailure(target, errText string) {
	r.finishProbe(target, false, errText)
}

func redactProxyURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, "://"); idx >= 0 {
		rest := value[idx+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return value[:idx+3] + "[REDACTED]@" + rest[at+1:]
		}
	}
	return value
}
