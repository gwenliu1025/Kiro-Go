# Kiro-Go 原生高缓存 usage 与 Sub2API 清退实施计划

> **面向代理工作者：** 必须使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans` 按任务逐项实施。所有步骤使用复选框跟踪，所有行为变更遵循 TDD。

**目标：** 让 Kiro-Go 成为 Anthropic 高缓存 `usage` 的唯一生成方，并从 Sub2API 定向清退 Equivalent Cache V1/V2，同时保留原生 usage 解析、分模型定价、分组倍率、历史数据库迁移兼容和既有 Fork 能力。

**架构：** Kiro-Go 在请求进入上游前执行一次单遍分析，生成调用方隔离的任务键、完整请求指纹、精确前缀断点和轻量特征；上游成功后，同步与流式共用同一个纯整数求解器生成最终 `ClaudeUsage`，再以两阶段提交更新内存生命周期。Sub2API 只消费标准 Anthropic usage，删除 V1/V2 的状态、改写、资格、锁价和审计运行时路径，不删除已发布迁移，也不影响原生价格与倍率链路。

**技术栈：** Go、标准库 `crypto/sha256`/`encoding/json`/`math`/`sync`、Anthropic Messages JSON/SSE、Docker Compose、Ent、PostgreSQL、Redis（仅保留 Sub2API 原生用途）、Vue/pnpm。

**实施仓库：**

- Kiro-Go：`Kiro-Go-sync`，实施分支 `feat/kiro-native-high-cache`
- Sub2API：`sub2api-configurable-update-repo`，实施分支 `feat/remove-equivalent-cache`
- 生产变更不属于自动实施范围；必须在毕业机验收后再次取得用户明确授权。

---

### 任务 1：修复 Kiro-Go 基线测试契约并记录基线

**文件：**

- 修改：`proxy/translator_test.go`
- 新建：`docs/superpowers/results/2026-07-14-kiro-go-baseline.md`

- [ ] **步骤 1：确认两个既有失败稳定复现**

运行：

```powershell
go test ./proxy -run '^(TestClaudeToolResultMixedTextAndImage|TestOpenAIToolResultImageCarriedWhenFollowedByUser)$' -count=1 -v
```

预期：两个用例失败。根因是 `72da572` 已要求孤立/历史工具结果扁平化，而 `2ad0c56` 的图片用例仍断言旧的结构化 `ToolResults`。

- [ ] **步骤 2：把 Claude 混合图片用例改为验证新契约**

将 `TestClaudeToolResultMixedTextAndImage` 的结构化断言替换为：

```go
if cur.UserInputMessageContext != nil && len(cur.UserInputMessageContext.ToolResults) != 0 {
	t.Fatalf("孤立工具结果应扁平化为文本，不能保留结构化 ToolResults")
}
if !strings.Contains(cur.Content, "here is the screenshot") {
	t.Fatalf("工具结果文本未保留，得到 %q", cur.Content)
}
```

保留对 `cur.Images` 数量和 PNG 数据的断言。

- [ ] **步骤 3：把 OpenAI 历史图片用例改为验证扁平化后的消息归属**

将历史图片统计改为：

```go
var toolHistImages int
for _, h := range payload.ConversationState.History {
	if h.UserInputMessage == nil {
		continue
	}
	if strings.Contains(h.UserInputMessage.Content, toolResultsContinuationPrefix) {
		if h.UserInputMessage.UserInputMessageContext != nil {
			t.Fatalf("历史工具结果应已扁平化")
		}
		toolHistImages += len(h.UserInputMessage.Images)
	}
}
```

继续断言当前用户消息不携带该图片。

- [ ] **步骤 4：验证定向测试和全量基线**

运行：

```powershell
go test ./proxy -run '^(TestClaudeToolResultMixedTextAndImage|TestOpenAIToolResultImageCarriedWhenFollowedByUser)$' -count=1 -v
go test ./...
go build ./...
go vet ./...
```

预期：全部通过。

- [ ] **步骤 5：记录基线环境与已知 race 限制**

在结果文档中记录：

```markdown
# Kiro-Go 改造前基线

- 提交：`2ab979f`
- 普通测试：`go test ./...`
- 构建：`go build ./...`
- 静态检查：`go vet ./...`
- Windows race 限制：当前 `CGO_ENABLED=0`；本机缺少 `gcc`，最终 race 验证在 Linux/毕业机执行。
- 已修正的既有测试契约：历史工具结果扁平化后仍保留图片归属，不恢复结构化历史 ToolResults。
```

- [ ] **步骤 6：提交基线修复**

```powershell
git add proxy/translator_test.go docs/superpowers/results/2026-07-14-kiro-go-baseline.md
git commit -m "测试：修正工具结果图片基线契约"
```

---

### 任务 2：建立双遍分析与首字性能基线

**文件：**

- 新建：`proxy/claude_request_benchmark_test.go`
- 新建：`proxy/claude_first_token_benchmark_test.go`
- 修改：`docs/superpowers/results/2026-07-14-kiro-go-baseline.md`

- [ ] **步骤 1：新增代表性请求生成器**

在测试文件中新增：

```go
func benchmarkClaudeRequest(size int) *ClaudeRequest {
	text := strings.Repeat("package main\nfunc main() { println(\"cache benchmark\") }\n", size/56+1)
	if len(text) > size {
		text = text[:size]
	}
	return &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		System: []interface{}{
			map[string]interface{}{"type": "text", "text": "You are a coding agent."},
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
		Messages: []ClaudeMessage{{Role: "user", Content: text}},
	}
}
```

- [ ] **步骤 2：新增旧双遍基准**

```go
func benchmarkLegacyClaudeAnalysis(b *testing.B, size int) {
	req := benchmarkClaudeRequest(size)
	tracker := newPromptCacheTracker(time.Hour)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tokens := estimateClaudeRequestInputTokens(req)
		_ = tracker.BuildClaudeProfile(req, tokens)
	}
}

