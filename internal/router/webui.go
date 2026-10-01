// Package router: admin console API for the embedded web UI.
//
// The webui is served by the same Gin process (single binary, see web.go);
// /api/* endpoints back it. Unlike the APISIX-facing /tasks/* endpoints (which
// trust the injected X-Uid), these are admin endpoints that see ALL users'
// data, so they are gated by the configured webui token (see config.WebUI).
package router

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"app-task/internal/auth"
	"app-task/internal/config"
	"app-task/internal/dto"
	"app-task/internal/model"
	"app-task/internal/repo"
	"app-task/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// webuiVersion is the service version. It is a var (not const) so release
// pipelines can inject the git tag at link time:
//
//	-X app-task/internal/router.webuiVersion=<tag>
var webuiVersion = "0.12.0"

// Version is the exported service version (CLI `--version` / `at info`).
var Version = webuiVersion

// registerWebuiRoutes mounts the admin API behind the auth gate. The console
// ALWAYS requires login (a default admin account is seeded at startup), so the
// gate is unconditional; webui.enabled=false disables the whole console.
func (r *Router) registerWebuiRoutes(e *gin.Engine, cfg *config.Config, authr *auth.Authenticator) {
	// Login/logout sit OUTSIDE the gate: login must be reachable without
	// credentials (it IS the credential check), and logout authenticates with
	// the very session it invalidates.
	e.POST("/api/login", authr.Login)
	e.POST("/api/logout", authr.Logout)

	api := e.Group("/api", authr.Middleware())
	api.GET("/info", r.apiInfo)
	api.GET("/stats", r.apiStats)

	api.GET("/cluster", auth.RequireAdmin(), r.apiCluster)
	api.POST("/cluster/join", auth.RequireAdmin(), r.apiClusterJoin)

	api.GET("/tasks", r.apiListTasks)
	api.GET("/tasks/:task_id", r.apiTaskDetail)
	api.POST("/tasks", r.apiCreateTask)

	api.GET("/logs", r.apiListLogs)

	api.GET("/emails", r.apiListEmails)
	api.POST("/emails/:email_id/retry", r.apiRetryEmail)

	// Notify channels (admin): CLI surface for the console's channel roster.
	channels := api.Group("/channels", auth.RequireAdmin())
	channels.GET("", r.apiListChannels)
	channels.POST("", r.apiUpsertChannel)
	channels.DELETE("/:name", r.apiDeleteChannel)
	channels.POST("/:name/test", r.apiTestChannel)

	// Business calendars (admin): CLI surface for holiday/adjustment dates.
	calendarAPI := api.Group("/calendars", auth.RequireAdmin())
	calendarAPI.GET("", r.apiListCalendars)
	calendarAPI.POST("", r.apiCreateCalendar)
	calendarAPI.POST("/dates", r.apiUpsertCalendarDate)
	calendarAPI.DELETE("/:name", r.apiDeleteCalendar)

	api.GET("/scripts", r.apiListScripts)
	api.GET("/scripts/:script_id", r.apiScriptDetail)
	api.POST("/scripts", r.apiCreateScript)
	api.POST("/scripts/:script_id/toggle", r.apiToggleScript)

	// Account management: admin-only.
	users := api.Group("/users", auth.RequireAdmin())
	users.GET("", r.apiListUsers)
	users.POST("", r.apiCreateUser)
	users.POST("/:user_id/password", r.apiSetUserPassword)
	users.DELETE("/:user_id", r.apiDeleteUser)
}

// ── info / stats ────────────────────────────────────────────────────

func (r *Router) apiInfo(c *gin.Context) {
	out := gin.H{"service": "app-task", "version": webuiVersion, "status": "running"}
	if s, ok := auth.CurrentUser(c); ok {
		out["user"] = gin.H{"username": s.Username, "role": s.Role, "is_admin": s.Role == "admin"}
	}
	c.JSON(http.StatusOK, out)
}

