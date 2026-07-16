package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const claudeContractCallerID = "contract-caller"

func TestClaudeNonStreamReturnsCompleteCacheUsageWithoutCacheControl(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := newClaudeUsageUpstream(t, "合同响应", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	recorder := performClaudeContractRequest(t, handler, false, "第一轮请求")
	if recorder.Code != http.StatusOK {
		t.Fatalf("同步请求失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	usage, rawUsage := decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
	assertCompleteClaudeUsageFields(t, rawUsage)
	assertUsageHitRate(t, usage)
	if usage.CacheReadInputTokens <= 0 {
		t.Fatalf("完整请求必须读取缓存：%+v", usage)
	}
	if usage.CacheCreationInputTokens !=
		usage.CacheCreation.Ephemeral5mInputTokens+
			usage.CacheCreation.Ephemeral1hInputTokens {
		t.Fatalf("缓存创建汇总与 TTL 明细不一致：%+v", usage)
	}
}

func TestClaudeStreamFinalDeltaMatchesNonStreamUsage(t *testing.T) {
	nonStreamHandler := setupClaudeContractHandler(t, 1)
	streamHandler := &Handler{
		pool:        nonStreamHandler.pool,
		promptCache: newPromptCacheTracker(time.Hour),
	}
	upstream := newClaudeUsageUpstream(t, "一致响应", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	nonStream := performClaudeContractRequest(t, nonStreamHandler, false, "同一请求")
	want, _ := decodeClaudeNonStreamUsage(t, nonStream.Body.Bytes())

	stream := performClaudeContractRequest(t, streamHandler, true, "同一请求")
	if stream.Code != http.StatusOK {
		t.Fatalf("流式请求失败：status=%d body=%s", stream.Code, stream.Body.String())
	}
	got, rawFinal := decodeClaudeFinalSSEUsage(t, stream.Body.String())
	assertCompleteClaudeUsageFields(t, rawFinal)
	if got != want {
		t.Fatalf("同步与流式最终 usage 不一致：sync=%+v stream=%+v", want, got)
	}

	startUsage, rawStart := decodeClaudeMessageStartUsage(t, stream.Body.String())
	assertCompleteClaudeUsageFields(t, rawStart)
	if startUsage.CacheReadInputTokens != 0 ||
		startUsage.CacheCreationInputTokens != 0 ||
		startUsage.CacheCreation.Ephemeral5mInputTokens != 0 ||
		startUsage.CacheCreation.Ephemeral1hInputTokens != 0 {
		t.Fatalf("message_start 的缓存字段必须为零：%+v", startUsage)
	}
}

func TestClaudeUsageRetryAcrossAccountsIsStable(t *testing.T) {
	handler := setupClaudeContractHandler(t, 2)
	var attempts atomic.Int32
	var authorizationMu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationMu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		authorizationMu.Unlock()
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		writeClaudeUsageFrame(t, w, "重试成功", 10_000)
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	recorder := performClaudeContractRequest(t, handler, false, "需要重试")
	if recorder.Code != http.StatusOK {
		t.Fatalf("账号重试后仍失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	usage, _ := decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
	if usage.CacheReadInputTokens <= 0 {
		t.Fatalf("重试成功后应生成缓存读取 usage：%+v", usage)
	}
	assertUsageHitRate(t, usage)
	if attempts.Load() != 2 {
		t.Fatalf("应跨两个账号重试，实际请求次数=%d", attempts.Load())
	}
	authorizationMu.Lock()
	defer authorizationMu.Unlock()
	if len(authorizations) != 2 || authorizations[0] == authorizations[1] {
		t.Fatalf("应使用两个不同账号完成重试")
	}

	snapshot := claudeContractSnapshot(t, handler, false, "需要重试")
	if snapshot.ExistingUsage == nil || *snapshot.ExistingUsage != usage {
		t.Fatalf("重试后的幂等 usage 未稳定保存：snapshot=%+v response=%+v", snapshot.ExistingUsage, usage)
	}
}

func TestClaudeFailedUpstreamDoesNotCommitCacheState(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream failed", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	recorder := performClaudeContractRequest(t, handler, false, "失败请求")
	if recorder.Code == http.StatusOK {
		t.Fatalf("上游失败时不得返回成功：%s", recorder.Body.String())
	}
	if handler.promptCache.taskCount() != 0 {
		t.Fatalf("上游失败不得提交高缓存状态")
	}
}

func TestClaudeMissingFinalUsageDoesNotCommitCacheState(t *testing.T) {
	tests := []struct {
		name        string
		stream      bool
		contentOnly bool
	}{
		{name: "同步空响应", stream: false},
		{name: "同步仅正文", stream: false, contentOnly: true},
		{name: "流式空响应", stream: true},
		{name: "流式仅正文", stream: true, contentOnly: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := setupClaudeContractHandler(t, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				if tc.contentOnly {
					_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
						"content": "只有正文，没有终态 token usage",
					}))
				}
			}))
			defer upstream.Close()
			defer swapKiroEndpointsForTest(t, upstream)()

			recorder := performClaudeContractRequest(
				t,
				handler,
				tc.stream,
				strings.Repeat("缺少终态用量的长请求", 4096),
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("缺少终态 usage 的响应仍应安全降级：status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			var usage ClaudeUsage
			if tc.stream {
				usage, _ = decodeClaudeFinalSSEUsage(t, recorder.Body.String())
			} else {
				usage, _ = decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
			}
			if usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 {
				t.Fatalf("缺少终态 usage 时不得生成高缓存用量：%+v", usage)
			}
			if handler.promptCache.taskCount() != 0 {
				t.Fatalf("缺少终态 usage 时不得提交高缓存状态")
			}
		})
	}
}

func TestClaudeContextUsageCommitsCacheStateWithoutExplicitTokenUsage(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "同步"},
		{name: "流式", stream: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := setupClaudeContractHandler(t, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
					"content": "正文只带最终上下文占比",
				}))
				_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
					"contextUsagePercentage": 1.0,
				}))
			}))
			defer upstream.Close()
			defer swapKiroEndpointsForTest(t, upstream)()

			recorder := performClaudeContractRequest(
				t,
				handler,
				tc.stream,
				strings.Repeat("生产上游上下文用量", 4096),
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("带最终上下文占比的请求失败：status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			var usage ClaudeUsage
			if tc.stream {
				usage, _ = decodeClaudeFinalSSEUsage(t, recorder.Body.String())
			} else {
				usage, _ = decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
			}
			if usage.CacheReadInputTokens <= 0 {
				t.Fatalf("最终上下文占比应足以生成缓存读取用量：%+v", usage)
			}
			assertUsageHitRate(t, usage)
			if !claudeUsageCostConserved(10_000, usage) {
				t.Fatalf("1%% 上下文占比应按 10,000 个原始输入 token 守恒：%+v", usage)
			}
			if handler.promptCache.taskCount() != 1 {
				t.Fatalf("最终上下文占比应提交高缓存状态")
			}
		})
	}
}

