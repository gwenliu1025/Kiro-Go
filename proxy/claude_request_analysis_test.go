package proxy

import (
	"testing"
)

func TestAnalyzeClaudeRequestWorksWithoutCacheControl(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-sonnet-4-6",
		System: "You are a coding agent.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "先读取仓库结构"},
			{Role: "assistant", Content: "好的"},
			{Role: "user", Content: "然后修改实现"},
		},
	}

	analysis := analyzeClaudeRequest(req, "api-key-1")

	if analysis.EstimatedInputTokens <= 0 {
		t.Fatalf("应生成输入 token 估算，得到 %d", analysis.EstimatedInputTokens)
	}
	if analysis.CacheableTokens <= 0 {
		t.Fatalf("没有 cache_control 时仍应生成可缓存 token")
	}
	if len(analysis.Prefixes) != len(req.Messages) {
		t.Fatalf("应为每条消息生成前缀，得到 %d", len(analysis.Prefixes))
	}
}

func TestAnalyzeClaudeRequestTaskKeyUsesAPIKeyScope(t *testing.T) {
	req := analysisTestRequest()

	first := analyzeClaudeRequest(req, "api-key-1")
	second := analyzeClaudeRequest(req, "api-key-2")

	if first.TaskKey == second.TaskKey {
		t.Fatalf("不同 API Key 作用域不得共享任务键")
	}
	if first.RequestFingerprint != second.RequestFingerprint {
		t.Fatalf("调用方作用域不应改变请求内容指纹")
	}
}

func TestAnalyzeClaudeRequestTaskKeyIgnoresUpstreamAccount(t *testing.T) {
	req := analysisTestRequest()
	var taskKeys [][32]byte

	for range []string{"upstream-a", "upstream-b"} {
		analysis := analyzeClaudeRequest(req, "api-key-1")
		taskKeys = append(taskKeys, analysis.TaskKey)
	}

	if taskKeys[0] != taskKeys[1] {
		t.Fatalf("上游账号切换不得影响任务键")
	}
}

func TestAnalyzeClaudeRequestTaskKeyChangesWithToolsSystemOrFirstUser(t *testing.T) {
	base := analysisTestRequest()
	baseKey := analyzeClaudeRequest(base, "api-key-1").TaskKey

	changedTool := cloneClaudeRequestForAnalysisTest(base)
	changedTool.Tools[0].Description = "读取文件并返回元数据"

	changedSystem := cloneClaudeRequestForAnalysisTest(base)
	changedSystem.System = "You are a careful reviewer."

	changedFirstUser := cloneClaudeRequestForAnalysisTest(base)
	changedFirstUser.Messages[0].Content = "检查测试目录"

	for name, req := range map[string]*ClaudeRequest{
		"工具":     changedTool,
		"系统提示":   changedSystem,
		"首条用户消息": changedFirstUser,
	} {
		if got := analyzeClaudeRequest(req, "api-key-1").TaskKey; got == baseKey {
			t.Fatalf("%s 变化后应生成新任务键", name)
		}
	}
}

func TestAnalyzeClaudeRequestFingerprintChangesWithLaterMessages(t *testing.T) {
	base := analysisTestRequest()
	continued := cloneClaudeRequestForAnalysisTest(base)
	continued.Messages = append(continued.Messages,
		ClaudeMessage{Role: "assistant", Content: "已读取文件"},
		ClaudeMessage{Role: "user", Content: "继续检查测试"},
	)

	first := analyzeClaudeRequest(base, "api-key-1")
	second := analyzeClaudeRequest(continued, "api-key-1")

	if first.TaskKey != second.TaskKey {
		t.Fatalf("后续消息增长不应改变任务键")
	}
	if first.RequestFingerprint == second.RequestFingerprint {
		t.Fatalf("后续消息增长必须改变完整请求指纹")
	}
}