func BenchmarkLegacyClaudeAnalysis1KB(b *testing.B)   { benchmarkLegacyClaudeAnalysis(b, 1<<10) }
func BenchmarkLegacyClaudeAnalysis64KB(b *testing.B)  { benchmarkLegacyClaudeAnalysis(b, 64<<10) }
func BenchmarkLegacyClaudeAnalysis512KB(b *testing.B) { benchmarkLegacyClaudeAnalysis(b, 512<<10) }
func BenchmarkLegacyClaudeAnalysis2MB(b *testing.B)   { benchmarkLegacyClaudeAnalysis(b, 2<<20) }
```

- [ ] **步骤 3：运行并记录基线**

```powershell
go test ./proxy -run '^$' -bench '^BenchmarkLegacyClaudeAnalysis' -benchmem -count=5
```

把 `ns/op`、`B/op`、`allocs/op` 的五次结果原样追加到基线文档。

- [ ] **步骤 4：建立即时假上游首字基线**

在 `proxy/claude_first_token_benchmark_test.go` 中复用 `httptest.Server` 和可恢复的 Kiro HTTP client 测试钩子。假上游收到请求后立即写出一个最小 event-stream 文本事件和完成 usage。

基准接口：

```go
func benchmarkClaudeFirstToken(b *testing.B, size int)

func BenchmarkClaudeFirstToken1KB(b *testing.B)   { benchmarkClaudeFirstToken(b, 1<<10) }
func BenchmarkClaudeFirstToken64KB(b *testing.B)  { benchmarkClaudeFirstToken(b, 64<<10) }
func BenchmarkClaudeFirstToken512KB(b *testing.B) { benchmarkClaudeFirstToken(b, 512<<10) }
func BenchmarkClaudeFirstToken2MB(b *testing.B)   { benchmarkClaudeFirstToken(b, 2<<20) }
```

每次迭代测量从进入 `/v1/messages` 到 `message_start` 或首个内容 SSE 写入 `httptest.ResponseRecorder` 的时间，不把假上游响应等待计入额外延迟。

运行：

```powershell
go test ./proxy -run '^$' -bench '^BenchmarkClaudeFirstToken' -benchmem -count=10
```

把各尺寸结果追加到基线文档。

- [ ] **步骤 5：提交性能基线**

```powershell
git add proxy/claude_request_benchmark_test.go proxy/claude_first_token_benchmark_test.go docs/superpowers/results/2026-07-14-kiro-go-baseline.md
git commit -m "测试：建立 Claude 请求分析性能基线"
```

---

### 任务 3：实现单遍 Claude 请求分析器

**文件：**

- 新建：`proxy/claude_request_analysis.go`
- 新建：`proxy/claude_request_analysis_test.go`
- 修改：`proxy/token_estimator.go`
- 修改：`proxy/claude_request_benchmark_test.go`
- 修改：`proxy/cache_tracker.go`

- [ ] **步骤 1：先写分析结果和隔离行为的失败测试**

测试必须覆盖：

```go
func TestAnalyzeClaudeRequestWorksWithoutCacheControl(t *testing.T)
func TestAnalyzeClaudeRequestTaskKeyUsesAPIKeyScope(t *testing.T)
func TestAnalyzeClaudeRequestTaskKeyIgnoresUpstreamAccount(t *testing.T)
func TestAnalyzeClaudeRequestTaskKeyChangesWithToolsSystemOrFirstUser(t *testing.T)
func TestAnalyzeClaudeRequestFingerprintChangesWithLaterMessages(t *testing.T)
func TestAnalyzeClaudeRequestExcludesCacheControlAndBillingHeader(t *testing.T)
func TestAnalyzeClaudeRequestProducesMessageEndPrefixes(t *testing.T)
func TestAnalyzeClaudeRequestCountsToolContent(t *testing.T)
func TestAnalyzeClaudeRequestMatchesLegacyTokenEstimate(t *testing.T)
```

API 形状固定为：

```go
type claudePrefixPoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
}

type claudeRequestAnalysis struct {
	EstimatedInputTokens int
	CacheableTokens      int
	ToolTokens           int
	TaskKey              [32]byte
	RequestFingerprint   [32]byte
	Prefixes             []claudePrefixPoint
}

func analyzeClaudeRequest(req *ClaudeRequest, callerScope string) claudeRequestAnalysis
```

匿名调用方使用固定常量：

```go
const anonymousCallerScope = "anonymous"
```

- [ ] **步骤 2：运行分析器测试并确认 RED**

```powershell
go test ./proxy -run '^TestAnalyzeClaudeRequest' -count=1 -v
```

预期：因类型和函数不存在而失败。

- [ ] **步骤 3：实现稳定增量编码**

分析器必须按以下顺序向哈希写入语义块：

```text
model -> tools -> system -> messages
```

实现要求：

```go
func writeAnalysisString(h hash.Hash, kind, value string)
func writeAnalysisValue(h hash.Hash, kind string, value interface{})
func writeCanonicalAnalysisJSON(w io.Writer, value interface{})
```

规则：

- map key 排序；
- 排除 `cache_control`；
- 排除 `x-anthropic-billing-header:` 文本块；
- 不写数组位置、临时请求 ID、API Key 原文或上游账号 ID；
- 图片、工具调用、工具结果等语义内容必须参与完整请求指纹；
- 只保存哈希和 token 数，不保存原文。

- [ ] **步骤 4：在同一次遍历中生成任务键和请求指纹**

任务键输入固定为：

```text
caller_scope
+ normalized model
+ normalized tools
+ normalized system
+ first non-empty user message
```

完整请求指纹覆盖全部规范化请求。每个消息结束位置复制当前 SHA-256 状态，形成精确前缀断点。

- [ ] **步骤 5：复用 token 启发式并移除 handler 双遍依赖**

保留：

```go
estimateApproxTokens
estimateClaudeOutputTokens
estimateOpenAIRequestInputTokens
```

`estimateClaudeRequestInputTokens` 可暂时保留为兼容包装：

```go
func estimateClaudeRequestInputTokens(req *ClaudeRequest) int {
	return analyzeClaudeRequest(req, anonymousCallerScope).EstimatedInputTokens
}
```

任务 6 完成 handler 接入后再决定是否删除包装。

- [ ] **步骤 6：验证 GREEN 与性能**

```powershell
go test ./proxy -run '^TestAnalyzeClaudeRequest' -count=1 -v
go test ./proxy -run '^$' -bench '^Benchmark(New|Legacy)ClaudeAnalysis' -benchmem -count=5
```

把基准中的新路径命名为：

```go
func BenchmarkNewClaudeAnalysis1KB(b *testing.B)
func BenchmarkNewClaudeAnalysis64KB(b *testing.B)
func BenchmarkNewClaudeAnalysis512KB(b *testing.B)
func BenchmarkNewClaudeAnalysis2MB(b *testing.B)
```

- [ ] **步骤 7：提交单遍分析器**

```powershell
git add proxy/claude_request_analysis.go proxy/claude_request_analysis_test.go proxy/token_estimator.go proxy/claude_request_benchmark_test.go proxy/cache_tracker.go
git commit -m "功能：新增单遍 Claude 请求分析器"
```

---

### 任务 4：重构调用方任务生命周期与单 TTL 状态

**文件：**

- 重写：`proxy/cache_tracker.go`
- 重写：`proxy/cache_tracker_test.go`

- [ ] **步骤 1：先写生命周期失败测试**

覆盖以下行为：

```go
func TestPromptCacheTrackerUsesCallerAndTaskKey(t *testing.T)
func TestPromptCacheTrackerRetryFingerprintIsIdempotent(t *testing.T)
func TestPromptCacheTrackerFindsLongestSuccessfulPrefix(t *testing.T)
func TestPromptCacheTrackerDoesNotAdvanceOnFailedRequest(t *testing.T)
func TestPromptCacheTrackerFiveMinuteReadRefreshesActivity(t *testing.T)
func TestPromptCacheTrackerOneHourReadDoesNotExtendCreationExpiry(t *testing.T)
func TestPromptCacheTrackerExpiredTaskRebuilds(t *testing.T)
func TestPromptCacheTrackerPrunesAfterSeventyMinutes(t *testing.T)
func TestPromptCacheTrackerIgnoresUpstreamAccountSwitch(t *testing.T)
func TestPromptCacheTrackerTTLDistributionIsStableTwentyEighty(t *testing.T)
```

新接口：

```go
type promptCachePhase uint8

