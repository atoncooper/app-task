package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// HTTPExecutor dispatches a task to a third-party executor over HTTP(S): it
// POSTs the opaque payload verbatim to task.ExecutorURL. The scheduler never
// interprets the payload — the executor is responsible for the business.
//
// The executor_url scheme may be http:// or https:// (TLS is transparent via
// net/http). For private CAs pass CAFile (PEM); for self-signed endpoints in
// trusted networks, InsecureSkipVerify opts out of certificate validation
// (explicitly — never set it in production).
//
// CA 惰性加载：CAFile 在首次派发时才读取，失败不缓存（下次派发重试），
// 而非构造时读取——纯调度栈没有挂 app-pay 的 CA 也能正常启动，
// app-pay 侧证书晚于 app-task 就绪时委托回调也能自愈。
//
// Execution model (per task):
//
//	sync  (async=false): 2xx response = success; other status = failure
//	async (async=true):  202 accepted → ErrAsync (task goes running, the
//	                     executor reports the outcome via the callback
//	                     endpoint /internal/task/{id}/complete)
type HTTPExecutor struct {
	opts HTTPOptions

	mu     sync.Mutex
	client *http.Client // nil = TLS 配置尚未成功构建（下次派发重试）

	breakersMu sync.Mutex
	breakers   map[string]*hostBreaker // lazily created per executor host
}

// HTTPOptions configures the HTTP executor transport.
type HTTPOptions struct {
	Timeout            time.Duration // per request timeout (default 30s)
	InsecureSkipVerify bool          // opt out of TLS cert validation (self-signed only)
	CAFile             string        // PEM file with a private CA to trust
	// Idle connections kept per executor host. Go's default (2) throttles
	// keep-alive reuse when the dispatch pool runs wider than that, forcing
	// reconnect churn on hot targets. 0 = default 32.
	IdleConnsPerHost int
	// Gateway-scoped auth: when an executor_url's host:port is listed in
	// GatewayHosts (e.g. the APISIX gateway fronting the main app), the
	// dispatch attaches GatewayHeader: GatewayApiKey so key-auth-protected
	// /internal/* callback endpoints accept the request. Hosts outside the
	// list (genuine third-party executors) NEVER receive the credential.
	// Empty GatewayApiKey (or empty set) = disabled (legacy behaviour).
	GatewayHosts  map[string]bool
	GatewayHeader string // header name; "" = "apikey" (APISIX key-auth default)
	GatewayApiKey string
	// Per-host circuit breaker: fast-fail dispatches to an executor host
	// after BreakerThreshold consecutive availability failures (network
	// errors / 5xx; a 4xx proves the target is alive and never counts), for
	// BreakerCooldownSeconds, then admit one half-open probe. Prevents one
	// dead executor from pinning every dispatch worker in timeout waits.
	// 0 = disabled (legacy behaviour).
	BreakerThreshold       int
	BreakerCooldownSeconds int // 0 = default 60s
}

// gatewayMatch reports whether the raw URL's host:port is in the allow set
// (case-insensitive). Unparseable URLs never match (dispatch fails later in
// validateExecutorURL anyway).
func gatewayMatch(rawURL string, hosts map[string]bool) bool {
	if len(hosts) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return hosts[strings.ToLower(u.Host)]
}

func NewHTTPExecutor(opts HTTPOptions) *HTTPExecutor {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.IdleConnsPerHost <= 0 {
		opts.IdleConnsPerHost = 32
	}
	if opts.BreakerCooldownSeconds <= 0 {
		opts.BreakerCooldownSeconds = 60
	}
	return &HTTPExecutor{opts: opts, breakers: map[string]*hostBreaker{}}
}

// breakerFor returns the per-host breaker, creating it on first use. Nil
// when the breaker is disabled (BreakerThreshold <= 0) — call sites skip
// the Allow/Record round-trips entirely then.
func (e *HTTPExecutor) breakerFor(host string) *hostBreaker {
	if e.opts.BreakerThreshold <= 0 {
		return nil
	}
	e.breakersMu.Lock()
	defer e.breakersMu.Unlock()
	if e.breakers == nil {
		e.breakers = map[string]*hostBreaker{}
	}
	b, ok := e.breakers[host]
	if !ok {
		b = newHostBreaker(e.opts.BreakerThreshold, time.Duration(e.opts.BreakerCooldownSeconds)*time.Second)
		e.breakers[host] = b
	}
	return b
}

