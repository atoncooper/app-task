// Package router wires the Gin engine and HTTP handlers.
//
// File layout (one concern per file):
//
//	router.go   - engine assembly: New / Router / routes
//	task.go      — task endpoints (/tasks/*) + shared helpers
//	complete.go — async callback from third-party executors
//	script.go   — Lua script management (/scripts*)
package router

import (
	"fmt"
	"log/slog"
	"net/http"

	"app-task/internal/auth"
	"app-task/internal/config"
	"app-task/internal/executor"
	"app-task/internal/logger"
	"app-task/internal/repo"
	"app-task/internal/router/middleware"
	"app-task/internal/security"
	"app-task/internal/service"

	"app-task/internal/cluster"

	"github.com/gin-gonic/gin"
)

func New(taskSvc *service.TaskService, emailSvc *service.EmailService, notifySvc *service.NotifyService, llm *service.LLMClient, luaExec *executor.LuaExecutor, cfg *config.Config) *gin.Engine {
	if !cfg.App.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	e := gin.New()
	// Trust no proxy: the webui login throttle keys on c.ClientIP(), and gin's
	// default (trust everything) lets any client spoof X-Forwarded-For to
	// sidestep it. With no trusted proxy the peer address is always used.
	// Side effect: audit-log source_ip for APISIX-routed calls shows the
	// apisix container IP instead of the end user - accepted tradeoff.
	_ = e.SetTrustedProxies(nil)
	e.Use(logger.GinLogger())
	e.Use(logger.GinRecovery())
	e.Use(middleware.SecurityHeaders())
	e.Use(middleware.CORS(cfg.Security.CORS.AllowOrigins))

	if cfg.WebUI.Enabled && cfg.WebUI.Token == "" {
		slog.Warn("webui master token not set (optional API key); console login is " +
			"username/password — the default admin/app-task-admin account is active, " +
			"change its password in the console account page")
	}

	// Central secret store + notify channel configs: AES-256-GCM with the
	// shared encryption key. Without it ctx.secret fails with a clear message
	// (feature disabled) and notify channel entities are disabled (inline
	// email notifications keep working).
	var cipher *security.Cipher
	if cfg.Security.SecretEncKey != "" {
		if c, cerr := security.NewCipher(cfg.Security.SecretEncKey); cerr == nil {
			cipher = c
		} else {
			slog.Error("secret encryption key invalid", "err", cerr)
		}
	}
	if cipher != nil {
		luaExec.Secrets = func(name string) (string, error) {
			row, err := repo.GetSecretByName(name)
			if err != nil {
				return "", err
			}
			if row == nil {
				return "", fmt.Errorf("secret not found: %s", name)
			}
			return cipher.Decrypt(row.ValueEnc)
		}
	}
	// Console/auth backing stores: memory by default (single instance, zero
	// dependencies); Redis when webui.session_store=redis (multi-instance
	// deployments share sessions, key reveals and throttles). Store selection
	// and failure semantics live in the auth package.
	stores := auth.NewStateStores(cfg)
	r := &Router{taskSvc: taskSvc, emailSvc: emailSvc, notifySvc: notifySvc, scriptGen: service.NewScriptGenService(llm), luaExec: luaExec, cfg: cfg,
		cipher: cipher, keys: auth.NewKeyService(stores.Throttle, stores.Limiter, stores.Reveals)}
	if cfg.WebUI.Enabled {
		// Console authenticator shares the session store; its login throttle
		// stays per-process memory (nil → memory default, pre-existing).
		r.consoleAuth = auth.NewWebUIAuthenticator(cfg.WebUI.Token, cfg.WebUI.SessionTTLMinutes, stores.Sessions, nil)
	}
	r.registerRoutes(e)
	return e
}

// Service-body caps (boundary hardening): the JSON bodies accepted by the
// key-authenticated service endpoints are bounded regardless of caller.
const (
	maxTaskPayloadBytes = 64 << 10  // /tasks/register + /internal/script/run payload
	maxEmailBodyBytes   = 256 << 10 // /internal/email/send (HTML bodies can be sizable)
	maxListLimit        = 200       // /tasks list page size cap (防撑爆: unbounded rows starve the pool)
)