const (
	promptCachePhaseFirst promptCachePhase = iota
	promptCachePhaseContinue
	promptCachePhaseRebuild
)

type promptCacheTTL uint8

const (
	promptCacheTTL5m promptCacheTTL = iota + 1
	promptCacheTTL1h
)

type promptCacheSnapshot struct {
	TaskKey             [32]byte
	RequestFingerprint  [32]byte
	TTL                  promptCacheTTL
	Phase                promptCachePhase
	MatchedPrefixTokens  int
	CurrentCacheable     int
	SuccessfulRounds     int
	AgeRatio             float64
	ExistingUsage        *ClaudeUsage
}

type promptCacheCommit struct {
	TaskKey             [32]byte
	RequestFingerprint  [32]byte
	TTL                  promptCacheTTL
	Prefixes             []claudePrefixPoint
	Usage                ClaudeUsage
	SuccessfulAt         time.Time
}

func (t *promptCacheTracker) Snapshot(analysis claudeRequestAnalysis, now time.Time) promptCacheSnapshot
func (t *promptCacheTracker) Commit(commit promptCacheCommit)
```

- [ ] **步骤 2：运行生命周期测试并确认 RED**

```powershell
go test ./proxy -run '^TestPromptCacheTracker' -count=1 -v
```

- [ ] **步骤 3：实现稳定 TTL 分配**

```go
const promptCacheAlgorithmVersion = "native-high-cache-v1"

func ttlForTask(taskKey [32]byte) promptCacheTTL {
	sum := sha256.Sum256(append(taskKey[:], promptCacheAlgorithmVersion...))
	if binary.BigEndian.Uint16(sum[:2])%100 < 20 {
		return promptCacheTTL5m
	}
	return promptCacheTTL1h
}
```

同一任务只能返回一种 TTL。

- [ ] **步骤 4：实现分片状态和两阶段提交**

使用固定分片减少全局锁竞争：

```go
const promptCacheShardCount = 32

type promptCacheShard struct {
	mu    sync.Mutex
	tasks map[[32]byte]*promptCacheTaskState
}

type promptCacheTracker struct {
	shards [promptCacheShardCount]promptCacheShard
}
```

`Snapshot` 只复制最小状态，不推进轮次；`Commit` 只在求解成功且上游成功后写入。幂等指纹命中时返回已有 usage。

- [ ] **步骤 5：实现精确最长前缀和过期语义**

- 5 分钟任务：读取命中后刷新最后活动时间；
- 1 小时任务：读取不延长首次/最近成功创建后的 60 分钟期限；
- TTL 过期后 phase 为 rebuild；
- 70 分钟后清理任务和幂等记录；
- 不使用最小 cache token 门槛；
- 不接受上游账号 ID。

- [ ] **步骤 6：验证生命周期和并发**

```powershell
go test ./proxy -run '^TestPromptCacheTracker' -count=1 -v
go test ./proxy -run '^TestPromptCacheTracker.*Concurrent' -count=50
```

- [ ] **步骤 7：提交生命周期重构**

```powershell
git add proxy/cache_tracker.go proxy/cache_tracker_test.go
git commit -m "功能：重构调用方高缓存任务生命周期"
```

---

### 任务 5：实现特征模型与整数费用守恒求解器

**文件：**

- 新建：`proxy/claude_usage_allocator.go`
- 新建：`proxy/claude_usage_allocator_test.go`
- 新建：`proxy/claude_usage_allocator_benchmark_test.go`

- [ ] **步骤 1：先写特征敏感性和分布失败测试**

覆盖：

```go
func TestClaudeUsageTargetsFirstRoundHasCreationOnly(t *testing.T)
func TestClaudeUsageTargetsGrowthRaisesCreationShare(t *testing.T)
func TestClaudeUsageTargetsReuseRaisesReadShare(t *testing.T)
func TestClaudeUsageTargetsRoundAgeToolAndJitterAffectResult(t *testing.T)
func TestClaudeUsageTargetsProduceDiverseBuckets(t *testing.T)
func TestClaudeUsageTargetsDoNotPileUpAtBounds(t *testing.T)
```

类型：

```go
type claudeUsageFeatures struct {
	Phase         promptCachePhase
	ReuseRatio    float64
	GrowthRatio   float64
	AgeRatio      float64
	RoundFactor   float64
	SizeFactor    float64
	ToolRatio     float64
	StableJitter  float64
}

type claudeUsageTargets struct {
	InputShare  float64
	ReadShare   float64
	CreateShare float64
}
```

- [ ] **步骤 2：实现设计公式**

```go
roundFactor := clamp(math.Log2(1+float64(successfulRounds))/4, 0, 1)
sizeFactor := clamp(math.Log2(1+float64(rawInputTokens))/20, 0, 1)

