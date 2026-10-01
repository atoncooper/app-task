package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"app-task/internal/executor"
	"app-task/internal/model"
	"app-task/internal/repo"

	"github.com/google/uuid"
)

// Scheduler polls the DB for due tasks and dispatches them to the executor
// registered for each task's task_type. Pure scheduling: the executor (HTTP to
// a third-party service, or the optional built-in Lua executor) does the
// work; the scheduler records every execution in task_log and advances the
// task state machine.
//
// Concurrency model: each tick claims a batch of due tasks and runs every
// dispatch in its own goroutine, bounded by the global slot pool (Workers)
// and the per-executor_url gates (PerURLLimit). Each dispatch first CLAIMS
// the task (conditional update pending → dispatching — mutually exclusive
// across workers AND instances), then runs the handler and finalizes. The
// tick waits for the batch to drain, so ticks never overlap and the DB scan
// rate adapts to executor latency.
//
// State machine:
//
//	pending -> dispatching -> completed            (sync 2xx)
//	pending -> dispatching -> running -> completed | failed   (async callback)
//	dispatching/failed -> pending                  (retry with next_retry_at)
//	dispatching older than DispatchingTTL -> pending (reclaim, at-least-once)
//	running > 10m without callback  -> failed (timeout)
type Scheduler struct {
	registry *executor.Registry
	stopCh   chan struct{}

	// Tuning (see SchedulerOptions; zero values fall back to the defaults).
	interval       time.Duration
	jitter         time.Duration // random pre-tick delay to de-sync cluster instances
	workers        int
	batchSize      int
	dispatchingTTL time.Duration
	perURLLimit    int
	maxShards      int
	notify         *NotifyService
	llm            *LLMClient
	sendPayload    bool
	owner          string

	slots    chan struct{}  // global dispatch concurrency bound (cap = workers)
	wg       sync.WaitGroup // loop goroutine + in-flight dispatch goroutines
	inflight atomic.Int64

	// Per-executor_url concurrency gates: one slow third-party executor can
	// only occupy PerURLLimit slots, never starve the whole pool.
	semsMu sync.Mutex
	sems   map[string]chan struct{}

	// cluster (optional, wired from the cluster manager) provides dead-node
	// takeover and weight-proportional claim limits.
	cluster ClusterView

	// weight is this node's dispatch share (scheduler.weight, default 1).
	weight int
}

// ClusterView abstracts the node roster for the scheduler (satisfied by
// *cluster.Manager): dead-node takeover and weight-proportional claiming.
type ClusterView interface {
	DeadNodes() []string
	MaxAliveWeight() int
	AliveCount() int
}

// SetClusterView attaches the cluster roster provider.
func (s *Scheduler) SetClusterView(v ClusterView) { s.cluster = v }

// SchedulerOptions configures the scheduler. Zero values fall back to
// production defaults in NewScheduler.
type SchedulerOptions struct {
	Interval       time.Duration // DB poll interval (default 30s)
	Workers        int           // global dispatch concurrency (default 16)
	BatchSize      int           // due tasks claimed per tick (default 50)
	DispatchingTTL time.Duration // stale dispatching claim reclaim age (default 2m)
	PerURLLimit    int           // max concurrent dispatches per executor_url (0 = unlimited)
	Owner          string        // instance identity for claims (default hostname+rand)
	Weight         int           // dispatch share relative to the max alive weight (default 1)
	MaxShards      int           // shard-broadcast fan-out cap (default 32)
	// Notify + LLM power the failure alert path (task.alert_channel): alerts
	// go out on final failure, with an AI RCA section when LLM is configured.
	Notify          *NotifyService
	LLM             *LLMClient
	SendPayloadData bool // false: RCA prompts never include task payload bodies
}

// Defaults for zero-valued SchedulerOptions fields.
const (
	defaultInterval    = 30 * time.Second
	defaultWorkers     = 16
	defaultBatchSize   = 50
	defaultDispatchTTL = 2 * time.Minute
	defaultPerURLLimit = 8
)

