package router

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"app-task/internal/auth"
	"app-task/internal/dto"
	"app-task/internal/repo"
	"app-task/internal/router/middleware"
	"app-task/internal/service"

	"github.com/gin-gonic/gin"
)

// resolveUID returns the effective owner uid for a request. A uid bound to
// the presented API key always wins; unbound keys (bootstrap/gateway) and
// console-session callers fall back to the client-supplied X-Uid header.
func resolveUID(c *gin.Context) (int64, bool) {
	if uid, ok := auth.BoundUID(c); ok {
		return uid, true
	}
	return uidFromHeader(c)
}

// register creates a pure scheduling task. The scheduler never interprets the
// payload — it is passed verbatim to the executor.
func (r *Router) register(c *gin.Context) {
	if c.Request.ContentLength > maxTaskPayloadBytes {
		middleware.RespondError(c, http.StatusBadRequest, "payload_too_large", "payload too large")
		return
	}
	var req dto.RegisterTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", "invalid request: "+err.Error())
		return
	}
	// Key-bound uid wins: a uid-pinned key may only register for its own uid.
	if bound, ok := auth.BoundUID(c); ok && bound != int64(req.UID) {
		middleware.RespondError(c, http.StatusForbidden, "forbidden",
			"task uid does not match the uid bound to the API key")
		return
	}
	var triggerTime time.Time
	if req.CronExpr == "" && req.TriggerTime == "" {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request",
			"trigger_time required when cron_expr is empty")
		return
	}
	if req.TriggerTime != "" {
		t, err := parseISO8601(req.TriggerTime)
		if err != nil {
			middleware.RespondError(c, http.StatusBadRequest, "invalid_trigger_time",
				"invalid trigger_time (expect ISO8601)")
			return
		}
		triggerTime = t
	}
	taskID, err := r.taskSvc.RegisterTask(service.RegisterOptions{
		UID: int64(req.UID), TaskType: req.TaskType, Payload: req.Payload, ExecutorURL: req.ExecutorURL,
		Async: req.Async, CronExpr: req.CronExpr, TriggerTime: triggerTime,
		MaxRetry: req.MaxRetry, Weight: req.Weight, Shard: req.Shard, ShardTotal: req.ShardTotal,
		CalendarID: req.CalendarID, AlertChannel: req.AlertChannel, Domain: strings.ToLower(strings.TrimSpace(req.Domain)),
	})
	if err != nil {
		if errors.Is(err, service.ErrInvalidExecutorURL) {
			middleware.RespondError(c, http.StatusBadRequest, "invalid_executor_url", err.Error())
			return
		}
		middleware.RespondError(c, http.StatusBadRequest, "register_failed", "register failed: "+err.Error())
		return
	}
	c.Header("Location", "/tasks/"+taskID)
	c.JSON(http.StatusCreated, gin.H{"task_id": taskID, "status": "pending"})
}

