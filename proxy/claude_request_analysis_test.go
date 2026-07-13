package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
)

type analysisJSONMarshaler string

func (value analysisJSONMarshaler) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"marshaled": string(value)})
}

type analysisJSONEmbedded struct {
	Embedded string `json:"embedded"`
}

type analysisJSONCompatibilityPayload struct {
	analysisJSONEmbedded
	Number int `json:"number,string"`
}

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

func TestAnalyzeClaudeRequestCountsTypedToolContent(t *testing.T) {
	req := analysisTestRequest()
	req.Messages = []ClaudeMessage{
		{Role: "user", Content: "读取配置"},
		{Role: "assistant", Content: []ClaudeContentBlock{
			{
				Type:  "tool_use",
				ID:    "tool-1",
				Name:  "read_file",
				Input: map[string]interface{}{"path": "config.json"},
			},
		}},
		{Role: "user", Content: []ClaudeContentBlock{
			{
				Type:      "tool_result",
				ToolUseID: "tool-1",
				Content:   "配置内容",
			},
		}},
	}

	analysis := analyzeClaudeRequest(req, "api-key-1")

	if analysis.ToolTokens <= 0 {
		t.Fatalf("类型化工具调用和工具结果应计入 ToolTokens")
	}
	if analysis.ToolTokens >= analysis.CacheableTokens {
		t.Fatalf("类型化工具 token 应只是可缓存上下文的一部分")
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

func TestAnalyzeClaudeRequestKeepsNestedBillingHeaderShapedData(t *testing.T) {
	build := func(payload interface{}) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			Tools: []ClaudeTool{{
				Name:        "submit_payload",
				Description: "提交结构化数据",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"payload": payload,
					},
				},
			}},
			Messages: []ClaudeMessage{
				{Role: "user", Content: "提交数据"},
				{Role: "assistant", Content: []interface{}{
					map[string]interface{}{
						"type": "tool_use",
						"name": "submit_payload",
						"input": map[string]interface{}{
							"payload": payload,
						},
					},
				}},
			},
		}
	}
	billingShaped := map[string]interface{}{
		"type": "text",
		"text": "x-anthropic-billing-header: 这是工具业务数据",
	}

	withData := analyzeClaudeRequest(build([]interface{}{billingShaped}), "api-key-1")
	withoutData := analyzeClaudeRequest(build([]interface{}{}), "api-key-1")

	if withData.TaskKey == withoutData.TaskKey {
		t.Fatalf("工具 schema 内嵌的计费头形状数据必须参与任务键")
	}
	if withData.RequestFingerprint == withoutData.RequestFingerprint {
		t.Fatalf("工具输入内嵌的计费头形状数据必须参与请求指纹")
	}
}

func TestAnalyzeClaudeRequestUsesFirstMeaningfulUserMessage(t *testing.T) {
	build := func(firstRealMessage string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{
				{Role: "user", Content: " \n\t "},
				{Role: "user", Content: []interface{}{
					map[string]interface{}{
						"type": "text",
						"text": "",
						"cache_control": map[string]interface{}{
							"type": "ephemeral",
						},
					},
				}},
				{Role: "user", Content: []ClaudeContentBlock{
					{Type: "text", Text: ""},
				}},
				{Role: "user", Content: firstRealMessage},
			},
		}
	}

	first := analyzeClaudeRequest(build("读取配置"), "api-key-1")
	second := analyzeClaudeRequest(build("运行测试"), "api-key-1")

	if first.TaskKey == second.TaskKey {
		t.Fatalf("空白和空文本块不得抢占首条有效用户消息")
	}
}

func TestAnalyzeClaudeRequestFiltersTypedBillingHeaderBlock(t *testing.T) {
	build := func(withBillingHeader bool, firstRealMessage string) *ClaudeRequest {
		blocks := []ClaudeContentBlock{{Type: "text", Text: firstRealMessage}}
		if withBillingHeader {
			blocks = append([]ClaudeContentBlock{{
				Type: "text",
				Text: "x-anthropic-billing-header: cc_version=2.1; cch=volatile;",
			}}, blocks...)
		}
		return &ClaudeRequest{
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{
				{Role: "user", Content: blocks},
			},
		}
	}

	plain := analyzeClaudeRequest(build(false, "读取配置"), "api-key-1")
	withHeader := analyzeClaudeRequest(build(true, "读取配置"), "api-key-1")
	changedUser := analyzeClaudeRequest(build(true, "运行测试"), "api-key-1")

	if plain.TaskKey != withHeader.TaskKey {
		t.Fatalf("类型化计费头不得改变任务键")
	}
	if plain.RequestFingerprint != withHeader.RequestFingerprint {
		t.Fatalf("类型化计费头不得改变请求指纹")
	}
	if plain.Prefixes[0].CumulativeTokens != withHeader.Prefixes[0].CumulativeTokens {
		t.Fatalf("类型化计费头不得改变可缓存前缀 token")
	}
	if withHeader.TaskKey == changedUser.TaskKey {
		t.Fatalf("类型化计费头不得抢占首条有效用户消息")
	}
}

