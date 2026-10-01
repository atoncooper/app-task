// Package dto: transport-layer data transfer objects — named request bodies
// for the JSON endpoints (service surface + admin console API). DTOs carry no
// behavior beyond shape: binding/validation tags, JSON names, and the exact
// wire contract documented in docs/api.md. Handlers translate a DTO into
// service-layer calls; the dto package imports nothing internal.
package dto

import "encoding/json"

// RegisterTaskRequest is the POST /tasks/register body (docs/api.md §4.1).
// UID is required on the service surface; a key-bound uid must match it.
type RegisterTaskRequest struct {
	UID          int64           `json:"uid" binding:"required"`
	TaskType     string          `json:"task_type"`    // http (default) / lua / notify
	Payload      json.RawMessage `json:"payload"`      // opaque task parameters
	ExecutorURL  string          `json:"executor_url"` // http mode: third-party executor endpoint
	Async        bool            `json:"async"`        // true: executor replies 202 + callback
	CronExpr     string          `json:"cron_expr"`    // 5-field cron; empty = one-shot
	TriggerTime  string          `json:"trigger_time"` // required when cron_expr is empty
	MaxRetry     int             `json:"max_retry"`
	Weight       int             `json:"weight"`
	Shard        bool            `json:"shard"`         // 分片广播模式
	ShardTotal   int             `json:"shard_total"`   // 0 = 按存活节点数
	CalendarID   string          `json:"calendar_id"`   // 业务日历（cron 任务生效）
	AlertChannel string          `json:"alert_channel"` // 最终失败告警渠道（notify 渠道名）
}

// AdminCreateTaskRequest is the POST /api/tasks body (admin console). Unlike
// the service surface, uid is optional: 0 attributes the task to the logged-in
// console user (sessionUID), master-token sessions pass 0 through.
type AdminCreateTaskRequest struct {
	UID          int64           `json:"uid"`
	TaskType     string          `json:"task_type"`
	Payload      json.RawMessage `json:"payload"`
	ExecutorURL  string          `json:"executor_url"`
	Async        bool            `json:"async"`
	CronExpr     string          `json:"cron_expr"`
	TriggerTime  string          `json:"trigger_time"`
	MaxRetry     int             `json:"max_retry"`
	Weight       int             `json:"weight"`
	Shard        bool            `json:"shard"`
	ShardTotal   int             `json:"shard_total"`
	CalendarID   string          `json:"calendar_id"`
	AlertChannel string          `json:"alert_channel"`
}

// CompleteTaskRequest is the POST /internal/task/:task_id/complete body —
// the async executor's final-outcome callback (running → completed | failed).
type CompleteTaskRequest struct {
	Status string `json:"status" binding:"required"` // completed | failed
	Result string `json:"result"`
	Error  string `json:"error"`
}

// SendEmailRequest is the POST /internal/email/send body: a standardized
// email the platform queues and delivers with retries.
type SendEmailRequest struct {
	To          []string `json:"to" binding:"required"`
	CC          []string `json:"cc"`
	Subject     string   `json:"subject" binding:"required,max=255"`
	HTML        string   `json:"html" binding:"required"`
	ReferenceID string   `json:"reference_id" binding:"max=64"`
}

// RunScriptRequest is the POST /internal/script/run body: on-demand execution
// of a registered Lua script (payload passed to ctx.payload() verbatim).
type RunScriptRequest struct {
	ScriptID string          `json:"script_id" binding:"required,max=64"`
	Payload  json.RawMessage `json:"payload"`
}

// ScriptUploadRequest is the shared create/update body used by both the
// key-auth POST /scripts endpoint and the admin console POST /api/scripts.
// ScriptID is optional: the server assigns a UUID when omitted (drives the
// upsert/update flow when present).
type ScriptUploadRequest struct {
	ScriptID    string `json:"script_id" binding:"max=64"`
	Name        string `json:"name" binding:"required,max=128"`
	Description string `json:"description" binding:"max=512"`
	Source      string `json:"source" binding:"required"`
	Enabled     *bool  `json:"enabled"`
	Operator    string `json:"operator" binding:"max=64"`
}

// ToggleScriptRequest is the POST /api/scripts/:script_id/toggle body: flips
// the enabled flag without creating a new version.
type ToggleScriptRequest struct {
	Enabled  bool   `json:"enabled"`
	Operator string `json:"operator" binding:"max=64"`
}

// LoginRequest is the POST /api/login body: username + password (console
// login) or the master token (script/API fallback).
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// CreateUserRequest is the POST /api/users body (admin only).
type CreateUserRequest struct {
	Username string `json:"username" binding:"required,min=2,max=64"`
	Password string `json:"password" binding:"required,min=8,max=128"`
	Role     string `json:"role"`
}

// SetUserPasswordRequest is the POST /api/users/:user_id/password body.
type SetUserPasswordRequest struct {
	Password string `json:"password" binding:"required,min=8,max=128"`
}

// AIGenerateScriptRequest is the AI script-generation endpoint body: a free
// text requirement description (rate-limited server-side).
type AIGenerateScriptRequest struct {
	Requirement string `json:"requirement"`
}

// UpsertCalendarDateRequest is the POST /api/calendars/dates body: one
// holiday/adjustment day on a named business calendar.
type UpsertCalendarDateRequest struct {
	Name    string `json:"name"`
	Date    string `json:"date"`
	DayType string `json:"day_type"`
}

// CreateCalendarRequest is the POST /api/calendars body.
type CreateCalendarRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpsertChannelRequest is the POST /api/channels body: a notify channel
// definition (config JSON holds the channel's credentials).
type UpsertChannelRequest struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Config string `json:"config"`
}