func TestClaudeFinalContextUsageOverridesEarlierPositiveValue(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		finalUsage float64
	}{
		{name: "同步最终零值", finalUsage: 0},
		{name: "流式最终零值", stream: true, finalUsage: 0},
		{name: "同步最终超范围值", finalUsage: 101},
		{name: "流式最终超范围值", stream: true, finalUsage: 101},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := setupClaudeContractHandler(t, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
					"contextUsagePercentage": 1.0,
				}))
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
					"content": "最终上下文占比覆盖先前正值",
				}))
				_, _ = w.Write(awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{
					"contextUsagePercentage": tc.finalUsage,
				}))
			}))
			defer upstream.Close()
			defer swapKiroEndpointsForTest(t, upstream)()

			recorder := performClaudeContractRequest(
				t,
				handler,
				tc.stream,
				strings.Repeat("最终零值上下文用量", 4096),
			)
			if recorder.Code != http.StatusOK {
				t.Fatalf("多次上下文占比请求失败：status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			var usage ClaudeUsage
			if tc.stream {
				usage, _ = decodeClaudeFinalSSEUsage(t, recorder.Body.String())
			} else {
				usage, _ = decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
			}
			if usage.CacheReadInputTokens != 0 || usage.CacheCreationInputTokens != 0 {
				t.Fatalf("最终无效上下文占比不得沿用先前正值生成缓存：%+v", usage)
			}
			if handler.promptCache.taskCount() != 0 {
				t.Fatalf("最终无效上下文占比不得提交高缓存状态")
			}
		})
	}
}