func TestAnalyzeClaudeRequestFingerprintIncludesBehaviorOptionsButNotStream(t *testing.T) {
	base := analysisTestRequest()
	base.MaxTokens = 4096
	base.Temperature = 0.2
	base.TopP = 0.9
	base.Thinking = &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048}
	base.ToolChoice = map[string]interface{}{"type": "auto"}

	changed := cloneClaudeRequestForAnalysisTest(base)
	changed.MaxTokens = 8192

	first := analyzeClaudeRequest(base, "api-key-1")
	second := analyzeClaudeRequest(changed, "api-key-1")
	if first.TaskKey != second.TaskKey {
		t.Fatalf("输出行为参数不应改变任务键")
	}
	if first.RequestFingerprint == second.RequestFingerprint {
		t.Fatalf("输出行为参数必须改变完整请求指纹")
	}
	if len(first.Prefixes) != len(second.Prefixes) ||
		first.Prefixes[len(first.Prefixes)-1].Fingerprint !=
			second.Prefixes[len(second.Prefixes)-1].Fingerprint {
		t.Fatalf("输出行为参数不应改变上下文前缀")
	}

	streamed := cloneClaudeRequestForAnalysisTest(base)
	streamed.Stream = true
	streamedAnalysis := analyzeClaudeRequest(streamed, "api-key-1")
	if streamedAnalysis.RequestFingerprint != first.RequestFingerprint {
		t.Fatalf("同步与流式应复用同一请求内容指纹")
	}
}

func TestAnalyzeClaudeRequestMatchesLegacyTokenEstimateForComplexJSON(t *testing.T) {
	tests := map[string]*ClaudeRequest{
		"带缓存元数据和 HTML 的 schema": {
			Model: "claude-sonnet-4-6",
			Tools: []ClaudeTool{{
				Name:        "render_html",
				Description: "渲染 <main>&content</main>",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"html": map[string]interface{}{
							"type":        "string",
							"description": "<div>&</div>",
						},
					},
					"cache_control": map[string]interface{}{"type": "ephemeral"},
				},
			}},
			Messages: []ClaudeMessage{{Role: "user", Content: "渲染页面"}},
		},
		"图片内容块": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type":       "base64",
						"media_type": "image/png",
						"data":       "iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB",
					},
				},
			}}},
		},
		"类型化内容": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: []ClaudeContentBlock{
				{Type: "text", Text: "类型化文本 <>&"},
				{
					Type: "image",
					Source: &ImageSource{
						Type:      "base64",
						MediaType: "image/png",
						Data:      "AAAA",
					},
				},
			}}},
		},
		"缺少文本字段的文本块": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":     "text",
					"metadata": "保留整块 JSON 计数",
				},
			}}},
		},
		"非字符串文本字段": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": 123,
				},
			}}},
		},
		"空工具调用回退": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "assistant", Content: []interface{}{
				map[string]interface{}{
					"type": "tool_use",
					"id":   "tool-1",
				},
			}}},
		},
		"未知类型元数据回退": {
			Model: "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
				map[string]interface{}{
					"type":     "custom",
					"text":     "",
					"metadata": "保留整块 JSON 计数",
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			}}},
		},
	}

	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			legacy := estimateClaudeRequestInputTokensLegacy(req)
			analysis := analyzeClaudeRequest(req, "api-key-1")
			if analysis.EstimatedInputTokens != legacy {
				t.Fatalf("复杂 JSON token 估算不一致：新=%d 旧=%d", analysis.EstimatedInputTokens, legacy)
			}
		})
	}
}

func TestAnalysisApproxTokenWriterHandlesSplitUTF8(t *testing.T) {
	payload := []byte("　中文")

	whole := &analysisApproxTokenWriter{}
	_, _ = whole.Write(payload)

	split := &analysisApproxTokenWriter{}
	for _, value := range payload {
		_, _ = split.Write([]byte{value})
	}

	if split.tokens() != whole.tokens() {
		t.Fatalf(
			"UTF-8 分块不得改变 token 估算：完整=%d 分块=%d",
			whole.tokens(),
			split.tokens(),
		)
	}
	if split.meaningful != whole.meaningful {
		t.Fatalf(
			"UTF-8 分块不得改变有效内容判断：完整=%t 分块=%t",
			whole.meaningful,
			split.meaningful,
		)
	}
}

func TestCanonicalAnalysisJSONMatchesEncodingJSONOmitEmpty(t *testing.T) {
	type nested struct {
		Value string `json:"value,omitempty"`
	}
	type payload struct {
		Empty nested `json:"empty,omitempty"`
	}

	assertCanonicalAnalysisJSONMatchesEncodingJSON(t, payload{})
}

func TestCanonicalAnalysisJSONMatchesEncodingJSONSpecialTypes(t *testing.T) {
	tests := map[string]interface{}{
		"整数键 map": map[int]string{2: "b", 1: "a"},
		"字节切片":    []byte{0, 1, 2, 250, 255},
		"自定义编码":   analysisJSONMarshaler("value"),
		"匿名字段": analysisJSONCompatibilityPayload{
			analysisJSONEmbedded: analysisJSONEmbedded{Embedded: "x"},
			Number:               42,
		},
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			assertCanonicalAnalysisJSONMatchesEncodingJSON(t, value)
		})
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

func assertCanonicalAnalysisJSONMatchesEncodingJSON(t *testing.T, value interface{}) {
	t.Helper()

	want, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("编码参考 JSON 失败：%v", err)
	}
	var got bytes.Buffer
	writeCanonicalAnalysisJSON(&got, value)
	if got.String() != string(want) {
		t.Fatalf("规范 JSON 与 encoding/json 不一致：得到=%s 期望=%s", got.String(), want)
	}
}

func cloneClaudeRequestForAnalysisTest(req *ClaudeRequest) *ClaudeRequest {
	cloned := *req
	cloned.Tools = append([]ClaudeTool(nil), req.Tools...)
	cloned.Messages = append([]ClaudeMessage(nil), req.Messages...)
	return &cloned
}