func NewScheduler(reg *executor.Registry, opts SchedulerOptions) *Scheduler {
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = defaultBatchSize
	}
	if opts.DispatchingTTL <= 0 {
		opts.DispatchingTTL = defaultDispatchTTL
	}
	if opts.Owner == "" {
		host, _ := os.Hostname()
		opts.Owner = host + "-" + uuid.NewString()[:8]
	}
	if opts.Weight <= 0 {
		opts.Weight = 1
	}
	if opts.Weight > 10000 { // overflow guard for the weighted-claim arithmetic
		opts.Weight = 10000
	}
	if opts.MaxShards <= 0 {
		opts.MaxShards = 32
	}
	return &Scheduler{
		registry:       reg,
		stopCh:         make(chan struct{}),
		interval:       opts.Interval,
		jitter:         opts.Interval / 10,
		workers:        opts.Workers,
		batchSize:      opts.BatchSize,
		dispatchingTTL: opts.DispatchingTTL,
		perURLLimit:    opts.PerURLLimit,
		maxShards:      opts.MaxShards,
		owner:          opts.Owner,
		notify:         opts.Notify,
		llm:            opts.LLM,
		sendPayload:    opts.SendPayloadData,
		weight:         opts.Weight,
		slots:          make(chan struct{}, opts.Workers),
		sems:           make(map[string]chan struct{}),
	}
}

func (s *Scheduler) Start() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.loop()
	}()
	slog.Info("[SCHEDULER] started", "interval", s.interval, "workers", s.workers,
		"batch", s.batchSize, "owner", s.owner, "handlers", s.registry.Types())
}

// Stop shuts the scheduler down: the poll loop exits (its in-flight batch wait
// is released via stopCh) and in-flight dispatch handlers are given a grace
// period to finish. Anything unfinished stays in dispatching and is reclaimed
// by this process after restart, or by another instance in a cluster.
func (s *Scheduler) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("[SCHEDULER] graceful stop timed out; in-flight dispatches left to reclaim")
	}
	slog.Info("[SCHEDULER] stopped")
}

// Stats exposes pool gauges for the console dashboard.
func (s *Scheduler) Stats() (workers int, inflight int64) {
	return s.workers, s.inflight.Load()
}