inputRaw := 0.03 -
	0.015*reuseRatio +
	0.010*toolRatio +
	0.005*(1-sizeFactor) +
	0.003*stableJitter
inputShare := clamp(inputRaw, 0.01, 0.05)
cacheTotal := 1 - inputShare
```

续轮：

```go
createRaw := 0.06 +
	0.28*math.Sqrt(growthRatio) +
	0.025*ageRatio +
	0.015*toolRatio -
	0.010*roundFactor +
	0.008*stableJitter

createMin := math.Max(0.08, cacheTotal-0.90)
createMax := math.Min(0.20, cacheTotal-0.78)
createShare := clamp(createRaw, createMin, createMax)
readShare := cacheTotal - createShare
```

首轮/重建：`readShare=0`，`createShare=cacheTotal`。

- [ ] **步骤 3：先写整数守恒失败测试**

覆盖：

```go
func TestAllocateClaudeUsageConservesFiveMinuteCost(t *testing.T)
func TestAllocateClaudeUsageConservesOneHourCost(t *testing.T)
func TestAllocateClaudeUsageUsesSingleTTL(t *testing.T)
func TestAllocateClaudeUsageKeepsOutputTokens(t *testing.T)
func TestAllocateClaudeUsageRespectsPhaseBounds(t *testing.T)
func TestAllocateClaudeUsageFallsBackForTinyOrImpossibleInputs(t *testing.T)
func TestAllocateClaudeUsageRejectsOverflow(t *testing.T)
func TestAllocateClaudeUsageChecksAtMostSixtyFourCandidates(t *testing.T)
func TestAllocateClaudeUsagePreservesOpusSonnetAndHaikuBaseCost(t *testing.T)
```

- [ ] **步骤 4：实现固定 64 候选求解**

API：

```go
const maxClaudeUsageCandidates = 64

func allocateClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	ttl promptCacheTTL,
	target claudeUsageTargets,
) (ClaudeUsage, bool)
```

实现步骤：

1. 选择创建权重：5m=`25`，1h=`40`；
2. 用连续目标计算显示 token 总量：

```go
weightedUnit := 20*target.InputShare + 2*target.ReadShare + float64(createWeight)*target.CreateShare
displayTotal := int(math.Round(float64(20*rawInputTokens) / weightedUnit))
targetInput := int(math.Round(target.InputShare * float64(displayTotal)))
targetCreate := int(math.Round(target.CreateShare * float64(displayTotal)))
```

3. 使用固定 8×8 邻近偏移，共最多 64 个候选；
4. 5m 创建候选必须满足 `C % 2 == 0`；
5. 由下式反解读取 token：

```go
numerator := 20*rawInputTokens - 20*inputTokens - createWeight*creationTokens
if numerator < 0 || numerator%2 != 0 {
	continue
}
readTokens := numerator / 2
```

6. 校验非负、单 TTL、缓存率、阶段比例和创建汇总；
7. 按目标比例平方距离排序，使用稳定字段作为平局规则；
8. 用独立函数重新计算 `20*T == 20*I + 2*R + W*C`。

- [ ] **步骤 5：实现标准完整 `ClaudeUsage`**

`ClaudeUsage` 不再省略缓存字段：

```go
type ClaudeUsage struct {
	InputTokens              int                      `json:"input_tokens"`
	OutputTokens             int                      `json:"output_tokens"`
	CacheCreationInputTokens int                      `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int                      `json:"cache_read_input_tokens"`
	CacheCreation            ClaudeCacheCreationUsage `json:"cache_creation"`
}
```

- [ ] **步骤 6：验证求解、分布和性能**

```powershell
go test ./proxy -run '^TestClaudeUsage(Targets|Allocate)' -count=1 -v
go test ./proxy -run '^TestClaudeUsageTargetsProduceDiverseBuckets$' -count=10
go test ./proxy -run '^$' -bench '^BenchmarkAllocateClaudeUsage' -benchmem -count=10
```

基准要求：求解候选不超过 64，P99 目标在 Linux/毕业机小于 `200 us`。

- [ ] **步骤 7：提交求解器**

```powershell
git add proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go proxy/claude_usage_allocator_benchmark_test.go proxy/translator.go
git commit -m "功能：实现高缓存特征模型与费用守恒求解"
```

---

### 任务 6：统一同步和流式最终 usage

**文件：**

- 修改：`proxy/handler.go`
- 修改：`proxy/handler_test.go`
- 修改：`proxy/kiro.go`
- 修改：`proxy/translator.go`
- 新建：`proxy/claude_usage_contract_test.go`

- [ ] **步骤 1：先写响应合同失败测试**

覆盖：

```go
func TestClaudeNonStreamReturnsCompleteCacheUsageWithoutCacheControl(t *testing.T)
func TestClaudeStreamFinalDeltaMatchesNonStreamUsage(t *testing.T)
func TestClaudeUsageRetryAcrossAccountsIsStable(t *testing.T)
func TestClaudeFailedUpstreamDoesNotCommitCacheState(t *testing.T)
func TestClaudeRetryFingerprintDoesNotAdvanceRound(t *testing.T)
func TestClaudeMessageStartDoesNotCommitCacheState(t *testing.T)
func TestClaudeClientDisconnectCommitsOnlyAfterFinalUpstreamUsage(t *testing.T)
func TestClaudeTruncatedPayloadFallsBackWithoutCommit(t *testing.T)
func TestKiroDebugLogDoesNotContainPromptOrAPIKey(t *testing.T)
```

测试必须比较完整字段：

```text
input_tokens
output_tokens
cache_read_input_tokens
cache_creation_input_tokens
cache_creation.ephemeral_5m_input_tokens
cache_creation.ephemeral_1h_input_tokens
```

- [ ] **步骤 2：在 handler 入口只分析一次**

替换：

```go
estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)
```

为：

```go
callerScope := apiKeyIDFromContext(r.Context())
if callerScope == "" {
	callerScope = anonymousCallerScope
}
analysis := analyzeClaudeRequest(effectiveReq, callerScope)
snapshot := h.promptCache.Snapshot(analysis, time.Now())
```

同步与流式都接收 `analysis` 和 `snapshot`，不接收上游账号 ID 作为缓存参数。

- [ ] **步骤 3：抽取统一最终分配函数**

```go
type preparedClaudeUsage struct {
	Usage  ClaudeUsage
	Commit promptCacheCommit
	OK     bool
}

func prepareFinalClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	analysis claudeRequestAnalysis,
	snapshot promptCacheSnapshot,
	now time.Time,
) preparedClaudeUsage
```

如果已有幂等 usage，直接复用；求解失败时返回原始 usage 且 `OK=false`，不提交状态。

- [ ] **步骤 4：流式只在最终 `message_delta` 使用最终 usage**

- `message_start.message.usage` 使用开始阶段估算，缓存字段为零；
- 账号切换不重新生成生命周期快照；
- 上游成功并得到最终 input/output 后调用 `prepareFinalClaudeUsage`；
- `message_delta.usage` 写出完整字段；
- 客户端仍连接时，最终事件写成功后提交；
- 客户端已断开但上游已排空且最终 usage 完整时允许提交；
- 上游失败或最终 usage 不完整时不提交。

把：

```go
func (h *Handler) sendSSE(...)
```

改为：

```go
func (h *Handler) sendSSE(...) error
```

并检查 `fmt.Fprintf` 返回值。

- [ ] **步骤 5：同步响应消费同一个 `ClaudeUsage`**

移除同步路径逐字段二次计算，改为：

```go
resp := KiroToClaudeResponse(...)
resp.Usage = prepared.Usage
```

编码成功后提交；编码失败时记录错误并不提交。

- [ ] **步骤 6：对超大截断请求安全降级**

让：

```go
func truncatePayloadToLimit(payload *KiroPayload, hasPriming bool) bool
```

返回是否发生截断。`KiroPayload` 使用不参与 JSON 序列化的内部标记保存结果。若实际转发 payload 已截断，则最终返回原始 usage，不提交高缓存任务状态，避免把未真正发送给 Kiro 的前缀登记为成功前缀。

- [ ] **步骤 7：删除敏感完整 payload 日志**

`proxy/kiro.go` 的 debug 日志不得打印完整 payload。替换为：

```go
logger.Debugf(
	"[KiroAPI] request model=%s payload_bytes=%d payload_sha256=%x",
	modelID,
	len(requestBody),
	sha256.Sum256(requestBody),
)
```

不得打印提示词、图片、API Key、Authorization、完整邮箱或 OAuth 数据。

- [ ] **步骤 8：验证合同、首字与回归**

```powershell
go test ./proxy -run '^Test(ClaudeNonStreamReturnsCompleteCacheUsage|ClaudeStreamFinalDeltaMatchesNonStreamUsage|ClaudeUsageRetryAcrossAccounts|ClaudeFailedUpstream|ClaudeRetryFingerprint|ClaudeMessageStart|ClaudeClientDisconnect|ClaudeTruncatedPayload|KiroDebugLog)' -count=1 -v
go test ./proxy -run '^$' -bench '^BenchmarkClaudeFirstToken' -benchmem -count=10
go test ./...
```

- [ ] **步骤 9：提交 handler 接入**

```powershell
git add proxy/handler.go proxy/handler_test.go proxy/kiro.go proxy/translator.go proxy/claude_usage_contract_test.go proxy/claude_first_token_benchmark_test.go
git commit -m "功能：统一 Claude 同步与流式最终用量"
```

---

### 任务 7：统一 Kiro-Go 8321 端口、Docker 网络与中文说明

**文件：**

- 修改：`config/config.go`
- 修改：`config/config_test.go`
- 修改：`config/apikeys_test.go`
- 修改：`Dockerfile`
- 修改：`docker-compose.yml`
- 修改：`README.md`
- 修改：`README_CN.md`

- [ ] **步骤 1：先写默认端口失败测试**

```go
func TestDefaultPortIs8321(t *testing.T) {
	resetTestConfig(t)
	if got := GetPort(); got != 8321 {
		t.Fatalf("默认端口=%d，期望 8321", got)
	}
}
```

更新现有配置 fixture 中与默认值相关的 `8080` 为 `8321`；代理 URL 测试中的业务示例端口不改。

- [ ] **步骤 2：运行并确认 RED**

```powershell
go test ./config -run '^TestDefaultPortIs8321$' -count=1 -v
```

- [ ] **步骤 3：修改默认配置和容器契约**

`config/config.go`：

```go
const defaultServerPort = 8321
```

新配置和零值回退统一使用该常量。

`Dockerfile`：

```dockerfile
EXPOSE 8321
```

`docker-compose.yml`：

```yaml
services:
  kiro-go:
    container_name: kiro-go-pr131
    ports:
      - "127.0.0.1:8321:8321"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "-", "http://127.0.0.1:8321/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    networks:
      sub2api_sub2api-network:
        aliases:
          - kiro-go-pr131

networks:
  sub2api_sub2api-network:
    external: true
```

若最终镜像没有 `wget`，在 Dockerfile 中安装轻量健康检查工具，或改用镜像中已存在的命令。

- [ ] **步骤 4：更新 README**

两份 README：

- 所有服务示例改为 `8321`；
- 主机映射改为 `127.0.0.1:8321:8321`；
- 增加 Fork 差异：Kiro-Go 原生生成标准高缓存 usage；
- 明确容器内 URL 为 `http://kiro-go-pr131:8321`；
- 明确公网不得直接暴露该端口；
- 中文文档和新增说明使用简体中文。

- [ ] **步骤 5：验证**

```powershell
go test ./config ./proxy
docker compose config
docker build -t kiro-go:native-high-cache-test .
```

- [ ] **步骤 6：提交端口和文档**

```powershell
git add config/config.go config/config_test.go config/apikeys_test.go Dockerfile docker-compose.yml README.md README_CN.md
git commit -m "配置：统一 Kiro-Go 8321 内网拓扑"
```

---

### 任务 8：为 Sub2API 清退编写原生解析与计费回归测试

**文件：**

- 修改：`backend/internal/service/gateway_anthropic_apikey_passthrough_test.go`
- 修改：`backend/internal/service/gateway_record_usage_test.go`
- 新建：`backend/internal/service/equivalent_cache_cleanup_regression_test.go`
- 修改：`backend/internal/service/model_pricing_resolver_test.go`
- 修改：`backend/migrations/migration_filename_uniqueness_test.go`

- [ ] **步骤 1：先写“账户 Extra 不得改写 usage”的失败测试**

```go
func TestEquivalentCacheCleanupAccountExtraCannotRewriteAnthropicUsage(t *testing.T)
```

构造启用以下旧配置的 Kiro APIKey 账户：

```json
{
  "equivalent_cache_billing_enabled": true,
  "equivalent_cache_allocation_v2": {
    "enabled": true,
    "mode": "active",
    "pricing_profile": "kiro_unified_5_25_0_6_6_25_10",
    "visible_rate_min": 0.96,
    "visible_rate_max": 0.999,
    "kiro_go_pool_confirmed": true
  }
}
```

上游返回标准同步 usage 后，断言响应体和 `ClaudeUsage` 六类字段完全等于上游。

- [ ] **步骤 2：写最终 SSE 覆盖测试**

```go
func TestEquivalentCacheCleanupFinalMessageDeltaOverridesNativeUsage(t *testing.T)
```

输入包含 `message_start` 临时值和最终 `message_delta.usage`，断言最终 input/output/read/create/5m/1h 全部采用 delta。

- [ ] **步骤 3：写分模型标准价格关系测试**

分别构造 Opus、Sonnet、Haiku 定价，断言：

```text
cache_read = 0.10 * input
cache_write_5m = 1.25 * input
cache_write_1h = 2.00 * input
```

错误配置必须返回可识别的验收失败，不得套用 Opus 的绝对价格。

- [ ] **步骤 4：写倍率只在基础费用后应用的测试**

同一 usage、同一模型、不同有效分组倍率，断言基础费用相同，实际费用只按倍率成比例变化。

- [ ] **步骤 5：写历史迁移保留测试**

```go
func TestEquivalentCacheAuditMigrationRemainsHistorical(t *testing.T)
```

断言 `174_usage_log_equivalent_cache_v2_audit.sql` 仍存在且没有新增 `DROP COLUMN` 迁移。

- [ ] **步骤 6：运行并确认 RED**

```powershell
go test ./internal/service -run '^TestEquivalentCacheCleanup' -count=1 -v
go test ./internal/service -run 'Test.*StandardCachePrice|Test.*GroupMultiplier' -count=1 -v
go test ./migrations -run '^TestEquivalentCacheAuditMigrationRemainsHistorical$' -count=1 -v
```

预期：至少账户 Extra 改写测试失败，证明旧 V1/V2 路径仍在运行。

- [ ] **步骤 7：提交回归测试**

```powershell
git add backend/internal/service/gateway_anthropic_apikey_passthrough_test.go backend/internal/service/gateway_record_usage_test.go backend/internal/service/equivalent_cache_cleanup_regression_test.go backend/internal/service/model_pricing_resolver_test.go backend/migrations/migration_filename_uniqueness_test.go
git commit -m "测试：固化 Equivalent Cache 清退回归契约"
```

---

### 任务 9：定向删除 Sub2API Equivalent Cache V1/V2 运行时

**文件：**

- 删除：`backend/internal/service/equivalent_cache_billing.go`
- 删除：`backend/internal/service/equivalent_cache_billing_test.go`
- 删除：`backend/internal/service/equivalent_cache_v2_allocator.go`
- 删除：`backend/internal/service/equivalent_cache_v2_allocator_test.go`
- 删除：`backend/internal/service/equivalent_cache_v2_response.go`
- 删除：`backend/internal/service/equivalent_cache_v2_response_test.go`
- 删除：`backend/internal/service/equivalent_cache_v2_session.go`
- 删除：`backend/internal/service/equivalent_cache_v2_session_test.go`
- 删除：`backend/internal/service/equivalent_cache_v2_types.go`
- 删除：`backend/internal/service/equivalent_cache_v2_types_test.go`
- 删除：`backend/internal/repository/equivalent_cache_v2_state.go`
- 删除：`backend/internal/repository/equivalent_cache_v2_state_test.go`
- 修改：`backend/internal/service/gateway_service.go`
- 修改：`backend/internal/service/gateway_forward.go`
- 修改：`backend/internal/service/gateway_anthropic_passthrough.go`
- 修改：`backend/internal/service/gateway_upstream_response.go`
- 修改：`backend/internal/service/gateway_usage_billing.go`
- 修改：`backend/internal/handler/gateway_handler.go`
- 修改：`backend/internal/handler/failover_loop.go`
- 修改：`backend/internal/repository/wire.go`
- 生成：`backend/cmd/server/wire_gen.go`
- 修改：对应测试和构造函数

- [ ] **步骤 1：以 `684a3f6b^` 为定向参考，不整体回退文件**

对每个共享文件查看：

```powershell
git diff 684a3f6b^ -- backend/internal/service/gateway_service.go
git show 684a3f6b^:backend/internal/service/gateway_service.go
```

只移除 Equivalent Cache 专属字段、构造参数和调用；保留之后的上游合并、Docker 更新、中文发布、OpenAI/Gemini/图片/视频等改动。

- [ ] **步骤 2：删除专属模块**

删除上述 V1/V2 service/repository 文件及其专属测试。

- [ ] **步骤 3：清理 `GatewayService` 和 `ForwardResult`**

移除：

- `EquivalentCacheV2StateStore`；
- V2 响应计划和响应改写结果；
- 原始 usage 快照、分配版本、分配种类、动态价格快照等只服务 V2 的字段；
- V2 构造参数、日志和响应头。

保留：

- 标准 `ClaudeUsage`；
- `parseSSEUsagePassthrough`；
- `parseClaudeUsageFromResponseBody`；
- 原生 `ForceCacheBilling`；
- 原生 `cache_ttl_override` 实现；
- 普通价格、倍率和扣费路径。

- [ ] **步骤 4：清理同步与流式改写接入点**

`gateway_anthropic_passthrough.go`：

- 删除 `prepareEquivalentCacheV2ResponsePlan`；
- 删除 `applyEquivalentCacheV2JSON/SSE`；
- 删除 V2 双轨辅助层 `parseRawSSEUsage` 和 `reconcileCachedTokensInSSEEvent`；
- 保持同步 body 原样返回；
- 保持最终 `message_delta.usage` 原生解析。

`gateway_upstream_response.go`：

- 删除 V2 响应二次改写；
- 保留 `applyCacheTTLOverride` 函数本身；
- 后续仅通过部署配置关闭 Kiro 账户的 override。

- [ ] **步骤 5：清理计费与审计写入**

`gateway_usage_billing.go`：

- 删除 `prepareEquivalentCacheV2BillingResults`；
- 删除 V2 双 usage、锁价和动态价格快照；
- 删除 raw usage 与 allocation version/kind 的运行时写入；
- 保留基础费用后应用用户/分组/系统有效倍率。

- [ ] **步骤 6：清理 failover 与 handler 依赖**

删除只为 V2 携带响应体、价格快照和状态存储的字段，以及 V2 新增的 `ForwardContext` 传播调用。`ForceCacheBilling`、`needForceCacheBilling` 和其原生上下文 API 必须保留，因为它们服务粘性会话切换，不属于 V1/V2。

更新 `backend/internal/repository/wire.go` 后运行：

```powershell
go generate ./cmd/server
```

重新生成 `wire_gen.go`，不得手工拼接构造参数。

- [ ] **步骤 7：运行定向回归**

```powershell
go test ./internal/service -run '^TestEquivalentCacheCleanup' -count=1 -v
go test ./internal/service -run 'Test(ParseSSEUsagePassthrough|ParseClaudeUsage|ForceCacheBilling|CacheTTLOverride)' -count=1 -v
go test ./internal/handler -run 'TestFailover.*ForceCacheBilling' -count=1 -v
```

- [ ] **步骤 8：提交运行时清退**

```powershell
git add -A backend/internal/service backend/internal/repository backend/internal/handler
git commit -m "重构：清退 Equivalent Cache V1 V2 运行时"
```

---

### 任务 10：移除 Sub2API 运行时审计字段，保留历史迁移

**文件：**

- 修改：`backend/ent/schema/usage_log.go`
- 生成：`backend/ent/**`
- 修改：`backend/internal/repository/usage_log_repo.go`
- 修改：`backend/internal/repository/usage_log_repo_insert.go`
- 修改：`backend/internal/repository/usage_log_repo_query.go`
- 修改：`backend/internal/repository/usage_log_repo_request_type_test.go`
- 修改：`backend/internal/handler/dto/mappers.go`
- 修改：`backend/internal/handler/dto/mappers_usage_test.go`
- 修改：`backend/internal/handler/dto/types.go`
- 修改：`backend/internal/service/usage_log.go`
- 保留不变：`backend/migrations/174_usage_log_equivalent_cache_v2_audit.sql`

- [ ] **步骤 1：先写 ORM 不暴露旧字段的失败测试**

更新 DTO 与 repository 测试，使它们不再期望：

```text
raw_input_tokens
raw_output_tokens
raw_cache_read_tokens
raw_cache_creation_tokens
raw_cache_creation_5m_tokens
raw_cache_creation_1h_tokens
usage_allocation_version
usage_allocation_kind
```

数据库历史列允许存在，但应用层不得读写或返回这些 V2 专属字段。

- [ ] **步骤 2：移除 Ent schema 字段并重新生成**

从 `backend/ent/schema/usage_log.go` 删除上述 V2 审计字段，然后运行：

```powershell
make -C backend generate
```

生成文件由 Ent 统一更新，不手工编辑。

- [ ] **步骤 3：更新原生 SQL repository**

从 `usageLogSelectColumns`、INSERT 列表、参数类型、扫描目标和统计查询中删除旧字段。不要修改历史迁移文件。

- [ ] **步骤 4：验证带额外历史列的 PostgreSQL 兼容性**

普通单元测试：

```powershell
go test ./internal/repository ./internal/handler/dto ./ent/schema ./migrations
```

集成环境可用时：

```powershell
go test -tags=integration ./internal/repository -run 'TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate|TestUsageLogRepoSuite' -count=1 -v
```

预期：ORM 未声明额外列时仍可正常启动和读写。

- [ ] **步骤 5：提交审计字段清理**

```powershell
git add backend/ent backend/internal/repository backend/internal/handler/dto backend/migrations
git commit -m "重构：停止读写 Equivalent Cache 审计字段"
```

---

### 任务 11：清理 Sub2API 文档、运维入口与 Kiro 配置说明

**文件：**

- 修改：`README.md`
- 修改：`README_CN.md`
- 删除：`docs/EQUIVALENT_CACHE_BILLING_CN.md`
- 删除：`docs/superpowers/specs/2026-07-12-kiro-go-cost-locked-equivalent-cache-v2-design.md`
- 删除：`docs/superpowers/specs/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix-design.md`
- 删除：`docs/superpowers/specs/2026-07-14-equivalent-cache-v2-account-pricing-and-kiro-internal-network-design.md`
- 删除：`docs/superpowers/plans/2026-07-12-kiro-go-cost-locked-equivalent-cache-v2.md`
- 删除：`docs/superpowers/plans/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix.md`
- 修改：`skills/sub2api-admin/SKILL.md`
- 修改：`skills/sub2api-admin/references/admin-cli.md`
- 修改：根工作区 `CURRENT_STATE.md`

- [ ] **步骤 1：删除废弃 V1/V2 文档**

只删除设计明确列出的废弃文档；保留当前 Kiro-Go 原生高缓存设计和实施计划。

- [ ] **步骤 2：清理 README**

删除 Sub2API 自身提供 Equivalent Cache 的声明，改为：

```text
Anthropic 高缓存 usage 由上游兼容服务返回；Sub2API 负责标准字段解析、分模型定价、有效倍率和扣费。
```

- [ ] **步骤 3：修订运维技能**

删除 V2 开关、账号资格、Redis 状态和专属审计操作。保留通用账户查询、批量更新、渠道价格与健康检查。

修正 Kiro Base URL 的实际批量更新形状：

```bash
node scripts/sub2api-admin.js accounts bulk-update --ids "$ACCOUNT_ID" \
  --json '{"credentials":{"base_url":"http://kiro-go-pr131:8321"}}'
```

关闭 Kiro TTL override：

```bash
node scripts/sub2api-admin.js accounts bulk-update --ids "$ACCOUNT_ID" \
  --json '{"extra":{"cache_ttl_override_enabled":false}}'
```

- [ ] **步骤 4：更新 CURRENT_STATE**

只记录当前有效状态：

- Kiro-Go PR #131 使用 `8321`；
- 容器内 URL 为 `http://kiro-go-pr131:8321`；
- Sub2API 不再运行 Equivalent Cache V1/V2；
- 历史审计列保留但不读写；
- 生产仍等待单独授权。

- [ ] **步骤 5：提交文档清理**

```powershell
git add -A README.md README_CN.md docs skills
git commit -m "文档：清理 Equivalent Cache 旧运维说明"
```

根工作区 `CURRENT_STATE.md` 单独提交到其所属仓库；如果根目录不是 Git 仓库，则保留为工作区运维文档修改，不混入两个代码仓库提交。

---

### 任务 12：双仓库完整验证与最终代码审查

**文件：**

- 修改：`docs/superpowers/results/2026-07-14-kiro-go-baseline.md`
- 新建：Sub2API `docs/superpowers/results/2026-07-14-equivalent-cache-cleanup-verification.md`

- [ ] **步骤 1：Kiro-Go 全量验证**

```powershell
$goFiles = rg --files proxy config -g '*.go'
gofmt -w $goFiles
go test ./...
go build ./...
go vet ./...
go test ./proxy -run '^$' -bench 'Benchmark(NewClaudeAnalysis|AllocateClaudeUsage)' -benchmem -count=10
```

Linux/毕业机：

```bash
go test -race ./...
```

- [ ] **步骤 2：Sub2API 后端验证**

```powershell
$goFiles = rg --files backend/internal backend/ent/schema -g '*.go'
gofmt -w $goFiles
Push-Location backend
go test ./...
go test -tags=unit ./...
go build ./cmd/server
Pop-Location
```

仓库根目录：

```powershell
pnpm --dir frontend install --frozen-lockfile
make test
make build
```

- [ ] **步骤 3：静态残留扫描**

```powershell
rg -n "equivalent_cache|EquivalentCache|UsageAllocationVersion|UsageAllocationKind" backend/internal backend/ent README* docs skills
```

允许残留：

- 历史迁移文件名和迁移兼容测试；
- 当前清退设计/实施/验证文档中的历史说明。

禁止残留：

- V1/V2 运行时模块、配置解析、响应改写、Redis 状态、资格和审计写入。

- [ ] **步骤 4：价格合同扫描**

确认 Kiro-Go 中不存在任何模型美元价格表：

```powershell
rg -n '\$[0-9]|MTok|0\.50|6\.25|10\.00|18\.75|30\.00' proxy
```

只允许无量纲权重 `20/2/25/40` 和测试中的分模型复算样例。

- [ ] **步骤 5：最终独立审查**

审查重点：

- 单遍分析是否实际只遍历请求一次；
- `cache_control` 是否完全不影响资格/TTL；
- 上游账号 ID 是否完全不参与任务键和状态；
- 同步/流式最终 usage 是否同源；
- 求解是否整数严格守恒、候选上限固定；
- Sub2API 是否保留原生解析和倍率；
- 历史迁移是否未被删除或改为 DROP；
- 端口和网络是否全部为 8321。

- [ ] **步骤 6：记录验证证据并提交**

```powershell
git add docs/superpowers/results
git commit -m "验证：记录原生高缓存与清退结果"
```

---

### 任务 13：毕业机备份、部署与真实链路验收

**文件：**

- 新建：`docs/superpowers/results/2026-07-14-graduation-native-cache-acceptance.md`
- 修改：根工作区 `CURRENT_STATE.md`

- [ ] **步骤 1：确认毕业机部署目录**

在只读 SSH 会话中定位 Kiro-Go Compose、配置和镜像来源。不得假设目录，不得改生产机。

- [ ] **步骤 2：备份毕业机**

备份：

- Kiro-Go 配置和 Compose；
- Sub2API Compose、`.env`、相关账户 `credentials/extra`；
- 分组与渠道价格；
- 当前精确镜像摘要；
- 数据库迁移状态和必要数据库备份。

备份路径、时间和摘要写入验收文档，不记录密钥、完整邮箱或 OAuth 数据。

- [ ] **步骤 3：部署 Kiro-Go**

仅定向重建 Kiro-Go，验证：

```bash
curl -fsS http://127.0.0.1:8321/health
docker exec sub2api getent hosts kiro-go-pr131
docker exec sub2api sh -lc 'wget -q -O - http://kiro-go-pr131:8321/health'
```

- [ ] **步骤 4：清理毕业机 Sub2API 配置**

先备份账户 JSON，再：

- 更新 `credentials.base_url` 为 `http://kiro-go-pr131:8321`；
- 关闭 `cache_ttl_override_enabled`；
- 删除/禁用 Equivalent Cache V1/V2 Extra；
- 恢复 Opus、Sonnet、Haiku 各自标准缓存相对价格。

JSONB 合并无法真正删除旧键时，显式写入 `enabled:false`，并记录遗留键只作为无效历史配置。

- [ ] **步骤 5：仅定向重建 Sub2API**

不得重建 PostgreSQL、Redis、Caddy、CPA、QQ/NapCat 或其他无关服务。记录变更前后容器启动时间。

- [ ] **步骤 6：执行真实同步和流式验收**

至少覆盖：

- 首轮创建；
- 后续读取加单 TTL 创建；
- 5m 与 1h 任务；
- Kiro 上游账号换源；
- 两个不同分组倍率；
- Sub2API 与下游兼容面板的同步/流式解析。

- [ ] **步骤 7：记录量化结果**

必须记录：

```text
缓存总比例
后续读取比例
后续创建比例
5m/1h 任务分布
无量纲费用差额
Opus/Sonnet/Haiku 基础费用差额
输出 token 差额
同步/流式字段差异
首字 P95 变化
求解 P99
```

所有费用差额目标为 0；首字 P95 持续回归目标不超过 1%。

- [ ] **步骤 8：更新当前状态**

只在证据齐全后更新 `CURRENT_STATE.md`。不得写“生产已完成”，除非用户之后明确授权并且生产验收实际通过。

---

### 任务 14：生产变更前授权关卡

- [ ] **步骤 1：整理精确发布信息**

包括：

- 两个仓库提交；
- Kiro-Go 精确镜像与摘要；
- Sub2API `0.1.152` 重建资产与摘要；
- 毕业机验收结果；
- 备份和回滚命令；
- 只重建 Kiro-Go/Sub2API 的变更清单。

- [ ] **步骤 2：停止自动执行并请求用户明确授权**

生产机 `154.36.172.65` 上的任何备份、配置、价格、账户、镜像、更新器、容器和真实流量操作都必须在此处停下，等待用户明确授权。

- [ ] **步骤 3：获授权后按定向流程执行**

只允许：

- 先备份；
- 先验证 Docker 内网 `kiro-go-pr131:8321`；
- 再验证主机 `127.0.0.1:8321`；
- 最后执行同步和流式真实请求；
- 只重建 Kiro-Go 和 Sub2API；
- 比较所有无关容器启动时间；
- 失败时分别回滚代码、镜像、网络和配置。
