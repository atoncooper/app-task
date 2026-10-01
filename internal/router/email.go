package router

import (
	"net/http"

	"app-task/internal/dto"
	"app-task/internal/router/middleware"

	"github.com/gin-gonic/gin"
)

// sendEmail accepts a standardized email from a third-party executor and
// queues it for delivery (email_queue + worker with retries). Contract:
//
//	{"to":["a@x.com"], "cc":["c@x.com"], "subject":"...", "html":"<div>...</div>", "reference_id":"task-xxx"}
//
// to/subject/html are required; cc and reference_id optional. The scheduler
// platform only understands this mail format — the executor renders the
// content (business side). Key-auth via APISIX.
func (r *Router) sendEmail(c *gin.Context) {
	if c.Request.ContentLength > maxEmailBodyBytes {
		middleware.RespondError(c, http.StatusBadRequest, "payload_too_large", "request too large")
		return
	}
	var req dto.SendEmailRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", "invalid request: "+err.Error())
		return
	}
	emailID, err := r.emailSvc.Enqueue(req.To, req.CC, req.Subject, req.HTML, req.ReferenceID)
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "enqueue_failed", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"email_id": emailID, "status": "queued"})
}
