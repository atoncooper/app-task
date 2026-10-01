package router

import (
	"net/http"

	"app-task/internal/dto"
	"app-task/internal/router/middleware"

	"github.com/gin-gonic/gin"
)

// completeTask is the async callback from a third-party executor that accepted
// a task (202): it reports the final outcome and the scheduler advances
// running -> completed | failed. Key-auth via APISIX.
func (r *Router) completeTask(c *gin.Context) {
	taskID := c.Param("task_id")
	var req dto.CompleteTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "invalid_request", "invalid request: "+err.Error())
		return
	}
	status, err := r.taskSvc.CompleteTask(taskID, req.Status, req.Result, req.Error)
	if err != nil {
		middleware.RespondError(c, http.StatusNotFound, "task_not_found", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"task_id": taskID, "status": status})
}
