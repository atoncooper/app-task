package service

// digest 任务类型测试：统计窗口、AI 总结与模板降级、渠道投递。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"app-task/internal/config"
	"app-task/internal/executor"
	"app-task/internal/model"
	"app-task/internal/repo"
)

func testErrPtr(s string) *string { return &s }

// seedDigestLogs inserts a success, a failed (×2) and a slow execution, plus
// one out-of-window row that must never appear in stats.
func seedDigestLogs(t *testing.T) {
	t.Helper()
	now := time.Now().UTC()
	logs := []model.TaskLog{
		{LogID: "d1", TaskID: "task-ok", Executor: "http://x", Status: "success", TriggerAt: now.Add(-time.Hour)},
		{LogID: "d2", TaskID: "task-bad", Executor: "http://x", Status: "failed", TriggerAt: now.Add(-2 * time.Hour), Error: testErrPtr("connection refused")},
		{LogID: "d3", TaskID: "task-bad", Executor: "http://x", Status: "failed", TriggerAt: now.Add(-3 * time.Hour), Error: testErrPtr("connection refused")},
		{LogID: "d4", TaskID: "task-ok", Executor: "http://x", Status: "success", DurationMS: 9000, TriggerAt: now.Add(-4 * time.Hour), Node: "node-1"},
		{LogID: "d5", TaskID: "task-old", Executor: "http://x", Status: "failed", TriggerAt: now.Add(-72 * time.Hour)},
	}
	for i := range logs {
		if err := repo.CreateTaskLog(&logs[i]); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeLLMServer returns an OpenAI-shaped endpoint capturing the user prompt.
func fakeLLMServer(t *testing.T, promptOut *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err == nil {
			if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
				if last, ok := msgs[len(msgs)-1].(map[string]any); ok {
					*promptOut, _ = last["content"].(string)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"AI 总结：一切正常。"}}]}`))
	}))
}

// readBody consumes a request body as a string.
func readBody(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// aiCfg points the AI config at the fake server.
func aiCfg(baseURL string) *config.AIConfig {
	return &config.AIConfig{BaseURL: baseURL, Model: "test-model", APIKey: "sk-test", TimeoutSeconds: 5}
}

// dingtalkOKEndpoint answers the errcode==0 body dingtalk robots return.
func dingtalkOKEndpoint() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
}

func TestDigestTemplateFallback(t *testing.T) {
	setupTestDB(t)
	seedDigestLogs(t)
	srv := dingtalkOKEndpoint()
	defer srv.Close()
	mustChannel(t, "digest-ding", repo.ChannelTypeDingTalk, `{"webhook":"`+srv.URL+`"}`, testCipher(t))

	// AI 未配置（nil client）→ 模板降级且投递成功
	h := NewDigestService(NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0), nil).Handler()
	if err := h(nil, executor.Task{ID: "digest-1", Payload: []byte(`{"channel":"digest-ding"}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestDigestAISummary(t *testing.T) {
	setupTestDB(t)
	seedDigestLogs(t)
	var prompt string
	llmSrv := fakeLLMServer(t, &prompt)
	defer llmSrv.Close()

	srv := dingtalkOKEndpoint()
	defer srv.Close()
	mustChannel(t, "digest-ai", repo.ChannelTypeDingTalk, `{"webhook":"`+srv.URL+`"}`, testCipher(t))

	h := NewDigestService(NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0), NewLLMClient(aiCfg(llmSrv.URL))).Handler()
	if err := h(nil, executor.Task{ID: "digest-2", Payload: []byte(`{"channel":"digest-ai","period":"daily","top_n":3}`)}); err != nil {
		t.Fatal(err)
	}
	// prompt 必须包含统计关键数字，且不包含窗口外的旧数据
	for _, want := range []string{"\"total\":4", "task-bad", "9000", "\"failed\":2"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %s: %s", want, prompt)
		}
	}
	if strings.Contains(prompt, "task-old") {
		t.Fatal("prompt includes out-of-window data")
	}
}

func TestDigestAIMailureFallsBack(t *testing.T) {
	setupTestDB(t)
	seedDigestLogs(t)
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer llmSrv.Close()

	var delivered string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered = readBody(r)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()
	mustChannel(t, "digest-fb", repo.ChannelTypeDingTalk, `{"webhook":"`+srv.URL+`"}`, testCipher(t))

	h := NewDigestService(NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0), NewLLMClient(aiCfg(llmSrv.URL))).Handler()
	if err := h(nil, executor.Task{ID: "digest-3", Payload: []byte(`{"channel":"digest-fb"}`)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(delivered, "AI 总结不可用") || !strings.Contains(delivered, "过去") {
		t.Fatalf("fallback text missing: %s", delivered)
	}
}

func TestDigestValidation(t *testing.T) {
	setupTestDB(t)
	h := NewDigestService(NewNotifyService(NewEmailService(emailCfg()), testCipher(t), 0, 0), nil).Handler()
	if err := h(nil, executor.Task{ID: "digest-4", Payload: []byte(`{}`)}); err == nil {
		t.Fatal("missing channel must error")
	}
	if err := h(nil, executor.Task{ID: "digest-5", Payload: []byte(`{"channel":"x","period":"hourly"}`)}); err == nil {
		t.Fatal("bad period must error")
	}
}