func TestClaudeRetryFingerprintDoesNotAdvanceRound(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := newClaudeUsageUpstream(t, "幂等响应", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	first := performClaudeContractRequest(t, handler, false, "完全相同的请求")
	second := performClaudeContractRequest(t, handler, false, "完全相同的请求")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("幂等请求失败：first=%d second=%d", first.Code, second.Code)
	}

	firstUsage, _ := decodeClaudeNonStreamUsage(t, first.Body.Bytes())
	secondUsage, _ := decodeClaudeNonStreamUsage(t, second.Body.Bytes())
	if firstUsage != secondUsage {
		t.Fatalf("同一请求指纹必须复用 usage：first=%+v second=%+v", firstUsage, secondUsage)
	}

	snapshot := claudeContractSnapshot(t, handler, false, "完全相同的请求")
	if snapshot.SuccessfulRounds != 1 {
		t.Fatalf("幂等重试不得推进轮次：得到 %d", snapshot.SuccessfulRounds)
	}
}

func TestClaudeMessageStartDoesNotCommitCacheState(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	releaseUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeClaudeUsageFrame(t, w, strings.Repeat("先发送正文", 20), 10_000)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releaseUpstream
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	writer := newClaudeSignalWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := newClaudeContractHTTPRequest(t, true, "等待最终 usage")
		handler.handleClaudeMessages(writer, request)
	}()

	select {
	case <-writer.messageStart:
	case <-time.After(2 * time.Second):
		close(releaseUpstream)
		t.Fatalf("未收到 message_start")
	}
	if handler.promptCache.taskCount() != 0 {
		close(releaseUpstream)
		t.Fatalf("message_start 阶段不得提交高缓存状态")
	}

	close(releaseUpstream)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("流式请求未在上游结束后退出")
	}
	if handler.promptCache.taskCount() != 1 {
		t.Fatalf("最终上游 usage 完整后应提交状态")
	}
}

func TestClaudeClientDisconnectCommitsOnlyAfterFinalUpstreamUsage(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstreamFinished := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamFinished)
		writeClaudeUsageFrame(t, w, "客户端已断开", 10_000)
	}))
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	writer := newClaudeDisconnectWriter()
	request := newClaudeContractHTTPRequest(t, true, "断开后排空")
	handler.handleClaudeMessages(writer, request)

	select {
	case <-upstreamFinished:
	default:
		t.Fatalf("客户端断开后仍应排空上游")
	}
	if handler.promptCache.taskCount() != 1 {
		t.Fatalf("上游已排空且最终 usage 完整时应提交状态")
	}
}

