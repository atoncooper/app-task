package bench

// Integration tests against a REAL MySQL 8 over real TCP — the complement to
// concurrency_test.go, which runs on in-memory SQLite and therefore cannot
// exercise:
//
//   - InnoDB row locks: ClaimTasksBatch's FOR UPDATE SKIP LOCKED candidates
//   - strict-mode DDL/DML (NO_ZERO_DATE etc.) — the Error-1292 zero-datetime
//     bug shipped precisely because SQLite accepts what MySQL rejects
//   - the full time pipeline over the wire: time.Time -> DSN loc=UTC ->
//     DATETIME(3) text -> parseTime -> time.Time (instant must round-trip)
//   - real network IO dispatch: scheduler -> HTTP executor -> callback finalize
//
// Gated by APPTASK_TEST_MYSQL_DSN (skipped when unset — plain `go test ./...`
// stays hermetic). One-shot test database:
//
//	docker compose -f docker-compose.test.yml up -d --wait
//	APPTASK_TEST_MYSQL_DSN='mysql://app_task:app-task@127.0.0.1:13306/app_task_test' \
//	  go test ./bench/ -run 'TestMySQL' -v
//	docker compose -f docker-compose.test.yml down
//
// Invariants re-proven here on MySQL semantics:
//
//	I1 claim exclusivity — concurrent ClaimTasksBatch claim disjoint sets
//	I2 fenced finalize   — stale tokens are rejected after a real reclaim
//	I3 at-least-once     — a reclaimed in-flight dispatch IS re-executed with
//	                       the same X-Task-Id, and the stale finalize loses

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"app-task/internal/db"
	"app-task/internal/executor"
	"app-task/internal/model"
	"app-task/internal/repo"
	"app-task/internal/service"

	"gorm.io/datatypes"
)

const mysqlDSNEnv = "APPTASK_TEST_MYSQL_DSN"

