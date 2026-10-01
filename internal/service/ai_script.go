package service

// AI Lua 脚本生成：需求描述 → LLM → 提取代码围栏 → 真实编译校验 → 返回给
// 编辑器（不自动保存）。安全三层：AI 只产出文本（保存/启用/试运行全走
// 现有人工路径）、沙箱 VM 禁 os/io/debug（AI 写了也跑不了系统调用）、
// 端点 admin 门禁 + 会话级限速防刷账单。

import (
	"context"
	"fmt"
	"strings"

	"app-task/internal/executor"
)

// luaGenSystemPrompt embeds the sandbox API contract — the model may only
// write against this surface, which is what makes generated code reviewable
// and safe. (The fence marker is built from runes so the Go source stays
// parseable.)
var luaGenSystemPrompt = "你是 app-task 平台的 Lua 脚本专家。为内置 Lua 执行器编写脚本，必须遵守：\n" +
	"1. 定义全局函数 handle(ctx)，平台每次执行调用它一次；脚本顶层只放定义。\n" +
	"2. ctx API（仅这些可用）：ctx.log(msg) 记日志；ctx.payload() 读任务 payload（table 或 nil）；\n" +
	"   ctx.task() 返回 {id, meta}；ctx.http_get(url) 返回 body,status 或 nil,err；\n" +
	"   ctx.http_post(url, body, content_type) 同上；\n" +
	"   ctx.http_req{method=,url=,body=,content_type=,headers=} 同上；\n" +
	"   ctx.retry(msg) 可重试失败；ctx.fail(msg) 硬失败；ctx.secret(name) 取密钥。\n" +
	"3. cjson.encode/decode 可用；标准库仅 string/table/math；没有 os/io——不要尝试读写文件或环境变量。\n" +
	"4. 平台是 at-least-once：脚本可能被重复执行，必须幂等。\n" +
	"5. 网络失败用 ctx.retry（可重试），业务校验失败用 ctx.fail。\n" +
	"6. 输出：先一行中文说明（不超过 80 字），然后一个 " + luaFenceStart + " 围栏代码块，只含脚本。"

// fence markers (assembled from runes: backticks cannot appear in a Go raw string)
var (
	luaFenceStart = strings.Repeat("`", 3) + "lua"
	luaFenceEnd   = strings.Repeat("`", 3)
)

// ScriptGenService serves the editor's AI button.
type ScriptGenService struct {
	llm *LLMClient
	rl  *channelLimiter // reuse the sliding-window limiter (per-operator key)
}

// NewScriptGenService builds the generator; nil LLM disables the endpoint.
func NewScriptGenService(llm *LLMClient) *ScriptGenService {
	return &ScriptGenService{llm: llm, rl: newChannelLimiter(5)}
}

// GenResult carries the extracted code plus the model's one-line explanation.
type GenResult struct {
	Code        string `json:"code"`
	Explanation string `json:"explanation"`
}

// Generate turns a requirement into compile-checked Lua source. When the
// model output does not build, it retries once feeding the compile error
// back; a persistent failure surfaces the compile error to the user.
func (s *ScriptGenService) Generate(operator, requirement string) (*GenResult, error) {
	if s.llm == nil || !s.llm.Enabled() {
		return nil, fmt.Errorf("AI 未启用（ai.model / ai.api_key 未配置）")
	}
	if !s.rl.allow("gen:" + operator) {
		return nil, fmt.Errorf("生成太频繁（每分钟 5 次），稍后再试")
	}
	requirement = strings.TrimSpace(requirement)
	if requirement == "" || len(requirement) > 2000 {
		return nil, fmt.Errorf("需求描述需为 1–2000 字")
	}
	reply, err := s.llm.Chat(context.Background(), luaGenSystemPrompt, requirement)
	if err != nil {
		return nil, fmt.Errorf("AI 生成失败：%w", err)
	}
	code, explanation := extractLuaFence(reply)
	if code == "" {
		return nil, fmt.Errorf("AI 未返回代码块，原始回复：%s", truncate(reply, 300))
	}
	if _, err := executor.CompileForValidation(code); err != nil {
		fixPrompt := fmt.Sprintf("%s\n\n你上次输出的代码编译报错：%v\n请重新输出（同样格式：一行说明 + %s 围栏）。\n需求：%s",
			reply, err, luaFenceStart, requirement)
		if reply2, err2 := s.llm.Chat(context.Background(), luaGenSystemPrompt, fixPrompt); err2 == nil {
			if code2, expl2 := extractLuaFence(reply2); code2 != "" {
				_, cerr := executor.CompileForValidation(code2)
				if cerr != nil {
					return nil, fmt.Errorf("AI 代码编译失败（重试后仍报错）：%v", cerr)
				}
				return &GenResult{Code: code2, Explanation: expl2}, nil
			}
		}
		return nil, fmt.Errorf("AI 代码编译失败：%v", err)
	}
	return &GenResult{Code: code, Explanation: explanation}, nil
}

// extractLuaFence pulls the first fenced Lua block and the trailing
// explanation line from the model reply.
func extractLuaFence(reply string) (code, explanation string) {
	i := strings.Index(reply, luaFenceStart)
	fenceLen := len(luaFenceStart)
	if i < 0 {
		i = strings.Index(reply, luaFenceEnd)
		if i < 0 {
			return "", ""
		}
		fenceLen = len(luaFenceEnd)
	}
	rest := reply[i+fenceLen:]
	j := strings.Index(rest, luaFenceEnd)
	if j < 0 {
		return "", ""
	}
	code = strings.TrimSpace(rest[:j])
	explanation = truncate(strings.TrimSpace(strings.TrimSuffix(reply[:i], "\n")), 120)
	if idx := strings.LastIndex(explanation, "\n"); idx >= 0 {
		explanation = strings.TrimSpace(explanation[idx+1:])
	}
	return code, explanation
}