func TestClaudeTruncatedPayloadFallsBackWithoutCommit(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := newClaudeUsageUpstream(t, "截断响应", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	oversized := strings.Repeat("超大上下文", maxPayloadBytes/len("超大上下文")+4096)
	recorder := performClaudeContractRequest(t, handler, false, oversized)
	if recorder.Code != http.StatusOK {
		t.Fatalf("截断请求失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	usage, rawUsage := decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
	assertCompleteClaudeUsageFields(t, rawUsage)
	if usage.InputTokens != 10_000 ||
		usage.CacheReadInputTokens != 0 ||
		usage.CacheCreationInputTokens != 0 {
		t.Fatalf("截断请求必须回退原始 usage：%+v", usage)
	}
	if handler.promptCache.taskCount() != 0 {
		t.Fatalf("截断请求不得提交未实际发送的前缀")
	}
}

func TestClaudeNonStreamEncodeFailureDoesNotCommitCacheState(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := newClaudeUsageUpstream(t, "编码失败", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	request := newClaudeContractHTTPRequest(t, false, "响应写入失败")
	handler.handleClaudeMessages(&claudeFailingResponseWriter{}, request)
	if handler.promptCache.taskCount() != 0 {
		t.Fatalf("同步响应编码失败时不得提交状态")
	}
}

func TestPrepareFinalClaudeUsageReusesExistingUsageWithRefreshCommit(t *testing.T) {
	request := &ClaudeRequest{
		Model:    "claude-sonnet-4.6",
		Messages: []ClaudeMessage{{Role: "user", Content: "幂等请求"}},
	}
	analysis := analyzeClaudeRequest(request, claudeContractCallerID)
	existing := testClaudeUsage(100, 10)
	now := time.Unix(1_700_000_000, 0)
	prepared := prepareFinalClaudeUsage(
		10_000,
		20,
		analysis,
		promptCacheSnapshot{
			TaskKey:            analysis.TaskKey,
			RequestFingerprint: analysis.RequestFingerprint,
			TTL:                promptCacheTTL5m,
			ExistingUsage:      &existing,
		},
		now,
	)

	if !prepared.OK || prepared.Usage != existing {
		t.Fatalf("应直接复用已有 usage：%+v", prepared)
	}
	if prepared.Commit.SuccessfulAt != now ||
		prepared.Commit.TaskKey != analysis.TaskKey ||
		prepared.Commit.RequestFingerprint != analysis.RequestFingerprint {
		t.Fatalf("幂等成功重试应生成仅用于刷新活动窗口的提交：%+v", prepared.Commit)
	}
}

func TestKiroDebugLogDoesNotContainPromptOrAPIKey(t *testing.T) {
	_ = setupClaudeContractHandler(t, 1)

	var logs bytes.Buffer
	oldLevel := logger.GetLevel()
	logger.SetLevel(logger.LevelDebug)
	logger.SetOutput(&logs)
	t.Cleanup(func() {
		logger.SetLevel(oldLevel)
		logger.SetOutput(os.Stdout)
	})

	upstream := newClaudeUsageUpstream(t, "日志响应", 10_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	const secretPrompt = "绝不能出现在日志中的提示词"
	const secretToken = "仅用于测试的伪令牌"
	const privateEmail = "private@example.invalid"
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: secretPrompt,
		ModelID: "claude-sonnet-4.6",
		Origin:  "AI_EDITOR",
	}
	account := &config.Account{
		ID:          "log-account",
		Email:       privateEmail,
		AccessToken: secretToken,
		ProfileArn:  "arn:aws:codewhisperer:profile/log-test",
	}

	if err := CallKiroAPI(account, payload, &KiroStreamCallback{}); err != nil {
		t.Fatalf("调用假上游失败：%v", err)
	}
	logText := logs.String()
	for _, secret := range []string{secretPrompt, secretToken, privateEmail} {
		if strings.Contains(logText, secret) {
			t.Fatalf("debug 日志泄露敏感内容")
		}
	}
	if !strings.Contains(logText, "payload_bytes=") ||
		!strings.Contains(logText, "payload_sha256=") {
		t.Fatalf("debug 日志应只记录 payload 大小和摘要：%s", logText)
	}
	if got := accountEmailForLog(account); got == privateEmail {
		t.Fatalf("日志邮箱必须脱敏")
	}
}

func setupClaudeContractHandler(t *testing.T, accountCount int) *Handler {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("初始化配置失败：%v", err)
	}
	for index := 0; index < accountCount; index++ {
		account := config.Account{
			ID:          "contract-account-" + string(rune('a'+index)),
			Enabled:     true,
			AccessToken: "contract-token-" + string(rune('a'+index)),
			ProfileArn:  "arn:aws:codewhisperer:profile/contract-" + string(rune('a'+index)),
		}
		if err := config.AddAccount(account); err != nil {
			t.Fatalf("添加测试账号失败：%v", err)
		}
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("设置测试端点失败：%v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("关闭端点回退失败：%v", err)
	}
	if _, err := config.AddApiKey(config.ApiKeyEntry{
		ID:      claudeContractCallerID,
		Name:    "合同测试调用方",
		Key:     "contract-key-placeholder",
		Enabled: true,
	}); err != nil {
		t.Fatalf("添加测试 API Key 失败：%v", err)
	}

	accountPool := accountpool.GetPool()
	accountPool.Reload()
	t.Cleanup(accountPool.WaitForPendingStats)
	return &Handler{
		pool:        accountPool,
		promptCache: newPromptCacheTracker(time.Hour),
	}
}

func newClaudeUsageUpstream(t *testing.T, content string, inputTokens int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeClaudeUsageFrame(t, w, content, inputTokens)
	}))
}

func writeClaudeUsageFrame(t *testing.T, w http.ResponseWriter, content string, inputTokens int) {
	t.Helper()
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content":      content,
		"inputTokens":  inputTokens,
		"outputTokens": 32,
	}))
}

