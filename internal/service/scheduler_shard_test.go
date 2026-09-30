package service

// 分片广播端到端：触发行 → 分裂成 N 片 → 各片独立派发执行 → 父任务收尾。

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"app-task/internal/executor"
	"app-task/internal/repo"
)

// shardBroadcastFixture: a shard-broadcast task whose executor records the
// X-Shard-Index headers it receives.
type shardBroadcastFixture struct {
	svc    *TaskService
	sched  *Scheduler
	taskID string
	mu     sync.Mutex
	got    map[string]int  // X-Shard-Index -> count
	fail   map[string]bool // shard index (string) -> respond 500
}

func newShardBroadcastFixture(t *testing.T, maxRetry int) *shardBroadcastFixture {
	t.Helper()
	setupTestDB(t)
	f := &shardBroadcastFixture{got: map[string]int{}, fail: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := r.Header.Get("X-Shard-Index")
		f.mu.Lock()
		f.got[idx]++
		fail := f.fail[idx]
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	f.svc = NewTaskService()
	f.taskID, _ = f.svc.RegisterTask(RegisterOptions{
		TaskType: "http", Payload: []byte(`{}`), ExecutorURL: srv.URL,
		TriggerTime: time.Now().UTC().Add(-time.Minute), MaxRetry: maxRetry,
		Shard: true,
	})
	reg := executor.NewRegistry()
	reg.Register("http", mustHTTPExecutor(t).Handler())
	f.sched = NewScheduler(reg, SchedulerOptions{Interval: 30 * time.Second, Owner: "node-a"})
	f.sched.SetClusterView(stubClusterView{alive: 3})
	return f
}

func (f *shardBroadcastFixture) runUntilParent(t *testing.T, status string) {
	t.Helper()
	for i := 0; i < 8; i++ {
		f.sched.tick()
		parent, _ := repo.GetTaskByID(f.taskID)
		if parent != nil && parent.Status == status {
			return
		}
	}
	parent, _ := repo.GetTaskByID(f.taskID)
	got := "nil"
	if parent != nil {
		got = parent.Status
	}
	t.Fatalf("parent never reached %s (status=%s, requests=%v)", status, got, f.got)
}

func TestSchedulerShardBroadcast(t *testing.T) {
	f := newShardBroadcastFixture(t, 0)
	f.runUntilParent(t, "completed")

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 3 {
		t.Fatalf("executor received %d distinct shards (%v), want 3", len(f.got), f.got)
	}
	for _, idx := range []string{"0", "1", "2"} {
		if f.got[idx] != 1 {
			t.Fatalf("shard %s received %d times, want 1", idx, f.got[idx])
		}
	}
	parent, _ := repo.GetTaskByID(f.taskID)
	if parent.ShardTotal != 3 {
		t.Fatalf("parent shard_total = %d, want 3", parent.ShardTotal)
	}
	children, _ := repo.ListShardChildren(f.taskID)
	if len(children) != 3 {
		t.Fatalf("children = %d, want 3", len(children))
	}
	for i := range children {
		if children[i].ShardIndex != i || children[i].Status != "completed" {
			t.Fatalf("child %d = %s", children[i].ShardIndex, children[i].Status)
		}
	}
	// traceability: parent carries split + completion log rows
	logs, _ := repo.ListTaskLogs(f.taskID, 10)
	var hasAccepted, hasSuccess bool
	for i := range logs {
		if logs[i].Status == "accepted" {
			hasAccepted = true
		}
		if logs[i].Status == "success" {
			hasSuccess = true
		}
	}
	if !hasAccepted || !hasSuccess {
		t.Fatalf("parent log rows missing split/completion entries")
	}
}

func TestSchedulerShardBroadcastFailure(t *testing.T) {
	f := newShardBroadcastFixture(t, 0)
	f.fail["1"] = true // shard 1 fails (no retry) → parent must fail
	f.runUntilParent(t, "failed")

	children, _ := repo.ListShardChildren(f.taskID)
	var failed int
	for i := range children {
		if children[i].Status == "failed" {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("failed children = %d, want 1", failed)
	}
}

func TestSchedulerShardBroadcastAsyncRejected(t *testing.T) {
	setupTestDB(t)
	svc := NewTaskService()
	if _, err := svc.RegisterTask(RegisterOptions{
		TaskType: "http", ExecutorURL: "http://x", TriggerTime: time.Now().UTC(),
		Shard: true, Async: true,
	}); err == nil {
		t.Fatal("shard broadcast + async must be rejected at creation")
	}
}

func TestSchedulerShardSingleAliveNode(t *testing.T) {
	// 存活节点数 = 1：退化为普通执行，不分裂。
	setupTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	svc := NewTaskService()
	taskID, _ := svc.RegisterTask(RegisterOptions{
		TaskType: "http", Payload: []byte(`{}`), ExecutorURL: srv.URL,
		TriggerTime: time.Now().UTC().Add(-time.Minute), Shard: true,
	})
	reg := executor.NewRegistry()
	reg.Register("http", mustHTTPExecutor(t).Handler())
	sched := NewScheduler(reg, SchedulerOptions{Interval: 30 * time.Second, Owner: "node-a"})
	sched.SetClusterView(stubClusterView{alive: 1})
	sched.tick()
	task, _ := repo.GetTaskByID(taskID)
	if task.Status != "completed" {
		t.Fatalf("status = %q, want completed (single node must not split)", task.Status)
	}
	if task.ShardTotal != 0 {
		t.Fatalf("shard_total = %d, want 0 (no split happened)", task.ShardTotal)
	}
	children, _ := repo.ListShardChildren(taskID)
	if len(children) != 0 {
		t.Fatalf("children = %d, want 0", len(children))
	}
}
