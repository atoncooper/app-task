// Package repo: data access for the scheduler's execution log.
package repo

import (
	"time"

	"app-task/internal/db"
	"app-task/internal/model"
)

// CreateTaskLog appends one execution record (every trigger writes a log).
func CreateTaskLog(l *model.TaskLog) error {
	return db.DB.Create(l).Error
}

// ListTaskLogs returns the execution trail for a task, newest first.
func ListTaskLogs(taskID string, limit int) ([]model.TaskLog, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []model.TaskLog
	err := db.DB.Where("task_id = ?", taskID).
		Order("id DESC").Limit(limit).Find(&out).Error
	return out, err
}

// ListRecentLogs returns the most recent execution logs across all tasks
// (admin console), optionally filtered by task_id.
func ListRecentLogs(taskID string, limit int) ([]model.TaskLog, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []model.TaskLog
	q := db.DB.Order("id DESC")
	if taskID != "" {
		q = q.Where("task_id = ?", taskID)
	}
	err := q.Limit(limit).Find(&out).Error
	return out, err
}

// CountTaskLogs returns the total number of execution records (dashboard).
func CountTaskLogs() (int64, error) {
	var n int64
	err := db.DB.Model(&model.TaskLog{}).Count(&n).Error
	return n, err
}

// ── digest 统计（AI/模板日报的数据源） ─────────────────────────────────────

// DigestStats is the execution window summary feeding daily/weekly digests.
type DigestStats struct {
	Total, Success, Failed, Retried int64
}

// DigestFailedRow is one task's failure aggregation within the window.
type DigestFailedRow struct {
	TaskID  string `gorm:"column:task_id"`
	Count   int64  `gorm:"column:c"`
	LastErr string `gorm:"column:last_error"`
}

// DigestSlowRow is one slow execution within the window.
type DigestSlowRow struct {
	TaskID     string    `gorm:"column:task_id"`
	DurationMS int64     `gorm:"column:duration_ms"`
	Node       string    `gorm:"column:node"`
	TriggerAt  time.Time `gorm:"column:trigger_at"`
}

// DigestWindowStats collects the counts, top failed tasks and slowest
// executions since `since` (single queries, no N+1).
func DigestWindowStats(since time.Time, topN int) (DigestStats, []DigestFailedRow, []DigestSlowRow, error) {
	var stats DigestStats
	rows := []struct {
		Status string `gorm:"column:status"`
		C      int64  `gorm:"column:c"`
	}{}
	if err := db.DB.Model(&model.TaskLog{}).
		Select("status, COUNT(*) AS c").
		Where("trigger_at >= ?", since).
		Group("status").Scan(&rows).Error; err != nil {
		return stats, nil, nil, err
	}
	for _, r := range rows {
		stats.Total += r.C
		switch r.Status {
		case "success":
			stats.Success = r.C
		case "failed":
			stats.Failed = r.C
		case "retry":
			stats.Retried = r.C
		}
	}
	if topN <= 0 {
		topN = 5
	}
	var failed []DigestFailedRow
	if err := db.DB.Model(&model.TaskLog{}).
		Select("task_id, COUNT(*) AS c, MAX(error) AS last_error").
		Where("trigger_at >= ? AND status = ?", since, "failed").
		Group("task_id").Order("c DESC").Limit(topN).
		Scan(&failed).Error; err != nil {
		return stats, nil, nil, err
	}
	var slow []DigestSlowRow
	if err := db.DB.Model(&model.TaskLog{}).
		Select("task_id, duration_ms, node, trigger_at").
		Where("trigger_at >= ? AND status = ?", since, "success").
		Order("duration_ms DESC").Limit(topN).
		Scan(&slow).Error; err != nil {
		return stats, nil, nil, err
	}
	return stats, failed, slow, nil
}
