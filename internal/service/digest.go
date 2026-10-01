package service

// DigestService executes task_type="digest": it aggregates the execution
// window (task_log) and delivers an AI-written (or deterministic template, if
// AI is not configured) Chinese summary through a notify channel. Scheduling
// is the user's cron — the platform's claim exclusivity keeps multi-instance
// deployments from double-sending, and business calendars skip holidays for
// free. task_log rows record every digest for later reference.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"app-task/internal/executor"
	"app-task/internal/repo"
)

const digestSystemPrompt = "你是运维监控助手。根据给出的定时任务执行统计数据，用不超过 200 字的中文写一段执行摘要：" +
	"先一句话总体健康度，再点出最需要关注的问题（失败/慢任务），最后给一条可执行的建议。" +
	"只输出摘要正文，不要客套话和标题。"

// DigestService wires the digest task type to its channel delivery.
type DigestService struct {
	notify *NotifyService
	llm    *LLMClient
}

// NewDigestService builds the digest handler's backing service.
func NewDigestService(notify *NotifyService, llm *LLMClient) *DigestService {
	return &DigestService{notify: notify, llm: llm}
}

// Handler returns the executor.Handler for task_type="digest".
func (s *DigestService) Handler() executor.Handler {
	return func(_ context.Context, task executor.Task) error {
		return s.execute(task)
	}
}

func (s *DigestService) execute(task executor.Task) error {
	var p struct {
		Channel string `json:"channel"`
		Period  string `json:"period"` // daily (default) | weekly
		TopN    int    `json:"top_n"`  // failed/slow list length (default 5, cap 20)
	}
	if len(task.Payload) > 0 {
		if err := json.Unmarshal(task.Payload, &p); err != nil {
			return fmt.Errorf("digest payload decode: %w", err)
		}
	}
	if strings.TrimSpace(p.Channel) == "" {
		return fmt.Errorf("digest payload missing channel")
	}
	period := p.Period
	if period == "" {
		period = "daily"
	}
	if period != "daily" && period != "weekly" {
		return fmt.Errorf("digest period must be daily | weekly")
	}
	topN := p.TopN
	if topN <= 0 {
		topN = 5
	}
	if topN > 20 {
		topN = 20
	}
	window := 24 * time.Hour
	if period == "weekly" {
		window = 7 * 24 * time.Hour
	}
	now := time.Now().UTC()
	since := now.Add(-window)

	stats, failed, slow, err := repo.DigestWindowStats(since, topN)
	if err != nil {
		return fmt.Errorf("digest stats: %w", err)
	}

	summary := templateDigest(period, now, window, stats, failed, slow)
	if s.llm.Enabled() {
		if aiText, err := s.llm.Chat(context.Background(), digestSystemPrompt,
			digestUserPrompt(period, now, window, stats, failed, slow)); err == nil {
			summary = aiText
		} else {
			slog.Warn("[DIGEST] AI summary unavailable — template fallback", "err", err)
			summary += "\n\n（AI 总结不可用，以上为统计模板）"
		}
	}

	notif := &Notification{
		Title:   fmt.Sprintf("执行%s（%s ~ %s）", periodCN(period), since.In(time.Local).Format("01-02"), now.In(time.Local).Format("01-02")),
		Text:    summary,
		MsgType: "markdown",
	}
	if err := s.notify.DeliverByChannel(p.Channel, notif, "digest:"+task.ID); err != nil {
		return fmt.Errorf("digest deliver: %w", err)
	}
	slog.Info("[DIGEST] delivered", "task_id", task.ID, "channel", p.Channel, "window", period)
	if task.LogSink != nil {
		task.LogSink(fmt.Sprintf("digest delivered via %q:\n%s", p.Channel, summary))
	}
	return nil
}

func periodCN(period string) string {
	if period == "weekly" {
		return "周报"
	}
	return "日报"
}

// templateDigest is the deterministic fallback (and the LLM's data source).
func templateDigest(period string, now time.Time, window time.Duration, st repo.DigestStats, failed []repo.DigestFailedRow, slow []repo.DigestSlowRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "过去 %s 共执行 **%d** 次：成功 %d，失败 %d，重试 %d。", periodCN(period), st.Total, st.Success, st.Failed, st.Retried)
	if st.Total > 0 {
		fmt.Fprintf(&b, "成功率 %.0f%%。\n", float64(st.Success)*100/float64(st.Total))
	}
	if len(failed) > 0 {
		b.WriteString("\n**失败任务**\n")
		for _, f := range failed {
			fmt.Fprintf(&b, "- `%s` ×%d：%s\n", f.TaskID, f.Count, truncate(f.LastErr, 120))
		}
	} else {
		b.WriteString("\n无失败任务 ✅\n")
	}
	if len(slow) > 0 {
		b.WriteString("\n**最慢执行**\n")
		for _, sl := range slow {
			fmt.Fprintf(&b, "- `%s` %s（节点 %s）\n", sl.TaskID, fmtDurationMS(sl.DurationMS), strOrDash2(sl.Node))
		}
	}
	return b.String()
}

// digestUserPrompt renders the LLM input: stats + lists as compact JSON.
func digestUserPrompt(period string, now time.Time, window time.Duration, st repo.DigestStats, failed []repo.DigestFailedRow, slow []repo.DigestSlowRow) string {
	type row struct {
		TaskID string `json:"task_id"`
		Count  int64  `json:"count"`
		Error  string `json:"error,omitempty"`
	}
	type slowRow struct {
		TaskID string `json:"task_id"`
		MS     int64  `json:"duration_ms"`
		Node   string `json:"node,omitempty"`
	}
	payload := map[string]any{
		"period":  periodCN(period),
		"window":  window.String(),
		"until":   now.Format(time.RFC3339),
		"total":   st.Total,
		"success": st.Success,
		"failed":  st.Failed,
		"retried": st.Retried,
	}
	if len(failed) > 0 {
		rows := make([]row, 0, len(failed))
		for _, f := range failed {
			rows = append(rows, row{TaskID: f.TaskID, Count: f.Count, Error: truncate(f.LastErr, 200)})
		}
		payload["failed_tasks"] = rows
	}
	if len(slow) > 0 {
		rows := make([]slowRow, 0, len(slow))
		for _, sl := range slow {
			rows = append(rows, slowRow{TaskID: sl.TaskID, MS: sl.DurationMS, Node: sl.Node})
		}
		payload["slowest"] = rows
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// fmtDurationMS shapes milliseconds as "1.2s"/"850ms" (digest display).
func fmtDurationMS(ms int64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	return fmt.Sprintf("%dms", ms)
}

// strOrDash2 renders empty node as a dash (display shaping for digest).
func strOrDash2(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