type Router struct {
	taskSvc     *service.TaskService
	emailSvc    *service.EmailService
	notifySvc   *service.NotifyService    // nil in tests — channel test-send then 500s
	scriptGen   *service.ScriptGenService // nil when AI unconfigured — endpoint 503s
	luaExec     *executor.LuaExecutor
	cfg         *config.Config
	cipher      *security.Cipher    // nil without SECURITY__API_KEY_ENCRYPTION_KEY
	keys        *auth.KeyService    // service-surface key auth (verify/throttle/reveal)
	consoleAuth *auth.Authenticator // nil when webui.enabled=false
}

// schedulerStats is wired from main (SetSchedulerStats) so /api/stats can
// expose the dispatch pool gauges without threading the scheduler through
// the Router constructor. Nil when unset (tests) — stats simply omit it.
var schedulerStats func() (workers int, inflight int64)

// SetSchedulerStats attaches the running scheduler's pool gauges.
func SetSchedulerStats(fn func() (workers int, inflight int64)) { schedulerStats = fn }

// clusterMgr is wired from main (SetClusterManager) for the /api/cluster
// roster endpoint. Nil when unset — the endpoint then returns an empty roster.
var clusterMgr *cluster.Manager

// SetClusterManager attaches the cluster node roster.
func SetClusterManager(m *cluster.Manager) { clusterMgr = m }

// nodeIdentity is this instance's scheduler id (SetNodeIdentity from main);
// on-demand script runs record it as the executing node (traceability).
var nodeIdentity string

// SetNodeIdentity attaches the scheduler's instance id.
func SetNodeIdentity(id string) { nodeIdentity = id }

func (r *Router) registerRoutes(e *gin.Engine) {
	e.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "healthy", "service": "app-task"})
	})

	// Admin console (single binary): server-rendered html/template pages
	// (POST-redirect-GET + flash) + the /api/* JSON group below. Disabled
	// entirely (pages + API) when webui.enabled=false.
	if r.cfg.WebUI.Enabled {
		r.registerPages(e, r.consoleAuth)
		r.registerWebuiRoutes(e, r.cfg, r.consoleAuth)
	}

	// Bootstrap service keys (e.g. the APISIX consumer key) so gateway-routed
	// calls pass the API-key middleware with no manual setup.
	auth.SeedBootstrapKeys(r.cfg.Security.ServiceKeys)

	// Backfill UUID identifiers for rows created before the uuid columns
	// existed (webui_user / secret legacy rows).
	if err := repo.EnsureUserIDs(); err != nil {
		slog.Error("[BACKFILL] user ids failed", "err", err)
	}
	if err := repo.EnsureSecretIDs(); err != nil {
		slog.Error("[BACKFILL] secret ids failed", "err", err)
	}

	// Service surface: every call must carry a valid API key. The gateway's
	// key-auth only guards the APISIX hop — this middleware is the authority
	// at the app-task port itself, so a direct connection cannot bypass it.
	svc := e.Group("", middleware.APIKeyAuth(r.keys, r.consoleAuth))

	// Task endpoints: register / detail / list. A task is a pure scheduling
	// definition (task_type + payload + executor_url + cron + retry + weight);
	// the scheduler dispatches it to a third-party executor and records the
	// outcome in task_log.
	tasks := svc.Group("/tasks")
	tasks.POST("/register", r.register)
	tasks.GET("/:task_id", r.detail)
	tasks.GET("", r.list)

	// Async callback: a third-party executor that accepted a task (202) reports
	// the final outcome here (key-auth). running -> completed | failed.
	svc.POST("/internal/task/:task_id/complete", r.completeTask)

	// Mail delivery (platform capability): a third-party executor posts a
	// standardized email (to/cc/subject/html) here; app-task queues + delivers
	// it with retries (key-auth).
	svc.POST("/internal/email/send", r.sendEmail)

	// Script run (platform capability): on-demand execution of a registered
	// Lua script (script_id + payload), synchronous with the result JSON.
	// Same pipeline as scheduled dispatch; runs are persisted to script_run.
	svc.POST("/internal/script/run", r.runScriptAPI)

	// Lua scripts (optional built-in executor, xxl-task GLUE-style): upload =
	// new version, takes effect immediately. Auth via APISIX key-auth; every
	// change is audit-logged.
	svc.POST("/scripts", r.uploadScript)
	svc.GET("/scripts", r.listScripts)
	svc.GET("/scripts/logs", r.scriptLogs)
}
