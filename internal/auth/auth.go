// Package auth: authentication and authorization services — API-key
// verification for the service surface, webui session authentication for the
// admin console, and the pluggable backing stores they share.
//
// The package sits at the service layer: it reads the api_key/webui_user
// tables via repo, never writes business state, and has no knowledge of
// route registration. HTTP concerns (header extraction, response shaping)
// live in internal/router/middleware; the gin-aware pieces here are the
// authenticator's own gate/handlers and the gin-context accessors for the
// credentials it stashes.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"app-task/internal/model"
	"app-task/internal/repo"

	"github.com/gin-gonic/gin"
)

const (
	apiKeyBytes = 32
	RevealTTL   = 5 * time.Minute
	ctxKeyName  = "api_key_name"
	ctxKeyUID   = "api_key_uid"
)

// Service-scope groups enforced by the middleware. A key's Scopes CSV must
// contain the group required by the request path; empty Scopes = all groups
// (legacy/bootstrap rows before scopes existed).
const (
	ScopeTasks    = "tasks"
	ScopeScripts  = "scripts"
	ScopeInternal = "internal"
)

// RequiredScope maps a request path to the scope group it belongs to.
func RequiredScope(path string) string {
	switch {
	case strings.HasPrefix(path, "/tasks"):
		return ScopeTasks
	case strings.HasPrefix(path, "/scripts"):
		return ScopeScripts
	case strings.HasPrefix(path, "/internal"):
		return ScopeInternal
	}
	return ""
}

// KeyHasScope reports whether the key's scope CSV grants the group. Empty CSV
// grants everything (rows created before scopes existed).
func KeyHasScope(scopes, required string) bool {
	if required == "" || scopes == "" {
		return true
	}
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == required {
			return true
		}
	}
	return false
}

// KeyService holds the auth throttle, the per-key rate limiter and the
// one-time reveal store — in-process memory by default, Redis-backed when
// webui.session_store=redis (see state_stores.go).
type KeyService struct {
	throttle throttler
	limiter  keyRateLimiter
	reveals  revealStore
}

// NewKeyService wires the shared backing stores into a key service. Nil
// inputs fall back to in-process memory implementations.
func NewKeyService(throttle throttler, limiter keyRateLimiter, reveals revealStore) *KeyService {
	if throttle == nil {
		throttle = newMemoryThrottler(webuiMaxFails, webuiFailWindow)
	}
	if limiter == nil {
		limiter = newMemoryLimiter()
	}
	if reveals == nil {
		reveals = newMemoryRevealStore()
	}
	return &KeyService{throttle: throttle, limiter: limiter, reveals: reveals}
}

// AllowRequest enforces the per-key rate cap (requests/minute).
// ratePerMin <= 0 means unlimited.
func (k *KeyService) AllowRequest(keyID string, ratePerMin int) bool {
	return k.limiter.allowRequest(keyID, ratePerMin)
}

// Allow / Fail / Reset mirror the console login throttle (10 failures per
// minute per client IP, shared by every protected service endpoint).
func (k *KeyService) Allow(ip string) bool { return k.throttle.allow(ip) }

func (k *KeyService) Fail(ip string) { k.throttle.fail(ip) }

func (k *KeyService) Reset(ip string) { k.throttle.reset(ip) }

// VerifyAPIKey resolves a presented plaintext key to its row: hash lookup,
// active status, not expired.
func (k *KeyService) VerifyAPIKey(plaintext string) (*model.APIKey, bool) {
	if plaintext == "" {
		return nil, false
	}
	row, err := repo.GetAPIKeyByHash(HashAPIKey(plaintext))
	if err != nil || row == nil {
		return nil, false
	}
	if row.Status != repo.APIKeyStatusActive {
		return nil, false
	}
	if row.ExpiresAt != nil && time.Now().After(*row.ExpiresAt) {
		return nil, false
	}
	return row, true
}

// PutReveal / TakeReveal implement the one-time plaintext reveal flow used by
// the console's key-generation page (shown exactly once, 5-minute TTL).
func (k *KeyService) PutReveal(plaintext string) string {
	return k.reveals.put(plaintext, RevealTTL)
}

func (k *KeyService) TakeReveal(token string) (string, bool) {
	return k.reveals.take(token, RevealTTL)
}

// ── key generation / hashing ────────────────────────────────────────

// HashAPIKey derives the stored digest of a presented key.
func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GenerateAPIKey mints a new credential: plaintext (shown once) + the row to
// persist (hash + display prefix).
func GenerateAPIKey() (plaintext string, row *model.APIKey, err error) {
	raw := make([]byte, apiKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	plaintext = "at_" + hex.EncodeToString(raw)
	row = &model.APIKey{
		KeyID:     NewKeyID(),
		KeyHash:   HashAPIKey(plaintext),
		KeyPrefix: plaintext[:12] + "…",
	}
	return plaintext, row, nil
}

// NewKeyID returns a fresh public identifier for an api_key row.
func NewKeyID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand failing means the process is broken anyway
		panic("auth: rand read failed: " + err.Error())
	}
	return hex.EncodeToString(raw)
}

// SeedBootstrapKeys seeds env-provided service keys (APISIX consumer key
// etc.) into the api_key table so gateway-routed calls pass the middleware
// with no manual setup. Runs once at startup, after DB init.
func SeedBootstrapKeys(serviceKeys []string) {
	for i, raw := range serviceKeys {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		name := "bootstrap-service-key"
		if len(serviceKeys) > 1 {
			name = "bootstrap-service-key-" + strconv.Itoa(i+1)
		}
		row := &model.APIKey{
			KeyID:     NewKeyID(),
			Name:      name,
			KeyHash:   HashAPIKey(strings.TrimSpace(raw)),
			KeyPrefix: strings.TrimSpace(raw)[:min(8, len(strings.TrimSpace(raw)))] + "…",
			Scopes:    ScopeTasks + "," + ScopeScripts + "," + ScopeInternal,
			Status:    repo.APIKeyStatusActive,
			CreatedBy: "bootstrap",
		}
		created, err := repo.UpsertAPIKey(row)
		if err != nil {
			slog.Error("[APIKEY] bootstrap key seed failed", "name", name, "err", err)
			continue
		}
		if !created {
			slog.Info("[APIKEY] bootstrap key row kept", "name", name)
		}
	}
	if len(serviceKeys) > 0 {
		slog.Info("[APIKEY] bootstrap service keys seeded", "count", len(serviceKeys))
	}
}

// ── gin-context accessors for the stashed credential ─────────────────

// CurrentAPIKeyName returns the credential name recorded by the middleware
// (audit metadata for script uploads etc.).
func CurrentAPIKeyName(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyName); ok {
		if name, ok := v.(string); ok {
			return name
		}
	}
	return ""
}

// BoundUID returns the uid pinned to the presented API key (0 = unbound:
// gateway keys trust the injected X-Uid header instead). Console-session and
// master-token callers never have a bound uid.
func BoundUID(c *gin.Context) (int64, bool) {
	if v, ok := c.Get(ctxKeyUID); ok {
		if uid, ok := v.(int64); ok && uid > 0 {
			return uid, true
		}
	}
	return 0, false
}

// SetCredential stashes the verified key identity on the request context.
func SetCredential(c *gin.Context, name string, uid int64) {
	c.Set(ctxKeyName, name)
	c.Set(ctxKeyUID, uid)
}