// detail returns a task plus its recent execution log (溯源).
func (r *Router) detail(c *gin.Context) {
	uid, ok := resolveUID(c)
	if !ok {
		return
	}
	taskID := c.Param("task_id")
	task, err := repo.GetTaskByID(taskID)
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if task == nil {
		middleware.RespondError(c, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if task.UID != uid {
		middleware.RespondError(c, http.StatusForbidden, "forbidden", "not the task owner")
		return
	}
	logs, _ := repo.ListTaskLogs(taskID, 10)
	logOut := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		logOut = append(logOut, gin.H{
			"trigger_at":  l.TriggerAt.Format(time.RFC3339),
			"executor":    l.Executor,
			"status":      l.Status,
			"duration_ms": l.DurationMS,
			"response":    l.Response,
			"error":       l.Error,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":      task.TaskID,
		"uid":          task.UID,
		"task_type":    task.TaskType,
		"status":       task.Status,
		"owner":        task.Owner,
		"trigger_time": task.TriggerTime.Format(time.RFC3339),
		"executor_url": task.ExecutorURL,
		"async":        task.Async,
		"domain":       task.Domain,
		"max_retry":    task.MaxRetry,
		"retry_count":  task.RetryCount,
		"last_result":  task.LastResult,
		"payload":      task.Payload,
		"logs":         logOut,
	})
}

// list returns the user's tasks, newest first.
func (r *Router) list(c *gin.Context) {
	uid, ok := resolveUID(c)
	if !ok {
		return
	}
	limit := 50
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			middleware.RespondError(c, http.StatusBadRequest, "invalid_request",
				"limit must be an integer in 1.."+strconv.Itoa(maxListLimit))
			return
		}
		limit = n
	}
	offset := 0
	if v := c.Query("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			middleware.RespondError(c, http.StatusBadRequest, "invalid_request",
				"offset must be a non-negative integer")
			return
		}
		offset = n
	}
	status := strings.TrimSpace(c.Query("status"))
	if status != "" && !validTaskStatuses[status] {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request",
			"status must be one of: pending, dispatching, running, completed, failed")
		return
	}
	// domain: optional cross-domain list scoping (e.g. "task_quiz" on the
	// scheduled-quiz page). Empty = unfiltered (all the uid's tasks).
	domain := strings.ToLower(strings.TrimSpace(c.Query("domain")))
	if err := service.ValidateDomain(domain); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	tasks, total, err := repo.ListTasksByUID(uid, status, domain, limit, offset)
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := make([]gin.H, 0, len(tasks))
	for _, j := range tasks {
		out = append(out, gin.H{
			"task_id":      j.TaskID,
			"task_type":    j.TaskType,
			"status":       j.Status,
			"trigger_time": j.TriggerTime.Format(time.RFC3339),
			"executor_url": j.ExecutorURL,
			"async":        j.Async,
			"domain":       j.Domain,
			// Opaque payload passed through for display (e.g. prompt); the
			// scheduler never interprets it.
			"payload": j.Payload,
		})
	}
	c.JSON(http.StatusOK, gin.H{"tasks": out, "total": total, "limit": limit, "offset": offset})
}

// deleteTask removes one task owned by the caller. Removable states are
// pending (cancel before trigger) and terminal (completed | failed cleanup);
// in-flight tasks (dispatching | running) are refused with 409 so an
// execution is never orphaned mid-flight. Foreign/missing tasks answer 404
// (no existence leak); task_log rows are kept as the audit trail.
func (r *Router) deleteTask(c *gin.Context) {
	uid, ok := resolveUID(c)
	if !ok {
		return
	}
	taskID := c.Param("task_id")
	task, err := repo.GetTaskByID(taskID)
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if task == nil {
		middleware.RespondError(c, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	if task.UID != uid {
		middleware.RespondError(c, http.StatusForbidden, "forbidden", "not the task owner")
		return
	}
	if task.Status == "dispatching" || task.Status == "running" {
		middleware.RespondError(c, http.StatusConflict, "task_in_flight",
			"task is in flight (dispatching/running) and cannot be deleted yet; retry once it settles")
		return
	}
	affected, err := repo.DeleteTaskForUID(taskID, uid)
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if affected == 0 {
		// Raced: the scheduler claimed the task between the check and the
		// delete. Re-read and answer on the fresh state.
		cur, _ := repo.GetTaskByID(taskID)
		if cur != nil && (cur.Status == "dispatching" || cur.Status == "running") {
			middleware.RespondError(c, http.StatusConflict, "task_in_flight",
				"task started executing; retry once it settles")
			return
		}
		middleware.RespondError(c, http.StatusNotFound, "task_not_found", "task not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"task_id": taskID, "deleted": true})
}

func uidFromHeader(c *gin.Context) (int64, bool) {
	s := c.GetHeader("X-Uid")
	if s == "" {
		middleware.RespondError(c, http.StatusUnauthorized, "unauthorized", "unauthorized (X-Uid missing)")
		return 0, false
	}
	uid, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		middleware.RespondError(c, http.StatusUnauthorized, "unauthorized", "invalid X-Uid")
		return 0, false
	}
	return uid, true
}

// validTaskStatuses is the closed set accepted by the list endpoint's status
// filter (mirrors the scheduler's state machine, repo/task.go).
var validTaskStatuses = map[string]bool{
	"pending": true, "dispatching": true, "running": true,
	"completed": true, "failed": true,
}

func parseISO8601(s string) (time.Time, error) {
	s = strings.Replace(s, "Z", "+00:00", 1)
	return time.Parse(time.RFC3339, s)
}
