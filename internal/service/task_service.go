// Package service: the scheduler's business-neutral services. app-task only
// registers tasks, dispatches them to executors, and records outcomes — all
// business logic lives in third-party executors.
package service

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"app-task/internal/model"
	"app-task/internal/repo"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// TaskService handles task registration and async completion callbacks.
type TaskService struct{}

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
}

func (s *TaskService) RegisterTask(o RegisterOptions) (string, error) {
	taskType := o.TaskType
	if taskType == "" {
		taskType = "http"
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
		next, err := NextCronTrigger(cronExpr, time.Now())
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
	taskID := uuid.NewString()
	task := &model.Task{
		TaskID:      taskID,
		UID:         o.UID,
		TaskType:    taskType,
		TriggerTime: triggerTime,
		Status:      "pending",
		MaxRetry:    maxRetry,
		Weight:      weight,
		Shard:       o.Shard,
		ShardTotal:  o.ShardTotal,
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
	slog.Info("[JOB] registered", "task_id", taskID, "task_type", taskType, "executor_url", o.ExecutorURL, "async", o.Async, "cron", cronExpr, "max_retry", maxRetry, "weight", weight, "trigger", formatBeijing(triggerTime), "shard", o.Shard)
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