func performClaudeContractRequest(
	t *testing.T,
	handler *Handler,
	stream bool,
	content string,
) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.handleClaudeMessages(recorder, newClaudeContractHTTPRequest(t, stream, content))
	return recorder
}

func newClaudeContractHTTPRequest(t *testing.T, stream bool, content string) *http.Request {
	t.Helper()
	body, err := json.Marshal(ClaudeRequest{
		Model:     "claude-sonnet-4.6",
		MaxTokens: 256,
		Stream:    stream,
		System:    "你是合同测试助手",
		Messages: []ClaudeMessage{
			{Role: "user", Content: content},
		},
	})
	if err != nil {
		t.Fatalf("编码 Claude 请求失败：%v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	return withApiKeyContext(request, &config.ApiKeyEntry{ID: claudeContractCallerID})
}

func claudeContractSnapshot(
	t *testing.T,
	handler *Handler,
	stream bool,
	content string,
) promptCacheSnapshot {
	t.Helper()
	var request ClaudeRequest
	httpRequest := newClaudeContractHTTPRequest(t, stream, content)
	body, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("读取合同请求失败：%v", err)
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("解析合同请求失败：%v", err)
	}
	analysis := analyzeClaudeRequest(&request, claudeContractCallerID)
	return handler.promptCache.Snapshot(analysis, time.Now())
}

func decodeClaudeNonStreamUsage(t *testing.T, body []byte) (ClaudeUsage, map[string]interface{}) {
	t.Helper()
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("解析同步响应失败：%v body=%s", err, body)
	}
	rawUsage, ok := raw["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("同步响应缺少 usage：%s", body)
	}
	return decodeClaudeUsageMap(t, rawUsage), rawUsage
}

func decodeClaudeFinalSSEUsage(t *testing.T, body string) (ClaudeUsage, map[string]interface{}) {
	t.Helper()
	for _, event := range decodeClaudeSSEEvents(t, body) {
		if event["type"] != "message_delta" {
			continue
		}
		rawUsage, ok := event["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("最终 message_delta 缺少 usage：%s", body)
		}
		return decodeClaudeUsageMap(t, rawUsage), rawUsage
	}
	t.Fatalf("流式响应缺少最终 message_delta：%s", body)
	return ClaudeUsage{}, nil
}

