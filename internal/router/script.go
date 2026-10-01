package router

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"app-task/internal/auth"
	"app-task/internal/dto"
	"app-task/internal/executor"
	"app-task/internal/model"
	"app-task/internal/repo"
	"app-task/internal/router/middleware"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (r *Router) uploadScript(c *gin.Context) {
	var req dto.ScriptUploadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", "invalid request: "+err.Error())
		return
	}
	// script_id is server-assigned when omitted (UUID); callers may supply
	// their own id to drive the upsert/update flow.
	req.ScriptID = strings.TrimSpace(req.ScriptID)
	if req.ScriptID == "" {
		req.ScriptID = uuid.NewString()
	}
	version, logID, err := r.applyScriptUpload(req, c)
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "script_rejected", "script rejected: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"script_id": req.ScriptID,
		"version":   version,
		"log_id":    logID,
		"status":    "ok",
	})
}

// applyScriptUpload compiles + persists a script version and writes the audit
// log entry. Shared by /scripts (key-auth) and /api/scripts (admin console).
func (r *Router) applyScriptUpload(req dto.ScriptUploadRequest, c *gin.Context) (version int, logID string, err error) {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Audit metadata: prefer explicit operator, fall back to header, then to
	// the authenticated webui user, then the API-key credential name; always
	// capture source IP + request id.
	operator := req.Operator
	if operator == "" {
		operator = c.GetHeader("X-Operator")
	}
	if operator == "" {
		operator = auth.OperatorOf(c)
	}
	if operator == "" {
		operator = auth.CurrentAPIKeyName(c)
	}
	requestID := c.GetHeader("X-Request-Id")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	sourceIP := c.ClientIP()

	version, err = r.luaExec.UpsertScript(executor.ScriptInput{
		ScriptID:    req.ScriptID,
		Name:        req.Name,
		Description: req.Description,
		Source:      req.Source,
		Enabled:     enabled,
	})
	if err != nil {
		return 0, "", err
	}

	action := "create"
	if version > 1 {
		action = "update"
	}
	summary := fmt.Sprintf("v%d: %s", version, req.Name)
	if !enabled {
		summary += " (disabled)"
	}
	logID = uuid.NewString()
	_ = repo.CreateScriptLog(&model.ScriptLog{
		LogID:     logID,
		ScriptID:  req.ScriptID,
		Version:   version,
		Action:    action,
		Operator:  operator,
		SourceIP:  sourceIP,
		RequestID: requestID,
		Summary:   summary,
	})
	return version, logID, nil
}

// listScripts returns the latest version of every script (management view).

func (r *Router) listScripts(c *gin.Context) {
	scripts, err := repo.ListScripts()
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := make([]gin.H, 0, len(scripts))
	for _, s := range scripts {
		out = append(out, gin.H{
			"script_id":  s.ScriptID,
			"name":       s.Name,
			"version":    s.Version,
			"enabled":    s.Enabled,
			"updated_at": s.UpdatedAt.Format(time.RFC3339),
		})
	}
	c.JSON(http.StatusOK, gin.H{"scripts": out})
}

// scriptLogs returns the audit trail for a script_id (newest first), so every
// upload/edit is traceable: operator, source IP, request id, version, action.

func (r *Router) scriptLogs(c *gin.Context) {
	scriptID := c.Query("script_id")
	if scriptID == "" {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", "script_id query param required")
		return
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	logs, err := repo.ListScriptLogs(scriptID, limit)
	if err != nil {
		middleware.RespondError(c, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := make([]gin.H, 0, len(logs))
	for _, l := range logs {
		out = append(out, gin.H{
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
	c.JSON(http.StatusOK, gin.H{"script_id": scriptID, "logs": out})
}