// apiStats aggregates dashboard counters: tasks by status, execution log
// total, email queue by status, script count.
func (r *Router) apiStats(c *gin.Context) {
	tasks, err := repo.CountTasksByStatus()
	if err != nil {
		tasks = map[string]int64{}
	}
	taskTotal, _ := repo.CountTasks("")
	logsTotal, _ := repo.CountTaskLogs()
	emails, _ := repo.CountEmailsByStatus()
	if emails == nil {
		emails = map[string]int64{}
	}
	emailTotal, _ := repo.CountEmails("")
	scripts, _ := repo.CountScripts()

	out := gin.H{
		"service":    gin.H{"status": "running", "version": webuiVersion},
		"tasks":      withTotal(tasks, taskTotal),
		"logs_total": logsTotal,
		"emails":     withTotal(emails, emailTotal),
		"scripts":    scripts,
		"now":        time.Now().Format(time.RFC3339),
	}
	if schedulerStats != nil {
		workers, inflight := schedulerStats()
		out["scheduler"] = gin.H{"workers": workers, "inflight": inflight}
	}
	c.JSON(http.StatusOK, out)
}

// apiCluster renders the node roster: liveness, versions, in-flight claims.
func (r *Router) apiCluster(c *gin.Context) {
	if clusterMgr == nil {
		c.JSON(http.StatusOK, gin.H{"nodes": []gin.H{}})
		return
	}
	nodes, err := clusterMgr.Nodes(time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"nodes": nodes})
}

func withTotal(m map[string]int64, total int64) gin.H {
	out := gin.H{}
	for k, v := range m {
		out[k] = v
	}
	out["total"] = total
	return out
}

// ── tasks ───────────────────────────────────────────────────────────

func (r *Router) apiListTasks(c *gin.Context) {
	limit, offset := pagination(c, 50, 200)
	status := c.Query("status")
	tasks, err := repo.ListAllTasks(status, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	total, _ := repo.CountTasks(status)
	out := make([]gin.H, 0, len(tasks))
	for _, j := range tasks {
		out = append(out, taskView(&j))
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "limit": limit, "offset": offset, "tasks": out})
}

func (r *Router) apiTaskDetail(c *gin.Context) {
	taskID := c.Param("task_id")
	task, err := repo.GetTaskByID(taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": "task not found"})
		return
	}
	logs, _ := repo.ListTaskLogs(taskID, 50)
	logOut := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		logOut = append(logOut, taskLogView(&l))
	}
	c.JSON(http.StatusOK, gin.H{"task": taskView(task), "logs": logOut})
}