func decodeClaudeMessageStartUsage(t *testing.T, body string) (ClaudeUsage, map[string]interface{}) {
	t.Helper()
	for _, event := range decodeClaudeSSEEvents(t, body) {
		if event["type"] != "message_start" {
			continue
		}
		message, ok := event["message"].(map[string]interface{})
		if !ok {
			t.Fatalf("message_start 缺少 message：%s", body)
		}
		rawUsage, ok := message["usage"].(map[string]interface{})
		if !ok {
			t.Fatalf("message_start 缺少 usage：%s", body)
		}
		return decodeClaudeUsageMap(t, rawUsage), rawUsage
	}
	t.Fatalf("流式响应缺少 message_start：%s", body)
	return ClaudeUsage{}, nil
}

func decodeClaudeSSEEvents(t *testing.T, body string) []map[string]interface{} {
	t.Helper()
	var events []map[string]interface{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("解析 SSE 事件失败：%v line=%s", err, line)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("扫描 SSE 响应失败：%v", err)
	}
	return events
}

func decodeClaudeUsageMap(t *testing.T, raw map[string]interface{}) ClaudeUsage {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("编码 usage 失败：%v", err)
	}
	var usage ClaudeUsage
	if err := json.Unmarshal(encoded, &usage); err != nil {
		t.Fatalf("解析 usage 失败：%v raw=%s", err, encoded)
	}
	return usage
}

func assertCompleteClaudeUsageFields(t *testing.T, raw map[string]interface{}) {
	t.Helper()
	for _, field := range []string{
		"input_tokens",
		"output_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"cache_creation",
	} {
		if _, ok := raw[field]; !ok {
			t.Fatalf("usage 缺少字段 %q：%v", field, raw)
		}
	}
	cacheCreation, ok := raw["cache_creation"].(map[string]interface{})
	if !ok {
		t.Fatalf("cache_creation 必须为对象：%v", raw)
	}
	for _, field := range []string{
		"ephemeral_5m_input_tokens",
		"ephemeral_1h_input_tokens",
	} {
		if _, ok := cacheCreation[field]; !ok {
			t.Fatalf("cache_creation 缺少字段 %q：%v", field, cacheCreation)
		}
	}
}

type claudeSignalWriter struct {
	header       http.Header
	messageStart chan struct{}
	once         sync.Once
	mu           sync.Mutex
	body         bytes.Buffer
}

func newClaudeSignalWriter() *claudeSignalWriter {
	return &claudeSignalWriter{
		header:       make(http.Header),
		messageStart: make(chan struct{}),
	}
}

func (w *claudeSignalWriter) Header() http.Header {
	return w.header
}

func (w *claudeSignalWriter) WriteHeader(int) {}

func (w *claudeSignalWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.body.Write(data)
	w.mu.Unlock()
	if bytes.Contains(data, []byte("event: message_start")) {
		w.once.Do(func() { close(w.messageStart) })
	}
	return len(data), nil
}

func (w *claudeSignalWriter) Flush() {}

type claudeDisconnectWriter struct {
	header http.Header
}

func newClaudeDisconnectWriter() *claudeDisconnectWriter {
	return &claudeDisconnectWriter{header: make(http.Header)}
}

func (w *claudeDisconnectWriter) Header() http.Header {
	return w.header
}

func (w *claudeDisconnectWriter) WriteHeader(int) {}

func (w *claudeDisconnectWriter) Write([]byte) (int, error) {
	return 0, errors.New("客户端已断开")
}

func (w *claudeDisconnectWriter) Flush() {}

type claudeFailingResponseWriter struct{}

func (w *claudeFailingResponseWriter) Header() http.Header {
	return make(http.Header)
}

func (w *claudeFailingResponseWriter) WriteHeader(int) {}

func (w *claudeFailingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("响应写入失败")
}