func (s *Scheduler) loop() {
	s.tick() // fire once immediately (catch up after restart)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			// Small random pre-tick delay: cluster instances started together
			// don't hit the DB in lockstep every interval.
			if s.jitter > 0 {
				time.Sleep(time.Duration(rand.Int63n(int64(s.jitter))))
			}
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	// 0. Extend recurring (cron) tasks into their next occurrence.
	s.extendCronTasks()

	// 0.4 Fast takeover: claims held by nodes whose heartbeat went stale are
	// reclaimed immediately (correctness is unaffected — finalizes are fenced
	// by claim token; a "dead" node that is actually just stalled discards its
	// result as orphaned).
	if s.cluster != nil {
		if ids := s.cluster.DeadNodes(); len(ids) > 0 {
			if n, err := repo.ReclaimDispatchingForNodes(ids); err != nil {
				slog.Error("[SCHEDULER] dead-node takeover failed", "err", err)
			} else if n > 0 {
				slog.Warn("[SCHEDULER] took over claims of dead nodes", "count", n, "nodes", ids)
			}
		}
	}

	// 0.5 Reclaim dispatching claims that outlived the timeout (crashed
	// worker/instance) so their tasks are re-dispatched.
	if n, err := repo.ReclaimDispatching(time.Now().UTC().Add(-s.dispatchingTTL)); err != nil {
		slog.Error("[SCHEDULER] reclaim stale dispatching failed", "err", err)
	} else if n > 0 {
		slog.Warn("[SCHEDULER] reclaimed stale dispatching claims", "count", n)
	}

	// 1. Claim the due batch (transactional, SKIP LOCKED on MySQL) and
	// dispatch it in parallel goroutines (bounded by the global slot pool +
	// per-URL gates), then wait for it to drain, so ticks never overlap and a
	// slow executor delays the next scan instead of piling up work. The claim
	// limit is capped at the pool size: claiming more would just saturate the
	// pool and release the excess right back.
	claimLimit := s.batchSize
	if claimLimit > s.workers {
		claimLimit = s.workers
	}
	// Weight-proportional claiming: scale down by ownWeight / maxAliveWeight
	// so a low-weight node (e.g. a box that also runs the manager duties)
	// takes a proportionally smaller share. The highest-weight node always
	// runs at full capacity; all-equal weights change nothing; if the
	// max-weight node dies the divisor shrinks and the survivors automatically
	// pick up its share. ceil() keeps the minimum at 1.
	if s.cluster != nil && s.weight > 0 {
		if mw := s.cluster.MaxAliveWeight(); mw > s.weight && mw > 0 {
			claimLimit = (claimLimit*s.weight + mw - 1) / mw
			if claimLimit < 1 {
				claimLimit = 1
			}
		}
	}
	claimed, err := repo.ClaimTasksBatch(claimLimit, s.owner, time.Now().UTC())
	if err != nil {
		slog.Error("[SCHEDULER] claim due batch failed", "err", err)
	}
	if len(claimed) > 0 {
		var batch sync.WaitGroup
		for i := range claimed {
			batch.Add(1)
			s.wg.Add(1)
			go func(t *model.Task) {
				defer s.wg.Done()
				defer batch.Done()
				s.dispatch(t)
			}(&claimed[i])
		}
		batchDone := make(chan struct{})
		go func() { batch.Wait(); close(batchDone) }()
		select {
		case <-batchDone:
		case <-s.stopCh:
			slog.Warn("[SCHEDULER] stop during dispatch batch; unfinished claims will be reclaimed")
		}
	}

	// 2. Async tasks stuck in running without a callback -> timeout -> failed.
	running, err := repo.ListRunning(50)
	if err != nil {
		slog.Error("[SCHEDULER] list running failed", "err", err)
	}
	for _, j := range running {
		if time.Since(j.UpdatedAt) > 10*time.Minute {
			slog.Warn("[SCHEDULER] running timeout (>10m), marking failed", "task_id", j.TaskID, "age", time.Since(j.UpdatedAt))
			if _, _ = repo.ConditionalUpdate(j.TaskID, "running", "failed", nil); true {
				s.writeLog(&j, "timeout", 0, "", "no callback within 10m")
			}
		}
	}
}

// dispatch runs one due task: acquire the global slot and the per-URL slot
// (both non-blocking — a saturated pool or executor leaves the task pending
// for the next tick instead of hoarding), claim it (pending → dispatching,
// mutually exclusive across workers/instances), execute, finalize.
func (s *Scheduler) dispatch(task *model.Task) {
	// 分片广播触发行：分裂后由子任务执行，本行转为 running 等全部子片终态。
	// 存活节点数 <=1 或分裂失败（认领丢失）时不执行——认领丢失意味着该行
	// 已被回收重派，由新的持有者重新分裂。
	if task.Shard && task.ParentTaskID == nil {
		n := s.shardCount(task)
		if n > 1 {
			ok, err := repo.SplitShardBroadcast(task, n, time.Now().UTC())
			if err != nil {
				s.markFailedOrRetry(task, fmt.Errorf("shard split: %w", err), time.Now())
				return
			}
			if !ok {
				s.logOrphan(task, "broadcast split (claim superseded)")
				return
			}
			s.writeLog(task, "accepted", 0, fmt.Sprintf("broadcast: %d shards", n), "")
			slog.Info("[SCHEDULER] shard broadcast split", "task_id", task.TaskID, "shards", n)
			return
		}
		// n <= 1：退化为普通执行（本行已是普通任务语义）
	}
	// Fencing: every transition below is guarded by this claim token, so a
	// stale holder (stalled past the dispatching TTL, claim reclaimed and
	// re-dispatched by another instance) can never clobber the newer claim —
	// its result is discarded as orphaned.
	token := task.ClaimToken
	// Global pool bound. Already claimed by this tick's batch — a saturated
	// pool/executor releases the claim so the task stays immediately due
	// (no executor fire, no retry counted, no 2-minute reclaim wait).
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		if _, err := repo.ReleaseClaim(task.TaskID, token); err != nil {
			slog.Warn("[SCHEDULER] release claim failed", "task_id", task.TaskID, "err", err)
		}
		return // pool saturated: task back to pending, retried next tick
	}
	// Per-executor bound.
	releaseURL, ok := s.tryAcquireURL(task)
	if !ok {
		if _, err := repo.ReleaseClaim(task.TaskID, token); err != nil {
			slog.Warn("[SCHEDULER] release claim failed", "task_id", task.TaskID, "err", err)
		}
		return // this executor is saturated: task back to pending
	}
	defer releaseURL()

	s.inflight.Add(1)
	defer s.inflight.Add(-1)

	start := time.Now()
	h, ok := s.registry.Handler(task.TaskType)
	if !ok {
		slog.Error("[SCHEDULER] no handler for task_type", "task_type", task.TaskType, "task_id", task.TaskID)
		s.markFailedOrRetry(task, fmt.Errorf("no handler for task type %q", task.TaskType), start)
		return
	}
	err := h(context.Background(), taskFromTask(task))
	if err == nil {
		// Sync success -> completed.
		if ok, _ := repo.FinalizeClaim(task.TaskID, token, "completed", map[string]any{"last_result": "ok"}); ok {
			s.writeLog(task, "success", time.Since(start).Milliseconds(), "ok", "")
			s.maybeFinalizeShardParent(task)
		} else {
			s.logOrphan(task, "success")
		}
		return
	}
	if errors.Is(err, executor.ErrAsync) {
		// Async accepted -> running; the executor reports via callback.
		if ok, _ := repo.FinalizeClaim(task.TaskID, token, "running", nil); ok {
			s.writeLog(task, "accepted", time.Since(start).Milliseconds(), "", "")
		} else {
			s.logOrphan(task, "accepted")
		}
		return
	}
	s.markFailedOrRetry(task, err, start)
}

