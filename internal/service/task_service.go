// Package service: the scheduler's business-neutral services. app-task only
// registers tasks, dispatches them to executors, and records outcomes — all
// business logic lives in third-party executors.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"

	"app-task/internal/model"
	"app-task/internal/repo"

	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// TaskService handles task registration and async completion callbacks.
// BlockPrivateExecutorHosts turns on the SSRF hardening: registrations whose
// executor_url targets loopback/private/link-local hosts are rejected
// (deployment policy, off by default — in-cluster executors are private).
type TaskService struct {
	BlockPrivateExecutorHosts bool
}

// ErrInvalidExecutorURL marks registration rejections caused by executor_url
// validation (scheme whitelist / private-host blocking); callers map it to
// their transport's specific error shape via errors.Is.
var ErrInvalidExecutorURL = errors.New("invalid executor_url")

func NewTaskService() *TaskService {
	return &TaskService{}
}

// RegisterTask registers a pure scheduling task: task_type selects the executor
// (http by default; lua for the optional built-in script executor),
// executor_url points at a third-party executor for http tasks, payload is
// passed to the executor verbatim (the scheduler never interprets it).
//
// Scheduling: cron_expr empty = one-shot (triggerTime used as-is); otherwise
// the first occurrence is computed from the cron expression. Recurring tasks
// are extended by the scheduler once they reach a terminal state.
// RegisterOptions carries one task registration's inputs (RegisterTask's
// former positional parameters, plus shard-broadcast settings).
type RegisterOptions struct {
	UID         int64
	TaskType    string
	Payload     []byte
	ExecutorURL string
	Async       bool
	CronExpr    string
	TriggerTime time.Time
	MaxRetry    int
	Weight      int
	// Shard: broadcast mode — each trigger fans out into shards (one per alive
	// node by default) instead of a single execution.
	Shard bool
	// ShardTotal fixes the fan-out (0 = auto: alive-node count at split time).
	ShardTotal int
	// CalendarID: business calendar name applied to cron materialization
	// (empty = none). One-shot trigger_time tasks ignore it.
	CalendarID string
	// AlertChannel: notify channel name pushed to when the task finally fails
	// (empty = no alert). AI RCA is attached automatically when configured.
	AlertChannel string
}

func (s *TaskService) RegisterTask(o RegisterOptions) (string, error) {
	taskType := o.TaskType
	if taskType == "" {
		taskType = "http"
	}
	// executor_url contract: absolute http/https URLs only; with the SSRF
	// hardening on, private/loopback targets are rejected (DNS-resolved,
	// fail-closed). Both surfaces (service API + admin console) funnel
	// through here, so the invariant holds no matter the caller.
	if o.ExecutorURL != "" {
		if err := validateExecutorURL(o.ExecutorURL, s.BlockPrivateExecutorHosts); err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidExecutorURL, err)
		}
	}
	weight := o.Weight
	if weight <= 0 {
		weight = 1
	}
	maxRetry := o.MaxRetry
	if maxRetry < 0 {
		maxRetry = 0
	}
	triggerTime := o.TriggerTime
	cronExpr := o.CronExpr
	if cronExpr != "" {
		next, err := NextTriggerForTask(cronExpr, time.Now(), o.CalendarID)
		if err != nil {
			return "", fmt.Errorf("invalid cron_expr: %w", err)
		}
		triggerTime = next
	}
	// 分片广播与异步执行器互斥：异步子片会让父任务的完成判定依赖第三方回调
	// 语义，超时/丢失场景无法收敛到终态。
	if o.Shard && o.Async {
		return "", fmt.Errorf("shard broadcast is not supported with async executors")
	}
	if o.ShardTotal < 0 || o.ShardTotal > 1000 {
		return "", fmt.Errorf("shard_total out of range (0..1000)")
	}
	if o.AlertChannel != "" {
		ch, err := repo.GetNotifyChannelByName(strings.TrimSpace(o.AlertChannel))
		if err != nil {
			return "", fmt.Errorf("alert channel lookup: %w", err)
		}
		if ch == nil {
			return "", fmt.Errorf("alert channel %q not found (create it first)", o.AlertChannel)
		}
	}
	taskID := uuid.NewString()
	task := &model.Task{
		TaskID:       taskID,
		UID:          o.UID,
		TaskType:     taskType,
		TriggerTime:  triggerTime,
		Status:       "pending",
		MaxRetry:     maxRetry,
		Weight:       weight,
		Shard:        o.Shard,
		ShardTotal:   o.ShardTotal,
		CalendarID:   o.CalendarID,
		AlertChannel: strings.TrimSpace(o.AlertChannel),
	}
	if len(o.Payload) > 0 {
		task.Payload = datatypes.JSON(o.Payload)
	}
	if o.ExecutorURL != "" {
		task.ExecutorURL = o.ExecutorURL
	}
	task.Async = o.Async
	if cronExpr != "" {
		task.CronExpr = cronExpr
	}
	if err := repo.CreateTask(task); err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	slog.Info("[JOB] registered", "task_id", taskID, "task_type", taskType, "executor_url", o.ExecutorURL, "async", o.Async, "cron", cronExpr, "max_retry", maxRetry, "weight", weight, "trigger", formatBeijing(triggerTime), "shard", o.Shard, "calendar", o.CalendarID)
	return taskID, nil
}