// clientForTLS lazily builds the HTTP client. CA 文件读取/解析失败返回错误且
// 不缓存失败态，允许后续派发（证书补齐后）自愈。
func (e *HTTPExecutor) clientForTLS() (*http.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client, nil
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: e.opts.InsecureSkipVerify} //nolint:gosec // 显式配置项（自签名内网专用）
	if e.opts.CAFile != "" {
		pem, err := os.ReadFile(e.opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("no valid CA certificate in " + e.opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	e.client = &http.Client{
		Timeout: e.opts.Timeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			MaxIdleConnsPerHost: e.opts.IdleConnsPerHost,
			// Honor HTTP_PROXY/HTTPS_PROXY/NO_PROXY so dispatches to
			// third-party executors on the public internet can route through
			// a deployment's egress proxy (internal targets must be listed in
			// NO_PROXY to stay direct).
			Proxy: http.ProxyFromEnvironment,
		},
	}
	return e.client, nil
}

// Handler returns the executor.Handler for task_type="http" (the default).
func (e *HTTPExecutor) Handler() Handler {
	return func(ctx context.Context, task Task) error {
		// Executor target + async flag ride in the task meta (scheduler adapts
		// them from the persisted task definition).
		rawURL, _ := task.Meta["executor_url"].(string)
		async, _ := task.Meta["async"].(bool)
		if err := validateExecutorURL(rawURL); err != nil {
			return err
		}
		client, err := e.clientForTLS()
		if err != nil {
			// TLS 配置错误（CA 缺失/非法）：按 transient 处理交给重试策略，
			// 证书补齐后自愈；重试耗尽走任务失败 + 兜底路径。
			return fmt.Errorf("%w: executor TLS setup: %v", ErrRetry, err)
		}
		var body io.Reader
		if len(task.Payload) > 0 {
			body = bytes.NewReader(task.Payload)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, body)
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		// Identify the task to the executor: it needs task_id to report back via
		// the completion callback / internal/email/send reference_id.
		req.Header.Set("X-Task-Id", task.ID)
		// Gateway-scoped auth: only targets on the configured gateway host list
		// receive the credential (main-app /internal/* callbacks behind APISIX
		// key-auth). Third-party executors never see the key.
		if e.opts.GatewayApiKey != "" && gatewayMatch(rawURL, e.opts.GatewayHosts) {
			name := e.opts.GatewayHeader
			if name == "" {
				name = "apikey"
			}
			req.Header.Set(name, e.opts.GatewayApiKey)
		}
		// 分片广播：执行器按片处理数据的去重/定位键
		if v, ok := task.Meta["shard_index"]; ok {
			req.Header.Set("X-Shard-Index", fmt.Sprint(v))
			req.Header.Set("X-Shard-Total", fmt.Sprint(task.Meta["shard_total"]))
		}
		// Per-host circuit breaker (0 threshold = disabled): fast-fail when the
		// circuit is open instead of queueing another timeout wait. Checked
		// immediately before Do — every path past Allow flows through the
		// success/failure recording below, so a half-open probe slot can never
		// leak on an early return (TLS setup, request build).
		var br *hostBreaker
		if e.opts.BreakerThreshold > 0 {
			if u, perr := url.Parse(rawURL); perr == nil && u.Host != "" {
				br = e.breakerFor(strings.ToLower(u.Host))
				if ok, retryAfter := br.Allow(time.Now()); !ok {
					return fmt.Errorf("%w: circuit open for executor host %s, probe after %s",
						ErrRetry, u.Host, retryAfter.Truncate(time.Millisecond))
				}
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			if br != nil {
				br.RecordFailure(time.Now())
			}
			// Network-level failure (incl. TLS handshake): transient, let the
			// retry policy decide.
			return fmt.Errorf("%w: executor unreachable: %v", ErrRetry, err)
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

		// Async accepted takes precedence over the generic 2xx branch.
		if resp.StatusCode == http.StatusAccepted && async {
			if br != nil {
				br.RecordSuccess()
			}
			return ErrAsync // accepted → running, callback will finalize
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if br != nil {
				br.RecordSuccess()
			}
			return nil // sync success
		}
		// 5xx counts as unavailability; 4xx is a deterministic rejection from
		// a live server and must not trip the breaker.
		if br != nil && resp.StatusCode >= 500 {
			br.RecordFailure(time.Now())
		}
		return fmt.Errorf("executor returned %d: %s", resp.StatusCode, truncateBytes(respBody, 500))
	}
}

// validateExecutorURL restricts the executor target to http/https.
func validateExecutorURL(raw string) error {
	if raw == "" {
		return errors.New("http task missing executor_url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid executor_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported executor_url scheme %q (only http/https)", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("executor_url %q missing host", raw)
	}
	return nil
}

func truncateBytes(b []byte, n int) string {
	s := string(b)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