// sendFailureAlert pushes a final-failure alert through the task's channel,
// with an AI root-cause section when the LLM gateway is configured. Runs in
// its own goroutine — panics are contained, delivery errors are logged only
// (the task itself is already terminal).
func (s *Scheduler) sendFailureAlert(task *model.Task, errMsg string) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("[SCHEDULER] failure alert panic", "task_id", task.TaskID, "panic", rec)
		}
	}()
	text := fmt.Sprintf("**任务最终失败**（重试 %d 次后）\n- task_id: `%s`\n- 执行器: %s\n- 执行节点: %s\n- 错误: %s",
		task.RetryCount, task.TaskID, executorName(task), s.owner, truncate(errMsg, 500))
	title := fmt.Sprintf("任务失败：%s", task.TaskID)

	if s.llm != nil && s.llm.Enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		text += s.rcaSection(ctx, task, errMsg)
	}
	notif := &Notification{Title: title, Text: text, MsgType: "markdown"}
	if err := s.notify.DeliverByChannel(task.AlertChannel, notif, ""); err != nil {
		slog.Error("[SCHEDULER] failure alert delivery failed", "task_id", task.TaskID, "channel", task.AlertChannel, "err", err)
		return
	}
	slog.Info("[SCHEDULER] failure alert sent", "task_id", task.TaskID, "channel", task.AlertChannel)
}

// rcaSection asks the LLM for a root-cause + fix suggestion from the error
// and the last few execution records. Privacy: task payload bodies are only
// included when ai.send_payload_data is true (may contain business data).
func (s *Scheduler) rcaSection(ctx context.Context, task *model.Task, errMsg string) string {
	logs, err := repo.ListTaskLogs(task.TaskID, 5)
	var records []string
	if err == nil {
		for i := range logs {
			e := ""
			if logs[i].Error != nil {
				e = *logs[i].Error
			}
			records = append(records, fmt.Sprintf("%s status=%s node=%s duration=%dms error=%s",
				logs[i].TriggerAt.Format(time.RFC3339), logs[i].Status, logs[i].Node, logs[i].DurationMS, e))
		}
	}
	user := fmt.Sprintf("定时任务信息：\n- task_type=%s executor=%s\n- 最终错误：%s\n- 最近执行记录：\n%s",
		task.TaskType, task.ExecutorURL, errMsg, strings.Join(records, "\n"))
	if s.sendPayload && len(task.Payload) > 0 {
		user += "\n- payload: " + truncate(string(task.Payload), 1000)
	}
	rca, err := s.llm.Chat(ctx, "你是运维专家。根据定时任务的错误与最近执行记录，用不超过 150 字的中文给出根因分析和一条修复建议。只输出正文。", user)
	if err != nil {
		slog.Warn("[SCHEDULER] AI RCA unavailable", "task_id", task.TaskID, "err", err)
		return "\n\n（AI 诊断不可用）"
	}
	return "\n\n**【AI 诊断】**\n" + rca
}

