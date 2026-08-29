package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

func (s *Server) survivalSnapshot() map[string]any {
	config := s.registerStore.Get()
	survival := mapValue(config["openai_survival"])
	if len(survival) == 0 {
		survival = map[string]any{"enabled": true, "interval_minutes": 60, "concurrency": 4, "refresh_codex_rt": true}
	}
	s.survivalMu.RLock()
	status := cloneMap(s.survivalStatus)
	s.survivalMu.RUnlock()
	for key, value := range survival {
		status[key] = value
	}
	return status
}

func (s *Server) runOpenAISurvival(w http.ResponseWriter) {
	s.survivalMu.Lock()
	if s.survivalRunning {
		s.survivalMu.Unlock()
		writeJSON(w, 200, map[string]any{"started": false, "survival": s.survivalSnapshot()})
		return
	}
	s.survivalRunning = true
	s.survivalStatus["running"] = true
	s.survivalStatus["last_started_at"] = time.Now().UTC().Format(time.RFC3339)
	s.survivalStatus["last_error"] = ""
	s.survivalMu.Unlock()
	go s.executeOpenAISurvival()
	writeJSON(w, 202, map[string]any{"started": true, "survival": s.survivalSnapshot()})
}

func (s *Server) executeOpenAISurvival() {
	config := mapValue(s.registerStore.Get()["openai_survival"])
	concurrency := intValue(config["concurrency"])
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}
	refreshFirst := boolValue(config["refresh_codex_rt"], true)
	items, err := s.store.AccountList()
	if err != nil {
		s.finishSurvival(map[string]any{"error": err.Error()})
		return
	}
	var mu sync.Mutex
	counts := map[string]int{}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, account := range items {
		account := account
		if strings.TrimSpace(accountToken(account)) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status := s.survivalProbe(account, refreshFirst)
			mu.Lock()
			counts[status]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	confirmed := 0
	errorsCount := 0
	for status, count := range counts {
		if isHealthyOpenAIPlan(status) {
			confirmed += count
		}
		if status == "error" || status == "token_dead" || status == "no_token" {
			errorsCount += count
		}
	}
	s.finishSurvival(map[string]any{"total": sumCounts(counts), "confirmed": confirmed, "errors": errorsCount, "statuses": counts})
}

func (s *Server) openAISurvivalScheduler() {
	// Match the Python service's startup grace period so account imports and
	// configuration updates settle before the first remote probe.
	timer := time.NewTimer(60 * time.Second)
	select {
	case <-timer.C:
	case <-s.survivalWake:
		timer.Stop()
	}
	for {
		config := mapValue(s.registerStore.Get()["openai_survival"])
		enabled := boolValue(config["enabled"], true)
		interval := intValue(config["interval_minutes"])
		if interval < 15 {
			interval = 60
		}
		if enabled {
			last := ""
			s.survivalMu.RLock()
			last = stringValue(s.survivalStatus["last_finished_at"])
			s.survivalMu.RUnlock()
			due := last == ""
			if parsed, err := time.Parse(time.RFC3339, last); err == nil {
				due = time.Since(parsed) >= time.Duration(interval)*time.Minute
			}
			if due {
				s.runOpenAISurvivalInternal()
			}
			next := time.Now().UTC().Add(time.Duration(interval) * time.Minute).Format(time.RFC3339)
			s.survivalMu.Lock()
			s.survivalStatus["next_run_at"] = next
			s.survivalMu.Unlock()
		}
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-s.survivalWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Server) runOpenAISurvivalInternal() {
	s.survivalMu.Lock()
	if s.survivalRunning {
		s.survivalMu.Unlock()
		return
	}
	s.survivalRunning = true
	s.survivalStatus["running"] = true
	s.survivalStatus["last_started_at"] = time.Now().UTC().Format(time.RFC3339)
	s.survivalStatus["last_error"] = ""
	s.survivalMu.Unlock()
	go s.executeOpenAISurvival()
}

func (s *Server) survivalProbe(account map[string]any, refreshFirst bool) string {
	token := accountToken(account)
	if token == "" {
		return "no_token"
	}
	active := account
	refreshed := false
	if refreshFirst && !strings.EqualFold(stringValue(account["source_type"]), "chatgpt_web") && stringValue(account["refresh_token"]) != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		result, err := s.openAIAccountClient().RefreshAccount(ctx, account)
		cancel()
		if err == nil {
			updated, _, updateErr := s.store.RotateAccountTokens(token, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields)
			if updateErr == nil && updated != nil {
				active = updated
				refreshed = true
			}
		}
	}
	if refreshed {
		if status := strings.ToLower(firstNonEmpty(stringValue(active["type"]), "free")); isHealthyOpenAIPlan(status) {
			_, _, _ = s.store.UpdateAccount(accountToken(active), map[string]any{"survival_status": status, "survival_last_probe_status": status, "survival_last_checked_at": time.Now().UTC().Format(time.RFC3339), "survival_alive": true, "survival_plan_type": status, "survival_check_error": nil})
			return status
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := s.openAIAccountClient().RefreshAccount(ctx, active)
	if err != nil {
		status := "error"
		if strings.Contains(strings.ToLower(err.Error()), "invalid") || strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
			status = "token_dead"
		}
		_, _, _ = s.store.UpdateAccount(accountToken(active), map[string]any{"survival_status": status, "survival_last_probe_status": status, "survival_last_checked_at": time.Now().UTC().Format(time.RFC3339), "survival_check_error": safeRefreshError(err)})
		return status
	}
	updated, _, updateErr := s.store.RotateAccountTokens(accountToken(active), result.AccessToken, result.RefreshToken, result.IDToken, map[string]any{"survival_status": firstNonEmpty(stringValue(result.Fields["type"]), "free"), "survival_last_probe_status": firstNonEmpty(stringValue(result.Fields["type"]), "free"), "survival_last_checked_at": time.Now().UTC().Format(time.RFC3339), "survival_alive": true, "survival_plan_type": firstNonEmpty(stringValue(result.Fields["type"]), "free"), "survival_check_error": nil})
	if updateErr != nil || updated == nil {
		return "error"
	}
	return firstNonEmpty(strings.ToLower(stringValue(result.Fields["type"])), "free")
}

func (s *Server) finishSurvival(summary map[string]any) {
	s.survivalMu.Lock()
	defer s.survivalMu.Unlock()
	s.survivalRunning = false
	s.survivalStatus["running"] = false
	s.survivalStatus["last_finished_at"] = time.Now().UTC().Format(time.RFC3339)
	if value, ok := summary["error"]; ok {
		s.survivalStatus["last_error"] = value
	} else {
		s.survivalStatus["last_error"] = ""
		s.survivalStatus["last_summary"] = summary
	}
}

func isHealthyOpenAIPlan(status string) bool {
	switch status {
	case "free", "plus", "pro", "team", "k12":
		return true
	}
	return false
}
func sumCounts(values map[string]int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}
