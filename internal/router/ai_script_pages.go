package router

// AI Lua 脚本生成端点：admin 门禁（与脚本管理同面）、AI 未配置 503、
// 生成失败 400 带原因（编辑器展示给用户）。

import (
	"app-task/internal/dto"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (r *Router) handleScriptAIGenerate(c *gin.Context) {
	s, ok := requireAdminPage(c)
	if !ok {
		return
	}
	if r.scriptGen == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "AI 未启用（ai.model / ai.api_key 未配置）"})
		return
	}
	var req dto.AIGenerateScriptRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "invalid request: " + err.Error()})
		return
	}
	result, err := r.scriptGen.Generate(s.Username, strings.TrimSpace(req.Requirement))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