// logOrphan records a dispatch result that could not be finalized because the
// claim was superseded (reclaimed and re-dispatched after a >TTL stall). The
// executor side effect DID happen — at-least-once semantics — so operators
// must know; the task state itself stays consistent with the live claim.
func (s *Scheduler) logOrphan(task *model.Task, outcome string) {
	slog.Warn("[SCHEDULER] orphaned dispatch result discarded (claim superseded)",
		"task_id", task.TaskID, "outcome", outcome)
}

// tryAcquireURL takes one slot of the executor_url's concurrency gate
// (non-blocking). ok=false means the executor is saturated: the caller must
// NOT claim the task — it stays pending and is retried next tick. Skipping
// (instead of blocking) keeps the pool responsive to every other executor:
// a saturated endpoint can only ever occupy its own PerURLLimit slots.
func (s *Scheduler) tryAcquireURL(task *model.Task) (release func(), ok bool) {
	if s.perURLLimit <= 0 {
		return func() {}, true
	}
	key := task.ExecutorURL
	if key == "" {
		key = task.TaskType // lua and other URL-less types share one gate
	}
	s.semsMu.Lock()
	sem, known := s.sems[key]
	if !known {
		sem = make(chan struct{}, s.perURLLimit)
		s.sems[key] = sem
	}
	s.semsMu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	default:
		return func() {}, false
	}
}

// shardCount computes the shard fan-out for a broadcast trigger: the task's
// fixed override when set, otherwise the live roster's alive-node count —
// clamped to [1, maxShards]. With no cluster view (single-instance dev) the
// count is 1 and the task executes inline.
func (s *Scheduler) shardCount(task *model.Task) int {
	n := 1
	if s.cluster != nil {
		n = s.cluster.AliveCount()
	}
	if task.ShardTotal > 0 {
		n = task.ShardTotal
	}
	if n > s.maxShards {
		n = s.maxShards
	}
	if n < 1 {
		n = 1
	}
	return n
}

// maybeFinalizeShardParent hooks the broadcast parent completion: a shard
// child that just reached a terminal state may be the last one — the repo's
// conditional update lets exactly one finalizer close the parent.
func (s *Scheduler) maybeFinalizeShardParent(task *model.Task) {
	if task.ParentTaskID == nil {
		return
	}
	finalized, status, err := repo.TryFinalizeShardParent(*task.ParentTaskID)
	if err != nil {
		slog.Error("[SCHEDULER] shard parent finalize failed", "parent", *task.ParentTaskID, "err", err)
		return
	}
	if !finalized {
		return
	}
	logStatus := "success"
	if status == "failed" {
		logStatus = "failed"
	}
	_ = repo.CreateTaskLog(&model.TaskLog{
		LogID:    uuid.NewString(),
		TaskID:   *task.ParentTaskID,
		Executor: "broadcast",
		Status:   logStatus,
		Node:     s.owner,
	})
	slog.Info("[SCHEDULER] shard broadcast parent finalized", "task_id", *task.ParentTaskID, "status", status)
}

