package register

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var ErrExecutorNotConfigured = errors.New("registration executor is not configured")

type RegistrationRequest struct {
	Target string
	Email  string
}

type RegistrationResult struct {
	Email  string
	SSO    string
	Status string
	Data   map[string]any
}

// MailProvider, CaptchaSolver and Registrar are deliberately small seams for
// production integrations. The default binary does not claim to solve CAPTCHAs
// or create third-party accounts until concrete providers are configured.
type MailProvider interface {
	RequestCode(context.Context, string) (string, error)
}

type CaptchaSolver interface {
	Solve(context.Context, string) (string, error)
}

type Registrar interface {
	Register(context.Context, RegistrationRequest, string, string) (RegistrationResult, error)
}

type Runtime struct {
	mu        sync.RWMutex
	Mail      MailProvider
	Captcha   CaptchaSolver
	Registrar Registrar
	running   bool
	started   time.Time
	lastErr   string
}

func NewRuntime() *Runtime { return &Runtime{} }

func (r *Runtime) SetDrivers(mail MailProvider, captcha CaptchaSolver, registrar Registrar) {
	r.mu.Lock()
	r.Mail, r.Captcha, r.Registrar = mail, captcha, registrar
	r.mu.Unlock()
}

func (r *Runtime) Ready(target string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !strings.EqualFold(strings.TrimSpace(target), "grok") && !strings.EqualFold(strings.TrimSpace(target), "openai") {
		return false
	}
	return r.Mail != nil && r.Captcha != nil && r.Registrar != nil
}

func (r *Runtime) Start(target string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ReadyLocked(target) {
		r.lastErr = ErrExecutorNotConfigured.Error()
		return ErrExecutorNotConfigured
	}
	r.running = true
	r.started = time.Now().UTC()
	r.lastErr = ""
	return nil
}

func (r *Runtime) ReadyLocked(target string) bool {
	if !strings.EqualFold(strings.TrimSpace(target), "grok") && !strings.EqualFold(strings.TrimSpace(target), "openai") {
		return false
	}
	return r.Mail != nil && r.Captcha != nil && r.Registrar != nil
}

func (r *Runtime) Stop() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

func (r *Runtime) Execute(ctx context.Context, request RegistrationRequest) (RegistrationResult, error) {
	r.mu.RLock()
	mail, captcha, registrar := r.Mail, r.Captcha, r.Registrar
	running := r.running
	r.mu.RUnlock()
	if !running || mail == nil || captcha == nil || registrar == nil {
		return RegistrationResult{}, ErrExecutorNotConfigured
	}
	code, err := mail.RequestCode(ctx, request.Email)
	if err != nil {
		return RegistrationResult{}, err
	}
	challenge, err := captcha.Solve(ctx, request.Target)
	if err != nil {
		return RegistrationResult{}, err
	}
	return registrar.Register(ctx, request, code, challenge)
}

func (r *Runtime) Status(target string) map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := map[string]any{
		"target":              target,
		"running":             r.running,
		"ready":               r.ReadyLocked(target),
		"mail_provider":       r.Mail != nil,
		"captcha_solver":      r.Captcha != nil,
		"registration_driver": r.Registrar != nil,
		"last_error":          r.lastErr,
	}
	if !r.started.IsZero() {
		result["started_at"] = r.started
	}
	return result
}