// CompleteTask handles the async callback from a third-party executor
// (POST /internal/task/{id}/complete): running -> completed|failed. Idempotent:
// a repeated callback returns the current status unchanged.
func (s *TaskService) CompleteTask(taskID, status, result, errMsg string) (string, error) {
	task, err := repo.GetTaskByID(taskID)
	if err != nil {
		return "", err
	}
	if task == nil {
		return "", errors.New("task not found")
	}
	toStatus := "completed"
	if status != "completed" {
		toStatus = "failed"
	}
	var lastResult *string
	if result != "" || errMsg != "" {
		s := result
		if errMsg != "" {
			if s != "" {
				s += "; "
			}
			s += "err: " + errMsg
		}
		lastResult = &s
	}
	updated, _ := repo.ConditionalUpdate(taskID, "running", toStatus, map[string]any{"last_result": lastResult})
	if !updated {
		// Idempotent: already terminal (or raced). Return current status.
		cur, _ := repo.GetTaskByID(taskID)
		if cur != nil {
			return cur.Status, nil
		}
		return "", nil
	}
	var errField *string
	if errMsg != "" {
		errField = &errMsg
	}
	_ = repo.CreateTaskLog(&model.TaskLog{
		LogID:    uuid.NewString(),
		TaskID:   taskID,
		Executor: task.ExecutorURL,
		Node:     task.Owner,
		Response: result,
		Status:   toStatus,
		Error:    errField,
	})
	slog.Info("[JOB] completed via callback", "task_id", taskID, "status", toStatus)
	return toStatus, nil
}

// validateExecutorURL enforces the executor_url contract for http tasks:
// absolute http/https URLs only. With blockPrivate set
// (security.executor_block_private_hosts) hosts resolving into
// loopback/private/link-local ranges are rejected, blunting SSRF probes of
// the internal network at registration time. Hostname resolution is
// fail-closed with a short timeout - a host that does not resolve now will
// not resolve at dispatch either. DNS rebinding between check and dispatch
// remains possible; the deployment boundary note in docs/api.md covers it.
func validateExecutorURL(raw string, blockPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("executor_url unparseable: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("executor_url scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("executor_url host is required")
	}
	if !blockPrivate {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("executor_url host %s is a private/loopback address "+
				"(blocked by security.executor_block_private_hosts)", host)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("executor_url host %s does not resolve", host)
	}
	for _, a := range addrs {
		if isBlockedIP(a.IP) {
			return fmt.Errorf("executor_url host %s resolves to a private/loopback address "+
				"(blocked by security.executor_block_private_hosts)", host)
		}
	}
	return nil
}

// isBlockedIP reports whether the address falls in a range tasks must not
// target when private-host blocking is enabled.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