// setupMySQLDB points db.DB at the real MySQL from APPTASK_TEST_MYSQL_DSN,
// migrates the full schema (exercising the startup DDL path on strict mode)
// and truncates every table for test isolation. Retries the connection so a
// freshly started container (first-boot init takes ~20s) is tolerated.
func setupMySQLDB(t *testing.T) {
	t.Helper()
	dsn := os.Getenv(mysqlDSNEnv)
	if dsn == "" {
		t.Skipf("%s not set — MySQL integration tests skipped (see docker-compose.test.yml)", mysqlDSNEnv)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := db.Init(db.Options{DSN: dsn, MaxOpenConns: 25, MaxIdleConns: 5})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connect mysql: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Cleanup(func() {
		db.Close()
		db.DB = nil
	})
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, table := range []string{
		"task_log", "task", "email_queue", "script_log", "script_run",
		"script", "api_key", "secret", "webui_user", "cluster_node",
	} {
		if err := db.DB.Exec("TRUNCATE TABLE " + table).Error; err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	// Sanity: the suite's guarantees assume MySQL 8 (SKIP LOCKED needs 8.0+).
	var version string
	if err := db.DB.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		t.Fatalf("read version: %v", err)
	}
	if len(version) < 1 || version[0] < '8' {
		t.Fatalf("mysql %q: SKIP LOCKED requires MySQL 8+", version)
	}
}

// seedMySQLDueTasks inserts n due pending tasks pointing at executorURL.
func seedMySQLDueTasks(t *testing.T, n int, executorURL string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	tasks := make([]model.Task, n)
	for i := range tasks {
		tasks[i] = model.Task{
			TaskID:      fmt.Sprintf("mysql-task-%d", i),
			TaskType:    "http",
			Payload:     datatypes.JSON(`{}`),
			ExecutorURL: executorURL,
			Status:      "pending",
			TriggerTime: past,
			MaxRetry:    0,
			Weight:      1,
		}
	}
	if err := db.DB.CreateInBatches(&tasks, 500).Error; err != nil {
		t.Fatalf("seed tasks: %v", err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

// ── I1 on real InnoDB: concurrent ClaimTasksBatch under SKIP LOCKED ──────

func TestMySQLBatchClaimExclusivity(t *testing.T) {
	setupMySQLDB(t)
	const (
		claimants = 8
		tasksN    = 300
		batch     = 15
	)
	seedMySQLDueTasks(t, tasksN, "http://mysql-itest-gate")

	var mu sync.Mutex
	claimed := make(map[string]int)
	var wg sync.WaitGroup
	for g := 0; g < claimants; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			owner := fmt.Sprintf("mysql-node-%d", g)
			for {
				rows, err := repo.ClaimTasksBatch(batch, owner, time.Now().UTC())
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(rows) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rows {
					claimed[r.TaskID]++
				}
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(claimed) != tasksN {
		t.Fatalf("distinct claimed tasks = %d, want %d", len(claimed), tasksN)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("task %s claimed %d times, want exactly 1 (SKIP LOCKED overlap)", id, n)
		}
	}
}

// ── I2 on real InnoDB: reclaim + fenced finalize ─────────────────────────

func TestMySQLFencedFinalizeUnderReclaim(t *testing.T) {
	setupMySQLDB(t)
	for round := 0; round < 20; round++ {
		seedMySQLDueTasks(t, 1, "http://mysql-itest-gate")
		id := "mysql-task-0"

		tok1, ok1, err := repo.ClaimTask(id, "stale", time.Now().UTC())
		if err != nil || !ok1 {
			t.Fatalf("round %d: first claim = %v %v", round, ok1, err)
		}
		if n, err := repo.ReclaimDispatching(time.Now().UTC().Add(time.Minute)); err != nil || n != 1 {
			t.Fatalf("round %d: reclaim = %d %v", round, n, err)
		}
		tok2, ok2, err := repo.ClaimTask(id, "fresh", time.Now().UTC())
		if err != nil || !ok2 {
			t.Fatalf("round %d: re-claim = %v %v", round, ok2, err)
		}

		if ok, _ := repo.FinalizeClaim(id, tok1, "completed", nil); ok {
			t.Fatalf("round %d: stale token finalized over the fresh claim", round)
		}
		if ok, _ := repo.FinalizeClaim(id, tok2, "completed", nil); !ok {
			t.Fatalf("round %d: fresh holder finalize rejected", round)
		}
		db.DB.Where("task_id = ?", id).Delete(&model.Task{})
	}
}

// ── Time pipeline over the wire + the Error-1292 zero-datetime regression ─

func TestMySQLTimeRoundTrip(t *testing.T) {
	setupMySQLDB(t)

	// trigger_time must round-trip as the SAME instant (ms precision): the
	// write path serializes via the driver's loc=UTC session, the read path
	// parses back into UTC. A loc/TZ mismatch shifts the instant by the
	// zone offset — exactly the failure class this guards against.
	trigger := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	task := model.Task{
		TaskID: "mysql-tz-task", TaskType: "http", Payload: datatypes.JSON(`{}`),
		ExecutorURL: "http://mysql-itest-gate", Status: "pending",
		TriggerTime: trigger, Weight: 1,
	}
	if err := db.DB.Create(&task).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.GetTaskByID("mysql-tz-task")
	if err != nil || got == nil {
		t.Fatalf("read back: %v, %v", got, err)
	}
	if got.TriggerTime.Location() != time.UTC {
		t.Fatalf("read-back location = %v, want UTC (DSN loc must pin UTC)", got.TriggerTime.Location())
	}
	if d := got.TriggerTime.Sub(trigger); d != 0 {
		t.Fatalf("trigger_time drifted %v over the round trip", d)
	}

	// Regression (Error 1292): pre-registering a node persists Go zero
	// time.Time unless JoinNode fills them — strict mode rejects
	// '0000-00-00'. The SQLite suite cannot see this.
	if created, err := repo.JoinNode(&model.ClusterNode{
		NodeID: "mysql-join-node", Weight: 2,
		State: repo.NodeStatePendingJoin, JoinedVia: "web", InvitedBy: "itest",
	}); err != nil || !created {
		t.Fatalf("join node = %v, %v (zero datetime rejected by strict mode?)", created, err)
	}
	row, err := repo.GetClusterNode("mysql-join-node")
	if err != nil || row == nil {
		t.Fatalf("joined row: %v, %v", row, err)
	}
	if row.StartedAt.IsZero() || row.LastHeartbeat.IsZero() {
		t.Fatalf("joined row has zero time: %v %v", row.StartedAt, row.LastHeartbeat)
	}

	// Activation overwrites started_at with the first heartbeat's instant.
	hb := time.Now().UTC().Truncate(time.Millisecond)
	if err := repo.UpsertNodeHeartbeat(&model.ClusterNode{
		NodeID: "mysql-join-node", Weight: 2, StartedAt: hb, LastHeartbeat: hb,
	}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	row, _ = repo.GetClusterNode("mysql-join-node")
	if row.State != repo.NodeStateActive || row.StartedAt.Sub(hb) != 0 {
		t.Fatalf("activation: state=%s started_at drift %v", row.State, row.StartedAt.Sub(hb))
	}

	// Negative control: raw '0000-00-00' must still be REJECTED — proves the
	// target really is strict-mode MySQL (otherwise the tests above are not
	// exercising the production semantics at all).
	if err := db.DB.Exec(
		"INSERT INTO cluster_node (node_id, state, started_at) VALUES ('raw-zero', 'pending_join', '0000-00-00 00:00:00')",
	).Error; err == nil {
		t.Fatal("zero datetime accepted — target is not strict-mode MySQL, suite is not meaningful")
	}
}

// ── Real network IO: scheduler → HTTP executor end to end ────────────────

func TestMySQLSchedulerDispatchEndToEnd(t *testing.T) {
	setupMySQLDB(t)

	var mu sync.Mutex
	var got []string // X-Task-Id headers, in arrival order
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("X-Task-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	const tasksN = 5
	seedMySQLDueTasks(t, tasksN, srv.URL)

	reg := executor.NewRegistry()
	reg.Register("http", executor.NewHTTPExecutor(executor.HTTPOptions{Timeout: 5 * time.Second}).Handler())
	sched := service.NewScheduler(reg, service.SchedulerOptions{
		Interval:       20 * time.Millisecond,
		Workers:        8,
		BatchSize:      10,
		DispatchingTTL: time.Minute,
		Owner:          "mysql-itest",
	})
	sched.Start()
	defer sched.Stop()

	waitFor(t, 15*time.Second, "all tasks completed", func() bool {
		var n int64
		db.DB.Model(&model.Task{}).Where("status = ?", "completed").Count(&n)
		return n == tasksN
	})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != tasksN {
		t.Fatalf("executor received %d requests, want %d", len(got), tasksN)
	}
	ids := make(map[string]bool)
	for _, id := range got {
		if id == "" {
			t.Fatal("dispatch missing X-Task-Id header (executor dedup key)")
		}
		ids[id] = true
	}
	if len(ids) != tasksN {
		t.Fatalf("X-Task-Id set = %v, want the %d seeded ids", ids, tasksN)
	}
	var logs int64
	db.DB.Model(&model.TaskLog{}).Where("status = ?", "success").Count(&logs)
	if logs != tasksN {
		t.Fatalf("task_log success rows = %d, want %d", logs, tasksN)
	}
	// Node traceability over real MySQL: every row records the executing node.
	var noNode int64
	db.DB.Model(&model.TaskLog{}).Where("node = ? OR node = ''", "mysql-itest").Count(&noNode)
	if noNode != tasksN {
		t.Fatalf("task_log rows without correct node = %d, want 0", tasksN-noNode)
	}
}

// TestMySQLAtLeastOnceRedelivery proves I3 over real IO: a dispatch that is
// reclaimed WHILE the executor is still processing must be re-executed (same
// X-Task-Id → executor-side idempotency key), and the stalled worker's late
// finalize must lose to the fresh claim (fencing).
func TestMySQLAtLeastOnceRedelivery(t *testing.T) {
	setupMySQLDB(t)

	release := make(chan struct{})
	var mu sync.Mutex
	var deliveries []string
	arrival := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Task-Id")
		mu.Lock()
		deliveries = append(deliveries, id)
		mu.Unlock()
		select {
		case arrival <- id:
		default:
		}
		<-release // hold the request open so the reclaim below wins the race deterministically
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	seedMySQLDueTasks(t, 1, srv.URL)

	reg := executor.NewRegistry()
	reg.Register("http", executor.NewHTTPExecutor(executor.HTTPOptions{Timeout: 30 * time.Second}).Handler())
	sched := service.NewScheduler(reg, service.SchedulerOptions{
		Interval:       20 * time.Millisecond,
		Workers:        4,
		BatchSize:      10,
		DispatchingTTL: time.Minute,
		Owner:          "mysql-itest",
	})
	sched.Start()
	defer sched.Stop()

	// First delivery arrives and is parked in the executor handler.
	select {
	case <-arrival:
	case <-time.After(10 * time.Second):
		t.Fatal("first dispatch never arrived")
	}
	// Reclaim the in-flight claim (simulated TTL expiry / dead-node takeover):
	// the task goes back to pending while execution #1 is still running.
	if n, err := repo.ReclaimDispatching(time.Now().UTC().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("reclaim = %d %v, want 1", n, err)
	}
	close(release) // execution #1 finishes; its finalize must be fenced out

	waitFor(t, 15*time.Second, "re-delivery + final completion", func() bool {
		var completed int64
		db.DB.Model(&model.Task{}).Where("status = ?", "completed").Count(&completed)
		return completed == 1
	})

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %d (%v), want exactly 2 — at-least-once redelivery broken", len(deliveries), deliveries)
	}
	if deliveries[0] != deliveries[1] || deliveries[0] == "" {
		t.Fatalf("X-Task-Id changed across redelivery: %q vs %q (executor idempotency key unstable)", deliveries[0], deliveries[1])
	}
	// Only the SECOND (token-valid) execution may leave a success log; the
	// orphaned first attempt is logged via slog only.
	var logs int64
	db.DB.Model(&model.TaskLog{}).Where("task_id = ? AND status = ?", "mysql-task-0", "success").Count(&logs)
	if logs != 1 {
		t.Fatalf("success task_log rows = %d, want 1 (orphaned attempt must not write)", logs)
	}
}
