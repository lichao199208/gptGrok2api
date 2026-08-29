package httpapi

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/oauth"
)

func (s *Server) grokProbeScheduler() {
	for {
		config := s.registerStore.GrokProbeSchedulerStatus()
		if !boolValue(config["enabled"], false) {
			select {
			case <-s.probeStop:
				return
			case <-s.probeWake:
			}
			continue
		}
		interval := intValue(config["interval_minutes"])
		if interval < 1 {
			interval = 60
		}
		last := stringValue(config["last_finished_at"])
		due := last == ""
		if parsed, err := time.Parse(time.RFC3339, last); err == nil {
			due = time.Since(parsed) >= time.Duration(interval)*time.Minute
		}
		if due {
			s.runGrokProbeCycle()
		}
		next := time.Now().UTC().Add(time.Duration(interval) * time.Minute).Format(time.RFC3339)
		_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, next, stringValue(config["last_finished_at"]), "")
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-s.probeStop:
			timer.Stop()
			return
		case <-s.probeWake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Server) runGrokProbeCycle() {
	_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(true, "", "", "")
	items, err := s.oauthStore.List()
	if err != nil {
		_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, "", time.Now().UTC().Format(time.RFC3339), err.Error())
		return
	}
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, account := range items {
		account := account
		if strings.EqualFold(account.Status, "disabled") || account.AccessToken == "" {
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); sem <- struct{}{}; defer func() { <-sem }(); s.probeOAuthAccount(account) }()
	}
	wg.Wait()
	_ = s.registerStore.UpdateGrokProbeSchedulerRuntime(false, "", time.Now().UTC().Format(time.RFC3339), "")
}

func (s *Server) probeOAuthAccount(account oauth.Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result := s.xaiProbe.Probe(ctx, map[string]any{"access_token": account.AccessToken, "refresh_token": account.RefreshToken, "id_token": account.IDToken})
	probe := map[string]any{"status": result.Status, "model": "grok-4.5", "http_status": result.HTTPStatus, "code": result.Code, "error": result.Error, "quota": result.Quota, "probed_at": time.Now().UTC().Format(time.RFC3339)}
	status := account.Status
	if result.Status == "valid" {
		status = "active"
	} else if result.Status == "invalid" {
		status = "invalid"
	} else if result.Status == "limited" {
		status = "limited"
	}
	_, _, _ = s.oauthStore.UpdateProbe(account.ID, result.AccessToken, result.RefreshToken, result.IDToken, probe, result.Error)
	if status != account.Status && status != "" {
		_, _ = s.oauthStore.SetStatus([]string{account.ID}, status)
	}
}
