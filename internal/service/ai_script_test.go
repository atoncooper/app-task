package service

// AI 生成 Lua 脚本测试：围栏提取、编译校验门、一次自动修复重试、限速。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"app-task/internal/executor"
)

// llmReplyServer returns an LLM endpoint with a canned reply body.
func llmReplyServer(t *testing.T, reply string, lastPrompt *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastPrompt != nil {
			b := readBody(r)
			*lastPrompt = b
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(reply) + `}}]}`))
	}))
}

// jsonString encodes s as a JSON string value.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestExtractLuaFence(t *testing.T) {
	fence := "```"
	reply := "检查接口并重试。\n" + fence + "lua\nfunction handle(ctx)\n  ctx.log(\"hi\")\nend\n" + fence
	code, expl := extractLuaFence(reply)
	if !strings.Contains(code, "handle(ctx)") || !strings.Contains(code, "ctx.log") {
		t.Fatalf("code extraction broken: %q", code)
	}
	if strings.Contains(code, fence) {
		t.Fatal("fence markers leaked into code")
	}
	if expl != "检查接口并重试。" {
		t.Fatalf("explanation = %q", expl)
	}
	if c, _ := extractLuaFence("no fence at all"); c != "" {
		t.Fatalf("fenceless reply must yield empty code, got %q", c)
	}
}

func TestScriptGenCompileGateAndRetry(t *testing.T) {
	setupTestDB(t)
	// 第一轮回复：语法错误；第二轮回复：合法脚本
	bad := "第一版。\n```lua\nfunction handle(ctx\nend\n```"
	good := "第二版 OK。\n```lua\nfunction handle(ctx)\n  ctx.log(\"ok\")\nend\n```"
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prompts = append(prompts, readBody(r))
		if len(prompts) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(bad) + `}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(good) + `}}]}`))
	}))
	defer srv.Close()

	svc := NewScriptGenService(NewLLMClient(aiCfg(srv.URL)))
	res, err := svc.Generate("admin", "每分钟检查订单接口")
	if err != nil {
		t.Fatalf("retry path must succeed: %v", err)
	}
	if !strings.Contains(res.Code, "handle(ctx)") || !strings.Contains(res.Explanation, "第二版") {
		t.Fatalf("result wrong: %+v", res)
	}
	// 第二次调用的 prompt 必须包含第一次的编译错误
	if len(prompts) != 2 || !strings.Contains(prompts[1], "编译报错") {
		t.Fatalf("fix prompt missing compile error: %d prompts", len(prompts))
	}
}

func TestScriptGenCompileFailSurfaces(t *testing.T) {
	setupTestDB(t)
	bad := "```lua\nfunction handle(ctx\nend\n```"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + jsonString(bad) + `}}]}`))
	}))
	defer srv.Close()
	svc := NewScriptGenService(NewLLMClient(aiCfg(srv.URL)))
	if _, err := svc.Generate("admin", "任意需求"); err == nil || !strings.Contains(err.Error(), "编译失败") {
		t.Fatalf("persistent compile failure must surface: %v", err)
	}
}

func TestScriptGenDisabledAndLimits(t *testing.T) {
	setupTestDB(t)
	svc := NewScriptGenService(nil) // AI 未配置
	if _, err := svc.Generate("admin", "需求"); err == nil || !strings.Contains(err.Error(), "AI 未启用") {
		t.Fatalf("nil LLM must fail loud: %v", err)
	}
	// 限速：每分钟 5 次
	svc2 := NewScriptGenService(NewLLMClient(aiCfg("http://127.0.0.1:1"))) // 永连不上
	for i := 0; i < 5; i++ {
		_, err := svc2.Generate("user-a", "需求") // 网络错误先于限速判断？限速在最前
		if err != nil && strings.Contains(err.Error(), "生成太频繁") {
			t.Fatalf("call %d unexpectedly rate limited", i+1)
		}
	}
	if _, err := svc2.Generate("user-a", "需求"); err == nil || !strings.Contains(err.Error(), "生成太频繁") {
		t.Fatal("6th call must be rate limited")
	}
	if _, err := svc2.Generate("user-b", "需求"); err == nil || strings.Contains(err.Error(), "生成太频繁") {
		t.Fatal("other operator must have own window")
	}
	// 编译门与 executor 沙箱一致
	if _, err := executor.CompileForValidation("function handle(ctx\nend"); err == nil {
		t.Fatal("broken syntax must fail validation")
	}
}