// apiCreateTask registers a task from the admin console. An omitted uid (0)
// attributes the task to the logged-in console user's own id; master-token
// sessions (no user row) and explicit uids pass through unchanged.
func (r *Router) apiCreateTask(c *gin.Context) {
	var req dto.AdminCreateTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	if req.UID == 0 {
		req.UID = sessionUID(c)
	}
	var triggerTime time.Time
	if req.CronExpr == "" && req.TriggerTime == "" {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "trigger_time required when cron_expr is empty"})
		return
	}
	if req.TriggerTime != "" {
		t, err := parseISO8601(req.TriggerTime)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid trigger_time (expect ISO8601)"})
			return
		}
		triggerTime = t
	}
	taskID, err := r.taskSvc.RegisterTask(service.RegisterOptions{
		UID: req.UID, TaskType: req.TaskType, Payload: req.Payload, ExecutorURL: req.ExecutorURL,
		Async: req.Async, CronExpr: req.CronExpr, TriggerTime: triggerTime,
		MaxRetry: req.MaxRetry, Weight: req.Weight, Shard: req.Shard, ShardTotal: req.ShardTotal,
		CalendarID: req.CalendarID, AlertChannel: req.AlertChannel,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "register failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"task_id": taskID, "status": "pending"})
}

// taskView maps a Task row to the admin console JSON shape.
func taskView(j *model.Task) gin.H {
	return gin.H{
		"task_id":           j.TaskID,
		"uid":               j.UID,
		"task_type":         j.TaskType,
		"status":            j.Status,
		"owner":             j.Owner,
		"claimed_at":        timePtr(j.ClaimedAt),
		"trigger_time":      j.TriggerTime.Format(time.RFC3339),
		"executor_url":      j.ExecutorURL,
		"async":             j.Async,
		"cron_expr":         j.CronExpr,
		"cron_next_task_id": j.CronNextTaskID,
		"max_retry":         j.MaxRetry,
		"retry_count":       j.RetryCount,
		"next_retry_at":     timePtr(j.NextRetryAt),
		"last_result":       j.LastResult,
		"weight":            j.Weight,
		"payload":           payloadView(j.Payload),
		"created_at":        j.CreatedAt.Format(time.RFC3339),
		"updated_at":        j.UpdatedAt.Format(time.RFC3339),
	}
}

// ── execution logs ──────────────────────────────────────────────────

func (r *Router) apiListLogs(c *gin.Context) {
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	logs, err := repo.ListRecentLogs(c.Query("task_id"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		out = append(out, taskLogView(&l))
	}
	c.JSON(http.StatusOK, gin.H{"logs": out})
}

func taskLogView(l *model.TaskLog) gin.H {
	return gin.H{
		"log_id":      l.LogID,
		"task_id":     l.TaskID,
		"trigger_at":  l.TriggerAt.Format(time.RFC3339),
		"executor":    l.Executor,
		"status":      l.Status,
		"duration_ms": l.DurationMS,
		"response":    l.Response,
		"error":       l.Error,
	}
}

// ── email queue ─────────────────────────────────────────────────────

func (r *Router) apiListEmails(c *gin.Context) {
	limit, offset := pagination(c, 50, 200)
	status := c.Query("status")
	emails, err := repo.ListEmails(status, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	total, _ := repo.CountEmails(status)
	out := make([]gin.H, 0, len(emails))
	for _, e := range emails {
		out = append(out, emailView(&e))
	}
	c.JSON(http.StatusOK, gin.H{"total": total, "limit": limit, "offset": offset, "emails": out})
}

func (r *Router) apiRetryEmail(c *gin.Context) {
	emailID := c.Param("email_id")
	ok, err := repo.ResetEmailForRetry(emailID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"detail": "email not in failed state (or not found)"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"email_id": emailID, "status": "pending"})
}

func emailView(e *model.EmailMessage) gin.H {
	return gin.H{
		"email_id":      e.EmailID,
		"to":            toStringSlice(e.To),
		"cc":            toStringSlice(e.CC),
		"subject":       e.Subject,
		"reference_id":  e.ReferenceID,
		"status":        e.Status,
		"retry_count":   e.RetryCount,
		"next_retry_at": timePtr(e.NextRetryAt),
		"last_error":    e.LastError,
		"created_at":    e.CreatedAt.Format(time.RFC3339),
		"sent_at":       timePtr(e.SentAt),
	}
}

// ── lua scripts ─────────────────────────────────────────────────────

func (r *Router) apiListScripts(c *gin.Context) {
	scripts, err := repo.ListScripts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(scripts))
	for _, s := range scripts {
		out = append(out, gin.H{
			"script_id":   s.ScriptID,
			"name":        s.Name,
			"description": s.Description,
			"version":     s.Version,
			"enabled":     s.Enabled,
			"updated_at":  s.UpdatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"scripts": out})
}

func (r *Router) apiScriptDetail(c *gin.Context) {
	scriptID := c.Param("script_id")
	latest, err := repo.GetLatestScript(scriptID)
	if err == repo.ErrScriptNotFound {
		c.JSON(http.StatusNotFound, gin.H{"detail": "script not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	versions, _ := repo.ListScriptVersions(scriptID)
	verOut := make([]gin.H, 0, len(versions))
	for _, v := range versions {
		verOut = append(verOut, gin.H{
			"version":    v.Version,
			"name":       v.Name,
			"enabled":    v.Enabled,
			"source":     v.Source,
			"updated_at": v.UpdatedAt.Format(time.RFC3339),
		})
	}
	audit, _ := repo.ListScriptLogs(scriptID, 50)
	auditOut := make([]gin.H, 0, len(audit))
	for _, l := range audit {
		auditOut = append(auditOut, gin.H{
			"log_id":     l.LogID,
			"version":    l.Version,
			"action":     l.Action,
			"operator":   l.Operator,
			"source_ip":  l.SourceIP,
			"request_id": l.RequestID,
			"summary":    l.Summary,
			"created_at": l.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"script_id":   latest.ScriptID,
		"name":        latest.Name,
		"description": latest.Description,
		"version":     latest.Version,
		"enabled":     latest.Enabled,
		"source":      latest.Source,
		"updated_at":  latest.UpdatedAt.Format(time.RFC3339),
		"versions":    verOut,
		"logs":        auditOut,
	})
}

func (r *Router) apiCreateScript(c *gin.Context) {
	var req dto.ScriptUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	version, logID, err := r.applyScriptUpload(req, c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "script rejected: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"script_id": req.ScriptID, "version": version, "log_id": logID, "status": "ok"})
}

// apiToggleScript flips the enabled flag on the latest version without
// creating a new version; every change is audit-logged.
func (r *Router) apiToggleScript(c *gin.Context) {
	scriptID := c.Param("script_id")
	var req dto.ToggleScriptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	latest, err := repo.GetLatestScript(scriptID)
	if err == repo.ErrScriptNotFound {
		c.JSON(http.StatusNotFound, gin.H{"detail": "script not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if err := repo.UpdateScriptEnabled(scriptID, req.Enabled); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	operator := req.Operator
	if operator == "" {
		operator = c.GetHeader("X-Operator")
	}
	if operator == "" {
		operator = auth.OperatorOf(c)
	}
	state := "enabled"
	if !req.Enabled {
		state = "disabled"
	}
	_ = repo.CreateScriptLog(&model.ScriptLog{
		LogID:     uuid.NewString(),
		ScriptID:  scriptID,
		Version:   latest.Version,
		Action:    "toggle",
		Operator:  operator,
		SourceIP:  c.ClientIP(),
		RequestID: c.GetHeader("X-Request-Id"),
		Summary:   "v" + strconv.Itoa(latest.Version) + " " + state,
	})
	c.JSON(http.StatusOK, gin.H{"script_id": scriptID, "version": latest.Version, "enabled": req.Enabled})
}

// ── webui users (admin only) ───────────────────────────────────────

func (r *Router) apiListUsers(c *gin.Context) {
	us, err := repo.ListUsers()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	out := make([]gin.H, 0, len(us))
	for _, u := range us {
		out = append(out, gin.H{
			"id":         u.ID,
			"user_id":    u.UserID,
			"username":   u.Username,
			"role":       u.Role,
			"created_at": u.CreatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"users": out})
}

func (r *Router) apiCreateUser(c *gin.Context) {
	var req dto.CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	if req.Role == "" {
		req.Role = "member"
	}
	if req.Role != "admin" && req.Role != "member" {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "role must be admin or member"})
		return
	}
	u, err := repo.CreateUser(req.Username, req.Password, req.Role)
	if err == repo.ErrUserExists {
		c.JSON(http.StatusConflict, gin.H{"detail": "username already exists"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": "create user failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": u.ID, "user_id": u.UserID, "username": u.Username, "role": u.Role})
}

func (r *Router) apiSetUserPassword(c *gin.Context) {
	uid, err := resolveUserID(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": err.Error()})
		return
	}
	var req dto.SetUserPasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	if err := repo.SetUserPassword(uid, req.Password); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (r *Router) apiDeleteUser(c *gin.Context) {
	uid, err := resolveUserID(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": err.Error()})
		return
	}
	me, ok := auth.CurrentUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"detail": "unauthorized"})
		return
	}
	if me.UserID == c.Param("user_id") {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "cannot delete your own account"})
		return
	}
	target, err := repo.GetUserByID(uid)
	if err != nil || target == nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": "user not found"})
		return
	}
	if target.Role == "admin" {
		admins, _ := repo.CountUsersByRole("admin")
		if admins <= 1 {
			c.JSON(http.StatusBadRequest, gin.H{"detail": "cannot delete the last admin"})
			return
		}
	}
	if err := repo.DeleteUser(uid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── shared helpers ──────────────────────────────────────────────────

func pagination(c *gin.Context, def, max int) (limit, offset int) {
	limit = def
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= max {
			limit = n
		}
	}
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offset = n
		}
	}
	return limit, offset
}

func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}

// payloadView renders the opaque task payload for display: pretty-printed
// JSON when possible, raw string otherwise.
func payloadView(p datatypes.JSON) any {
	if len(p) == 0 {
		return nil
	}
	var pretty any
	if err := json.Unmarshal(p, &pretty); err == nil {
		return pretty
	}
	return string(p)
}

// toStringSlice decodes a JSON-encoded string array (email to/cc columns).
func toStringSlice(j datatypes.JSON) []string {
	var ss []string
	if len(j) > 0 {
		_ = json.Unmarshal(j, &ss)
	}
	return ss
}

// resolveUserID resolves a user_id route parameter (UUID) to the internal
// row id used by the repo helpers. Unknown identifiers → error.
func resolveUserID(userID string) (int64, error) {
	u, err := repo.GetUserByUserID(userID)
	if err != nil {
		return 0, err
	}
	if u == nil {
		return 0, errors.New("user not found")
	}
	return u.ID, nil
}
