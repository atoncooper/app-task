package service

// 失败告警 + AI 根因测试：最终失败才告警（重试中不发）、AI 诊断拼接、
// 未配置渠道不发送、payload 默认不进 prompt。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"app-task/internal/executor"
	"app-task/internal/repo"
)

// alertFixture: dingtalk channel capturing delivered alerts + optional LLM.
type alertFixture struct {
	svc       *TaskService
	sched     *Scheduler
	taskID    string
	alerts    []string
	llmPrompt *string
}

func newAlertFixture(t *testing.T, maxRetry int, llmURL string) *alertFixture {
	t.Helper()
	setupTestDB(t)
	f := &alertFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.alerts = append(f.alerts, readBody(r))
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	mustChannel(t, "alert-ding", repo.ChannelTypeDingTalk, `{"webhook":"`+srv.URL+`"}`, testCipher(t))

	if llmURL == "fake" {
		f.llmPrompt = new(string)
		llmSrv := fakeLLMServer(t, f.llmPrompt)
		t.Cleanup(llmSrv.Close)
		llmURL = llmSrv.URL
	}
	var llm *LLMClient
	if llmURL != "" {
		llm = NewLLMClient(aiCfg(llmURL))
	}

	f.svc = NewTaskService()
	taskID, err := f.svc.RegisterTask(RegisterOptions{
		TaskType: "http", Payload: []byte(`{"secret_key":"a-1"}`), ExecutorURL: "http://127.0.0.1:1/unreachable",
		TriggerTime: time.Now().UTC().Add(-time.Minute), MaxRetry: maxRetry,
		AlertChannel: "alert-ding",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.taskID = taskID
	reg := executor.NewRegistry()
	reg.Register("http", mustHTTPExecutor(t).Handler())
	f.sched = NewScheduler(reg, SchedulerOptions{Interval: 30 * time.Second, Owner: "node-x",
		Notify: NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0), LLM: llm})
	return f
}

func (f *alertFixture) runTickAndSettle(t *testing.T) {
	t.Helper()
	f.sched.tick()
	time.Sleep(300 * time.Millisecond) // alert goroutine
}

func TestFailureAlertOnFinalFailure(t *testing.T) {
	f := newAlertFixture(t, 0, "")
	f.runTickAndSettle(t)
	if len(f.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(f.alerts))
	}
	if !strings.Contains(f.alerts[0], f.taskID) {
		t.Fatalf("alert missing task id: %s", f.alerts[0])
	}
	if strings.Contains(f.alerts[0], "AI 诊断") {
		t.Fatalf("nil LLM must not add RCA: %s", f.alerts[0])
	}
}

func TestFailureAlertNoAlertDuringRetry(t *testing.T) {
	f := newAlertFixture(t, 2, "") // 2 retries before final failure
	f.runTickAndSettle(t)
	if len(f.alerts) != 0 {
		t.Fatalf("alerts during retry = %d, want 0", len(f.alerts))
	}
}

func TestFailureAlertWithRCA(t *testing.T) {
	f := newAlertFixture(t, 0, "fake")
	f.runTickAndSettle(t)
	if len(f.alerts) != 1 {
		t.Fatalf("alerts = %d, want 1", len(f.alerts))
	}
	if !strings.Contains(f.alerts[0], "AI 诊断") || !strings.Contains(f.alerts[0], "一切正常") {
		t.Fatalf("RCA section missing: %s", f.alerts[0])
	}
	if f.llmPrompt == nil || !strings.Contains(*f.llmPrompt, "unreachable") {
		t.Fatalf("RCA prompt missing error context: %v", f.llmPrompt)
	}
	// 隐私默认：payload 不进 prompt（send_payload_data=false）
	if strings.Contains(*f.llmPrompt, "a-1") {
		t.Fatal("payload leaked into RCA prompt by default")
	}
}

func TestFailureAlertCarriesNode(t *testing.T) {
	f := newAlertFixture(t, 0, "")
	f.runTickAndSettle(t)
	if len(f.alerts) == 0 {
		t.Fatal("no alert")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(f.alerts[0]), &body); err != nil {
		t.Fatalf("alert not JSON: %v", err)
	}
	text, _ := body["markdown"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "执行节点: node-x") {
		t.Fatalf("alert missing node traceability: %s", text)
	}
}

func TestNoAlertWithoutChannel(t *testing.T) {
	setupTestDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("alert sent despite empty alert_channel")
	}))
	defer srv.Close()
	mustChannel(t, "unused", repo.ChannelTypeDingTalk, `{"webhook":"`+srv.URL+`"}`, testCipher(t))

	svc := NewTaskService()
	taskID, err := svc.RegisterTask(RegisterOptions{
		TaskType: "http", ExecutorURL: "http://127.0.0.1:1/x",
		TriggerTime: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := executor.NewRegistry()
	reg.Register("http", mustHTTPExecutor(t).Handler())
	sched := NewScheduler(reg, SchedulerOptions{Interval: 30 * time.Second, Owner: "n1",
		Notify: NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0)})
	sched.tick()
	_ = taskID
	time.Sleep(200 * time.Millisecond)
}