func TestAnalyzeClaudeRequestExcludesCacheControlAndBillingHeader(t *testing.T) {
	build := func(withMetadata bool) *ClaudeRequest {
		system := []interface{}{
			map[string]interface{}{"type": "text", "text": "stable system"},
		}
		content := []interface{}{
			map[string]interface{}{"type": "text", "text": "stable user"},
		}
		if withMetadata {
			system = append([]interface{}{
				map[string]interface{}{
					"type": "text",
					"text": "x-anthropic-billing-header: cc_version=2.1; cch=volatile;",
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
						"ttl":  "5m",
					},
				},
			}, system...)
			content[0].(map[string]interface{})["cache_control"] = map[string]interface{}{
				"type": "ephemeral",
				"ttl":  "1h",
			}
		}
		return &ClaudeRequest{
			Model:  "claude-sonnet-4-6",
			System: system,
			Tools: []ClaudeTool{{
				Name:        "read_file",
				Description: "读取文件",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string"},
					},
					"cache_control": map[string]interface{}{"type": "ephemeral"},
				},
			}},
			Messages: []ClaudeMessage{{Role: "user", Content: content}},
		}
	}

	plain := analyzeClaudeRequest(build(false), "api-key-1")
	withMetadata := analyzeClaudeRequest(build(true), "api-key-1")

	if plain.TaskKey != withMetadata.TaskKey {
		t.Fatalf("cache_control 和计费头不得改变任务键")
	}
	if plain.RequestFingerprint != withMetadata.RequestFingerprint {
		t.Fatalf("cache_control 和计费头不得改变请求指纹")
	}
	if len(plain.Prefixes) != len(withMetadata.Prefixes) ||
		plain.Prefixes[0].Fingerprint != withMetadata.Prefixes[0].Fingerprint {
		t.Fatalf("cache_control 和计费头不得改变消息前缀")
	}
}

func TestAnalyzeClaudeRequestProducesMessageEndPrefixes(t *testing.T) {
	req := analysisTestRequest()
	req.Messages = append(req.Messages,
		ClaudeMessage{Role: "assistant", Content: "文件内容"},
		ClaudeMessage{Role: "user", Content: "总结修改点"},
	)

	analysis := analyzeClaudeRequest(req, "api-key-1")

	if len(analysis.Prefixes) != len(req.Messages) {
		t.Fatalf("前缀数量应等于消息数量，得到 %d", len(analysis.Prefixes))
	}
	for i := range analysis.Prefixes {
		if analysis.Prefixes[i].CumulativeTokens <= 0 {
			t.Fatalf("第 %d 个前缀缺少累计 token", i)
		}
		if i > 0 && analysis.Prefixes[i].CumulativeTokens <= analysis.Prefixes[i-1].CumulativeTokens {
			t.Fatalf("消息前缀累计 token 应严格增长")
		}
		if i > 0 && analysis.Prefixes[i].Fingerprint == analysis.Prefixes[i-1].Fingerprint {
			t.Fatalf("相邻消息前缀指纹不得相同")
		}
	}
}

func TestAnalyzeClaudeRequestCountsToolContent(t *testing.T) {
	req := analysisTestRequest()
	req.Messages = []ClaudeMessage{
		{Role: "user", Content: "读取配置"},
		{Role: "assistant", Content: []interface{}{
			map[string]interface{}{
				"type":  "tool_use",
				"id":    "tool-1",
				"name":  "read_file",
				"input": map[string]interface{}{"path": "config.json"},
			},
		}},
		{Role: "user", Content: []interface{}{
			map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": "tool-1",
				"content":     "配置内容",
			},
		}},
	}

	analysis := analyzeClaudeRequest(req, "api-key-1")

	if analysis.ToolTokens <= 0 {
		t.Fatalf("工具调用和工具结果应计入 ToolTokens")
	}
	if analysis.ToolTokens >= analysis.CacheableTokens {
		t.Fatalf("工具 token 应只是可缓存上下文的一部分")
	}
}

func TestAnalyzeClaudeRequestMatchesLegacyTokenEstimate(t *testing.T) {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "系统提示"},
			map[string]interface{}{"type": "thinking", "thinking": "内部推理"},
		},
		Tools: []ClaudeTool{{
			Name:        "read_file",
			Description: "读取文件",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
			},
		}},
		Messages: []ClaudeMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "读取配置"},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type":  "tool_use",
					"name":  "read_file",
					"input": map[string]interface{}{"path": "config.json"},
				},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":    "tool_result",
					"content": "配置内容",
				},
			}},
		},
	}

	legacy := estimateClaudeRequestInputTokensLegacy(req)
	analysis := analyzeClaudeRequest(req, "api-key-1")

	if analysis.EstimatedInputTokens != legacy {
		t.Fatalf("单遍估算应与旧实现一致：新=%d 旧=%d", analysis.EstimatedInputTokens, legacy)
	}
}

func analysisTestRequest() *ClaudeRequest {
	return &ClaudeRequest{
		Model:  "claude-sonnet-4-6",
		System: "You are a coding agent.",
		Tools: []ClaudeTool{{
			Name:        "read_file",
			Description: "读取文件",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string"},
				},
			},
		}},
		Messages: []ClaudeMessage{{Role: "user", Content: "读取 config.json"}},
	}
}

func cloneClaudeRequestForAnalysisTest(req *ClaudeRequest) *ClaudeRequest {
	cloned := *req
	cloned.Tools = append([]ClaudeTool(nil), req.Tools...)
	cloned.Messages = append([]ClaudeMessage(nil), req.Messages...)
	return &cloned
}
