// Idempotency: HTTP-level dedup for /internal/* write endpoints. A caller
// that retries blind (its own timeout, its own at-least-once policy) sends
// the same Idempotency-Key; the first response is replayed from cache for
// the TTL instead of executing the side effect again.
package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"app-task/internal/auth"

	"github.com/gin-gonic/gin"
)

// maxCachedBody caps the response body kept per key: /internal/* write
// endpoints answer with small JSON; a bigger body means something unexpected
// and is simply not cached (the request still executes normally).
const maxCachedBody = 1 << 20

// maxKeys bounds the store; on overflow expired entries are swept, and a
// still-full store resets (losing dedup precision beats growing unbounded).
const maxKeys = 1 << 14

// idempotencyRecord is the cached response (or an in-flight placeholder).
type idempotencyRecord struct {
	status      int
	contentType string
	body        []byte
	expiry      time.Time
	inFlight    bool
}

// idempotencyStore maps scope|key → record with lazy expiry sweeping.
type idempotencyStore struct {
	mu      sync.Mutex
	records map[string]idempotencyRecord
	ttl     time.Duration
}

func newIdempotencyStore(ttl time.Duration) *idempotencyStore {
	return &idempotencyStore{records: make(map[string]idempotencyRecord), ttl: ttl}
}

// get returns a live (non-expired) record; expired entries are dropped on
// sight.
func (s *idempotencyStore) get(scopeKey string) (idempotencyRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[scopeKey]
	if !ok {
		return idempotencyRecord{}, false
	}
	if time.Now().After(r.expiry) {
		delete(s.records, scopeKey)
		return idempotencyRecord{}, false
	}
	return r, true
}

// put stores a record (final response or in-flight placeholder) with a
// fresh expiry; when the map is at cap, expired entries are swept first and
// a still-full store resets.
func (s *idempotencyStore) put(scopeKey string, r idempotencyRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.expiry = time.Now().Add(s.ttl)
	if len(s.records) >= maxKeys {
		now := time.Now()
		for k, v := range s.records {
			if now.After(v.expiry) {
				delete(s.records, k)
			}
		}
		if len(s.records) >= maxKeys {
			s.records = make(map[string]idempotencyRecord)
		}
	}
	s.records[scopeKey] = r
}

// drop removes a record (used to release the in-flight placeholder when the
// outcome must not be cached).
func (s *idempotencyStore) drop(scopeKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, scopeKey)
}

// captureWriter buffers the handler response so it can be replayed later.
type captureWriter struct {
	gin.ResponseWriter
	buf *bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

// Idempotency dedups write requests carrying an Idempotency-Key header for
// ttl. ttl <= 0 disables the gate (passthrough). Keys are scoped by the
// presented API credential name, so two callers sharing a key string never
// collide. Only 2xx responses are cached — a 5xx is a transient failure and
// the caller's retry must execute for real. Replayed responses carry the
// Idempotency-Replayed: true header. Concurrent duplicates while the first
// execution is still in flight get 409 Conflict; the client retries and the
// response then replays from cache.
func Idempotency(ttl time.Duration) gin.HandlerFunc {
	if ttl <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	st := newIdempotencyStore(ttl)
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
		if key == "" {
			c.Next()
			return
		}
		if len(key) > 200 {
			RespondError(c, http.StatusBadRequest, "invalid_request", "idempotency key too long (max 200)")
			c.Abort()
			return
		}
		scopeKey := auth.CurrentAPIKeyName(c) + "|" + key
		if rec, ok := st.get(scopeKey); ok {
			if rec.inFlight {
				RespondError(c, http.StatusConflict, "conflict",
					"request with this idempotency key is still in flight")
				c.Abort()
				return
			}
			slog.Info("[IDEMPOTENCY] replaying cached response",
				"scope", auth.CurrentAPIKeyName(c), "key_len", len(key))
			c.Header("Idempotency-Replayed", "true")
			c.Data(rec.status, rec.contentType, rec.body)
			c.Abort()
			return
		}
		st.put(scopeKey, idempotencyRecord{inFlight: true}) // reserve the slot

		// A handler panic unwinds past the recording code below, leaving the
		// in-flight placeholder stuck until TTL (retries 409 the whole time).
		// Release the slot and re-panic so the upstream GinRecovery keeps its
		// behaviour unchanged.
		defer func() {
			if r := recover(); r != nil {
				st.drop(scopeKey)
				panic(r)
			}
		}()

		cw := &captureWriter{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = cw
		c.Next()

		status := cw.Status()
		if status >= 200 && status < 300 && cw.buf.Len() <= maxCachedBody {
			st.put(scopeKey, idempotencyRecord{
				status:      status,
				contentType: cw.Header().Get("Content-Type"),
				body:        cw.buf.Bytes(),
			})
			return
		}
		// Non-2xx or oversized: release the placeholder so the caller's
		// retry executes for real.
		st.drop(scopeKey)
	}
}