// markFailedOrRetry applies the task's retry policy to a failed dispatch:
// schedule a retry (back to pending with retry_count+1 and next_retry_at at
// an exponential backoff, capped) while retries remain, otherwise fail. The
// outcome is always recorded in task_log. All transitions are fenced by the
// dispatching claim this worker holds (and clear it).
func (s *Scheduler) markFailedOrRetry(task *model.Task, err error, start time.Time) {
	duration := time.Since(start).Milliseconds()
	errMsg := err.Error()
	if task.MaxRetry > 0 && task.RetryCount < task.MaxRetry {
		shift := task.RetryCount
		if shift > 30 {
			shift = 30
		}
		backoff := time.Duration(1<<uint(shift)) * time.Second // 1s, 2s, 4s…
		if backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
		next := time.Now().UTC().Add(backoff)
		ok, _ := repo.FinalizeClaim(task.TaskID, task.ClaimToken, "pending",
			map[string]any{"retry_count": task.RetryCount + 1, "next_retry_at": next, "owner": "", "claimed_at": nil})
		if ok {
			s.writeLog(task, "retry", duration, "", errMsg)
			slog.Warn("[SCHEDULER] retry scheduled", "task_id", task.TaskID, "task_type", task.TaskType, "retry", task.RetryCount+1, "next_retry_at", formatBeijing(next), "err", errMsg)
			return
		}
		s.logOrphan(task, "failure")
		return
	}
	if ok, _ := repo.FinalizeClaim(task.TaskID, task.ClaimToken, "failed", map[string]any{"owner": "", "claimed_at": nil}); ok {
		s.writeLog(task, "failed", duration, "", errMsg)
		s.maybeFinalizeShardParent(task)
		if task.AlertChannel != "" && s.notify != nil {
			go s.sendFailureAlert(task, errMsg) // 不阻塞派发池：LLM 调用秒级到十秒级
		}
		slog.Error("[SCHEDULER] task failed", "task_id", task.TaskID, "task_type", task.TaskType, "err", errMsg)
	} else {
		s.logOrphan(task, "failure")
	}
}

// writeLog appends one execution record to task_log (溯源).
func (s *Scheduler) writeLog(task *model.Task, status string, durationMS int64, response, errMsg string) {
	var errField *string
	if errMsg != "" {
		errField = &errMsg
	}
	_ = repo.CreateTaskLog(&model.TaskLog{
		LogID:      uuid.NewString(),
		TaskID:     task.TaskID,
		Executor:   executorName(task),
		Request:    truncate(string(task.Payload), 1000),
		Response:   truncate(response, 2000),
		Status:     status,
		DurationMS: durationMS,
		Node:       task.Owner,
		Error:      errField,
	})
}

func executorName(task *model.Task) string {
	if task.TaskType == "lua" {
		return "lua"
	}
	if task.ExecutorURL != "" {
		return task.ExecutorURL
	}
	return task.TaskType
}

// taskFromTask adapts the persisted task into the generic executor.Task view.
// Handlers never see the concrete model — only ID/payload/meta.
func taskFromTask(j *model.Task) executor.Task {
	return executor.Task{
		ID:      j.TaskID,
		Payload: j.Payload,
		Meta: map[string]any{
			"uid":          j.UID,
			"status":       j.Status,
			"task_type":    j.TaskType,
			"executor_url": j.ExecutorURL,
			"async":        j.Async,
			"retry_count":  j.RetryCount,
			"owner":        j.Owner, // dispatching node identity (visible to executors for audit)
			"shard_index":  j.ShardIndex,
			"shard_total":  j.ShardTotal,
		},
	}
}

// extendCronTasks clones terminal cron tasks into their next pending
// occurrence. ExtendCronTask is atomic (conditional link + insert in one
// transaction), so concurrent instances never produce orphan duplicates.
func (s *Scheduler) extendCronTasks() {
	toExtend, err := repo.ListCronTasksToExtend(50)
	if err != nil {
		slog.Error("[SCHEDULER] list cron extend failed", "err", err)
		return
	}
	for _, j := range toExtend {
		next, err := NextTriggerForTask(j.CronExpr, time.Now(), j.CalendarID)
		if err != nil {
			slog.Error("[SCHEDULER] cron parse failed", "task_id", j.TaskID, "cron_expr", j.CronExpr, "err", err)
			continue
		}
		newID, won, err := repo.ExtendCronTask(&j, next)
		if err != nil {
			slog.Error("[SCHEDULER] create next cron task failed", "task_id", j.TaskID, "err", err)
			continue
		}
		if !won {
			continue // another instance extended it first
		}
		slog.Info("[SCHEDULER] cron extended", "task_id", j.TaskID, "next_task_id", newID, "next", formatBeijing(next))
	}
}
