# Kiro-Go 缓存分布 V4 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在仅修改 Kiro-Go 的前提下，将正常请求命中率校准到约 92.8%，将超大截断请求改为 20K–300K 受控读取和约 100K–350K 的 1 小时创建，使固定 500 条样本对应的 Sub2API 全量面板命中率约为 85%。

**Architecture:** 正常请求继续经过现有整数费用守恒分配器，但改用独立哈希盐、规模锚点命中率和随规模增加的创建份额。截断请求进入独立的无状态超大分配器，先生成读取目标，再按费用可行性限制创建目标，且永不提交 Prompt Cache Tracker。同步和流式处理器只负责选择正常或超大分配路径。

**Tech Stack:** Go、`testing`、`httptest`、现有 Kiro-Go Prompt Cache Tracker、Anthropic usage JSON 合同。

---

## 文件结构

- 修改 `proxy/claude_usage_allocator.go`：正常目标曲线、独立稳定哈希、75% 创建分类、扩大校验范围、超大受控读写分配器。
- 修改 `proxy/claude_usage_allocator_test.go`：正常比例、分类分布、规模方向、费用守恒和超大分配单元测试。
- 修改 `proxy/handler.go`：新增超大 usage 准备函数，并在同步和流式成功终态按 `WasTruncated` 分流。
- 修改 `proxy/claude_usage_contract_test.go`：截断请求同步合同、字段完整性、费用守恒和不提交状态。
- 修改 `proxy/cache_tracker.go`：算法版本升级为 `native-high-cache-v4`。
- 修改 `proxy/cache_tracker_test.go`：锁定新算法版本下 TTL 稳定分布与幂等行为。

## Task 1：正常请求命中率与创建类别

**Files:**
- Modify: `proxy/claude_usage_allocator_test.go`
- Modify: `proxy/claude_usage_allocator.go`

- [ ] **Step 1：先写失败测试，锁定 92.8% 规模曲线和 70%–80% 创建率**

将旧的 50% 创建测试和“大输入读取更多”测试替换为：

```go
func TestClaudeUsageRequestClassIsStableAndSeventyFivePercentCreate(t *testing.T) {
	var taskKey, fingerprint [32]byte
	taskKey[0] = 7
	fingerprint[0] = 11
	want := claudeUsageRequestClassFor(taskKey, fingerprint)
	for i := 0; i < 100; i++ {
		if got := claudeUsageRequestClassFor(taskKey, fingerprint); got != want {
			t.Fatalf("同一请求类别不稳定：第 %d 次=%d，期望=%d", i, got, want)
		}
	}

	createCount := 0
	for i := 0; i < 10_000; i++ {
		fingerprint[0] = byte(i)
		fingerprint[1] = byte(i >> 8)
		if claudeUsageRequestClassFor(taskKey, fingerprint) == claudeUsageReadCreate {
			createCount++
		}
	}
	if createCount < 7_000 || createCount > 8_000 {
		t.Fatalf("创建类别应位于 70%%–80%%：%d/10000", createCount)
	}
}

func TestClaudeUsageTargetsLargeInputsReadLessAndCreateMore(t *testing.T) {
	small := claudeUsageFeatures{
		SizeFactor:             0.65,
		StableHitJitter:        0,
		StableCreationJitter:   0,
		CreateCache:            true,
	}
	large := small
	large.SizeFactor = 0.875

	smallTarget := claudeUsageTargetsForFeatures(small)
	largeTarget := claudeUsageTargetsForFeatures(large)
	if largeTarget.ReadShare >= smallTarget.ReadShare {
		t.Fatalf("大输入读取比例应下降：小=%.6f 大=%.6f", smallTarget.ReadShare, largeTarget.ReadShare)
	}
	smallFraction := smallTarget.CreateShare / (1 - smallTarget.ReadShare)
	largeFraction := largeTarget.CreateShare / (1 - largeTarget.ReadShare)
	if largeFraction <= smallFraction {
		t.Fatalf("大输入创建份额应提高：小=%.6f 大=%.6f", smallFraction, largeFraction)
	}
}

func TestClaudeUsageTargetAnchors(t *testing.T) {
	small := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		SizeFactor:      0.60,
		StableHitJitter: 0,
	})
	large := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		SizeFactor:      0.875,
		StableHitJitter: 0,
	})
	if math.Abs(small.ReadShare-0.95) > 1e-9 {
		t.Fatalf("小请求锚点错误：%.6f", small.ReadShare)
	}
	if math.Abs(large.ReadShare-0.90) > 1e-9 {
		t.Fatalf("大请求锚点错误：%.6f", large.ReadShare)
	}
}
```

同时删除或改写所有依赖旧语义的测试：

- 删除 `TestClaudeUsageTargetsGrowthRaisesCreationShare`，创建份额不再由 `GrowthRatio` 控制。
- 将 `TestClaudeUsageTargetsKeepCreationSmall` 改为校验 `creation_fraction` 位于 26%–75%。
- 将所有 `StableJitter` 字段替换为 `StableHitJitter` 和 `StableCreationJitter`。
- 将 `representativeClaudeUsageFeatures` 的最后一个字段拆成两个独立微扰。

- [ ] **Step 2：运行测试并确认旧模型失败**

Run:

```powershell
go test ./proxy -run 'TestClaudeUsage(RequestClassIsStableAndSeventyFivePercentCreate|TargetsLargeInputsReadLessAndCreateMore|TargetAnchors)$' -count=1
```

Expected: FAIL，旧代码仍为 50% 创建率、95%–96% 命中率，并让大输入读取更多。

- [ ] **Step 3：实现独立哈希盐、规模压力和锚点插值**

在 `proxy/claude_usage_allocator.go` 中将常量和特征改为：

```go
const (
	maxClaudeUsageCandidates = 64
	minClaudeReadHitRate     = 0.85
	maxClaudeReadHitRate     = 0.95
)

type claudeUsageFeatures struct {
	Phase                   promptCachePhase
	ReuseRatio              float64
	GrowthRatio             float64
	AgeRatio                float64
	RoundFactor             float64
	SizeFactor              float64
		ToolRatio               float64
	StableHitJitter         float64
	StableCreationJitter    float64
	CreateCache             bool
}
```

`buildClaudeUsageFeatures` 必须使用独立盐填充新字段：

```go
StableHitJitter: stableClaudeUsageJitterFor(
	snapshot.TaskKey,
	snapshot.RequestFingerprint,
	"hit",
),
StableCreationJitter: stableClaudeUsageJitterFor(
	snapshot.TaskKey,
	snapshot.RequestFingerprint,
	"creation",
),
```

新增带盐稳定哈希：

```go
func stableClaudeUsageUnit(taskKey, requestFingerprint [32]byte, salt string) float64 {
	hasher := sha256.New()
	_, _ = hasher.Write(taskKey[:])
	_, _ = hasher.Write(requestFingerprint[:])
	_, _ = hasher.Write([]byte(promptCacheAlgorithmVersion))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(salt))
	sum := hasher.Sum(nil)
	return float64(binary.BigEndian.Uint64(sum[:8])) / float64(^uint64(0))
}

func stableClaudeUsageJitterFor(taskKey, requestFingerprint [32]byte, salt string) float64 {
	return 2*stableClaudeUsageUnit(taskKey, requestFingerprint, salt) - 1
}
```

创建类别使用独立概率与抽样盐：

```go
func claudeUsageRequestClassFor(taskKey, requestFingerprint [32]byte) claudeUsageRequestClass {
	probability := clampFloat(
		0.75+0.02*stableClaudeUsageJitterFor(taskKey, requestFingerprint, "create-probability"),
		0.70,
		0.80,
	)
	if stableClaudeUsageUnit(taskKey, requestFingerprint, "create-draw") < probability {
		return claudeUsageReadCreate
	}
	return claudeUsageReadOnly
}
```

实现 `smoothstep` 和锚点插值：

```go
func smoothstep01(value float64) float64 {
	t := clampFloat(value, 0, 1)
	return t * t * (3 - 2*t)
}

func smoothstep(edge0, edge1, value float64) float64 {
	if edge1 <= edge0 {
		return 0
	}
	return smoothstep01((value - edge0) / (edge1 - edge0))
}

func claudeBaseHitRate(sizePressure float64) float64 {
	anchors := [...]struct{ pressure, hit float64 }{
		{0.00, 0.950},
		{0.40, 0.950},
		{0.70, 0.950},
		{0.88, 0.948},
		{1.00, 0.900},
	}
	p := clampFloat(sizePressure, 0, 1)
	for i := 1; i < len(anchors); i++ {
		if p > anchors[i].pressure {
			continue
		}
		left, right := anchors[i-1], anchors[i]
		t := smoothstep01((p - left.pressure) / (right.pressure - left.pressure))
		return left.hit + (right.hit-left.hit)*t
	}
	return anchors[len(anchors)-1].hit
}
```

目标函数改为：

```go
func claudeUsageTargetsForFeatures(features claudeUsageFeatures) claudeUsageTargets {
	sizePressure := smoothstep(0.60, 0.875, clampFloat(features.SizeFactor, 0, 1))
	targetHitRate := clampFloat(
		claudeBaseHitRate(sizePressure)+0.010*clampFloat(features.StableHitJitter, -1, 1),
		minClaudeReadHitRate,
		maxClaudeReadHitRate,
	)
	nonReadShare := 1 - targetHitRate
	createShare := 0.0
	if features.CreateCache {
		creationFraction := clampFloat(
			0.32+0.38*math.Pow(sizePressure, 1.2)+0.035*clampFloat(features.StableCreationJitter, -1, 1),
			0.26,
			0.75,
		)
		createShare = nonReadShare * creationFraction
	}
	return claudeUsageTargets{
		InputShare:  nonReadShare - createShare,
		ReadShare:   targetHitRate,
		CreateShare: createShare,
	}
}
```

- [ ] **Step 4：运行正常目标测试**

Run:

```powershell
go test ./proxy -run 'TestClaudeUsage(RequestClassIsStableAndSeventyFivePercentCreate|TargetsLargeInputsReadLessAndCreateMore|TargetAnchors)$' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交正常比例模型**

```powershell
git add proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go
git commit -m "实现：调整Kiro-Go正常缓存分布"
```

## Task 2：扩大整数分配合法范围

**Files:**
- Modify: `proxy/claude_usage_allocator_test.go`
- Modify: `proxy/claude_usage_allocator.go`

- [ ] **Step 1：写失败测试，覆盖 85%–95% 读取和最多 15% 普通输入**

```go
func TestAllocateClaudeUsageSupportsExpandedInputShare(t *testing.T) {
	target := claudeUsageTargets{
		InputShare:  0.15,
		ReadShare:   0.85,
		CreateShare: 0,
	}
	usage, ok := allocateClaudeUsage(100_000, 100, promptCacheTTL1h, target)
	if !ok {
		t.Fatalf("85%% 只读目标应存在合法整数解")
	}
	assertClaudeUsageConserved(t, 100_000, usage)
	assertUsageHitRate(t, usage)
}

func TestAllocateClaudeUsageSupportsLargeCreationFraction(t *testing.T) {
	target := claudeUsageTargets{
		InputShare:  0.025,
		ReadShare:   0.90,
		CreateShare: 0.075,
	}
	usage, ok := allocateClaudeUsage(200_000, 100, promptCacheTTL1h, target)
	if !ok {
		t.Fatalf("大请求 75%% 非读取创建份额应存在合法整数解")
	}
	assertClaudeUsageConserved(t, 200_000, usage)
}
```

- [ ] **Step 2：运行测试并确认校验器拒绝旧范围**

Run:

```powershell
go test ./proxy -run 'TestAllocateClaudeUsage(SupportsExpandedInputShare|SupportsLargeCreationFraction)$' -count=1
```

Expected: FAIL，旧校验仍限制普通输入为 1%–5%。

- [ ] **Step 3：扩展目标和结果校验范围**

将 `validClaudeUsageTarget` 和 `validAllocatedClaudeUsage` 中的普通输入上限统一改为：

```go
if target.InputShare < 0.01 || target.InputShare > 0.15 {
	return false
}
```

以及：

```go
if inputShare < 0.01 || inputShare > 0.15 ||
	readShare < minClaudeReadHitRate || readShare > maxClaudeReadHitRate {
	return false
}
```

同步更新测试辅助断言：

```go
func assertTargetShares(t *testing.T, target claudeUsageTargets) {
	t.Helper()
	sum := target.InputShare + target.ReadShare + target.CreateShare
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("目标比例之和应为 1，得到 %.12f", sum)
	}
	if target.InputShare < 0.01 || target.InputShare > 0.15 {
		t.Fatalf("普通输入目标越界：%.6f", target.InputShare)
	}
	assertTargetHitRate(t, target.ReadShare)
}
```

`assertUsageRatios` 中的普通输入范围同样改为 1%–15%。保留 64 个候选上限、只读降级、单 TTL 和严格费用守恒。

- [ ] **Step 4：运行分配器测试**

Run:

```powershell
go test ./proxy -run 'TestAllocateClaudeUsage|TestClaudeUsageTargets' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交整数分配调整**

```powershell
git add proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go
git commit -m "实现：扩展Kiro-Go缓存整数分配范围"
```

## Task 3：超大截断请求受控读写分配器

**Files:**
- Modify: `proxy/claude_usage_allocator_test.go`
- Modify: `proxy/claude_usage_allocator.go`

- [ ] **Step 1：写失败测试，锁定 20K–300K 读取、100K–350K 创建和费用边界**

```go
func TestAllocateOversizeClaudeUsageIsStableAndConserved(t *testing.T) {
	var taskKey, fingerprint [32]byte
	taskKey[0] = 17
	fingerprint[0] = 29
	first, ok := allocateOversizeClaudeUsage(779_460, 321, taskKey, fingerprint)
	if !ok {
		t.Fatalf("超大请求分配失败")
	}
	second, ok := allocateOversizeClaudeUsage(779_460, 321, taskKey, fingerprint)
	if !ok || second != first {
		t.Fatalf("超大请求分配必须稳定：first=%+v second=%+v", first, second)
	}
	if first.CacheReadInputTokens < 20_000 || first.CacheReadInputTokens > 300_000 {
		t.Fatalf("超大读取越界：%+v", first)
	}
	if first.CacheCreationInputTokens <= 0 || first.CacheCreationInputTokens > 350_000 {
		t.Fatalf("超大创建越界：%+v", first)
	}
	if first.CacheCreation.Ephemeral5mInputTokens != 0 ||
		first.CacheCreation.Ephemeral1hInputTokens != first.CacheCreationInputTokens {
		t.Fatalf("超大请求必须只创建 1h 缓存：%+v", first.CacheCreation)
	}
	assertClaudeUsageConserved(t, 779_460, first)
}

func TestAllocateOversizeClaudeUsagePrioritizesFeasibilityForSmallBudget(t *testing.T) {
	var taskKey, fingerprint [32]byte
	usage, ok := allocateOversizeClaudeUsage(101_523, 10, taskKey, fingerprint)
	if !ok {
		t.Fatalf("最小固定样本应存在可行分配")
	}
	reserve := maxInt(1_000, int(math.Ceil(0.01*101_523)))
	if usage.InputTokens < reserve {
		t.Fatalf("普通输入保留不足：usage=%+v reserve=%d", usage, reserve)
	}
	if usage.CacheCreationInputTokens >= 100_000 {
		t.Fatalf("费用不足时创建目标必须下调：%+v", usage)
	}
	assertClaudeUsageConserved(t, 101_523, usage)
}

func TestAllocateOversizeClaudeUsageRejectsInvalidBudget(t *testing.T) {
	var key [32]byte
	for _, rawInput := range []int{-1, 0, 1, 10_000, math.MaxInt} {
		if usage, ok := allocateOversizeClaudeUsage(rawInput, 10, key, key); ok {
			t.Fatalf("输入 %d 应回退：%+v", rawInput, usage)
		}
	}
}
```

- [ ] **Step 2：运行测试并确认函数尚不存在**

Run:

```powershell
go test ./proxy -run 'TestAllocateOversizeClaudeUsage' -count=1
```

Expected: FAIL，`allocateOversizeClaudeUsage` 未定义。

- [ ] **Step 3：实现超大目标和整数费用分配**

在 `proxy/claude_usage_allocator.go` 新增：

```go
func oversizeClaudePressure(rawInputTokens int) float64 {
	return smoothstep(180_000, 680_000, float64(rawInputTokens))
}

func roundToMultiple(value float64, multiple int) int {
	if multiple <= 1 {
		return int(math.Round(value))
	}
	return int(math.Round(value/float64(multiple))) * multiple
}

func allocateOversizeClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	taskKey [32]byte,
	requestFingerprint [32]byte,
) (ClaudeUsage, bool) {
	if rawInputTokens <= 0 || rawOutputTokens < 0 || rawInputTokens > maxIntValue()/20 {
		return ClaudeUsage{}, false
	}

	pressure := oversizeClaudePressure(rawInputTokens)
	readTarget := roundToMultiple(clampFloat(
		20_000+280_000*pressure+
			30_000*stableClaudeUsageJitterFor(taskKey, requestFingerprint, "oversize-read"),
		20_000,
		300_000,
	), 10)
	creationTarget := int(math.Round(clampFloat(
		100_000+250_000*pressure+
			25_000*stableClaudeUsageJitterFor(taskKey, requestFingerprint, "oversize-creation"),
		100_000,
		350_000,
	)))
	reserve := maxInt(1_000, int(math.Ceil(0.01*float64(rawInputTokens))))
	maxCreation := (20*rawInputTokens - 2*readTarget - 20*reserve) / 40
	if maxCreation <= 0 {
		return ClaudeUsage{}, false
	}
	creationTokens := minInt(creationTarget, maxCreation)
	remaining := 20*rawInputTokens - 2*readTarget - 40*creationTokens
	if remaining < 0 || remaining%20 != 0 {
		return ClaudeUsage{}, false
	}
	inputTokens := remaining / 20
	if inputTokens < reserve {
		return ClaudeUsage{}, false
	}

	usage := ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             rawOutputTokens,
		CacheReadInputTokens:     readTarget,
		CacheCreationInputTokens: creationTokens,
	}
	usage.CacheCreation.Ephemeral1hInputTokens = creationTokens
	if !claudeUsageCostConserved(rawInputTokens, usage) {
		return ClaudeUsage{}, false
	}
	return usage, true
}
```

- [ ] **Step 4：运行超大分配器测试**

Run:

```powershell
go test ./proxy -run 'TestAllocateOversizeClaudeUsage' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交超大分配器**

```powershell
git add proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go
git commit -m "实现：增加Kiro-Go超大请求受控缓存分配"
```

## Task 4：同步和流式处理器接入超大路径

**Files:**
- Modify: `proxy/claude_usage_contract_test.go`
- Modify: `proxy/handler.go`

- [ ] **Step 1：将旧截断回退测试改成失败的受控读写合同测试**

```go
func TestClaudeTruncatedPayloadReturnsControlledCacheUsageWithoutCommit(t *testing.T) {
	handler := setupClaudeContractHandler(t, 1)
	upstream := newClaudeUsageUpstream(t, "截断响应", 500_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	oversized := strings.Repeat("超大上下文", maxPayloadBytes/len("超大上下文")+4096)
	recorder := performClaudeContractRequest(t, handler, false, oversized)
	if recorder.Code != http.StatusOK {
		t.Fatalf("截断请求失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	usage, rawUsage := decodeClaudeNonStreamUsage(t, recorder.Body.Bytes())
	assertCompleteClaudeUsageFields(t, rawUsage)
	if usage.CacheReadInputTokens < 20_000 || usage.CacheReadInputTokens > 300_000 {
		t.Fatalf("截断读取越界：%+v", usage)
	}
	if usage.CacheCreationInputTokens <= 0 || usage.CacheCreationInputTokens > 350_000 {
		t.Fatalf("截断创建越界：%+v", usage)
	}
	if usage.CacheCreation.Ephemeral1hInputTokens != usage.CacheCreationInputTokens ||
		usage.CacheCreation.Ephemeral5mInputTokens != 0 {
		t.Fatalf("截断请求必须只创建 1h 缓存：%+v", usage)
	}
	assertClaudeUsageConserved(t, 500_000, usage)
	if handler.promptCache.taskCount() != 0 {
		t.Fatalf("截断请求不得提交未实际发送的前缀")
	}
}
```

增加流式合同测试：

```go
func TestClaudeTruncatedPayloadStreamMatchesNonStreamWithoutCommit(t *testing.T) {
	nonStreamHandler := setupClaudeContractHandler(t, 1)
	streamHandler := &Handler{
		pool:        nonStreamHandler.pool,
		promptCache: newPromptCacheTracker(time.Hour),
	}
	upstream := newClaudeUsageUpstream(t, "截断一致响应", 500_000)
	defer upstream.Close()
	defer swapKiroEndpointsForTest(t, upstream)()

	oversized := strings.Repeat("超大上下文", maxPayloadBytes/len("超大上下文")+4096)
	nonStream := performClaudeContractRequest(t, nonStreamHandler, false, oversized)
	want, _ := decodeClaudeNonStreamUsage(t, nonStream.Body.Bytes())

	stream := performClaudeContractRequest(t, streamHandler, true, oversized)
	if stream.Code != http.StatusOK {
		t.Fatalf("流式截断请求失败：status=%d body=%s", stream.Code, stream.Body.String())
	}
	got, rawFinal := decodeClaudeFinalSSEUsage(t, stream.Body.String())
	assertCompleteClaudeUsageFields(t, rawFinal)
	if got != want {
		t.Fatalf("截断同步与流式 usage 不一致：sync=%+v stream=%+v", want, got)
	}
	if nonStreamHandler.promptCache.taskCount() != 0 || streamHandler.promptCache.taskCount() != 0 {
		t.Fatalf("截断请求不得提交 Tracker 状态")
	}
}
```

- [ ] **Step 2：运行合同测试并确认旧处理器仍返回零缓存**

Run:

```powershell
go test ./proxy -run 'TestClaudeTruncatedPayload' -count=1
```

Expected: FAIL，旧处理器仍绕过分配器。

- [ ] **Step 3：新增超大准备函数并修改同步、流式分流**

在 `proxy/handler.go` 新增：

```go
func prepareOversizeClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	analysis claudeRequestAnalysis,
) preparedClaudeUsage {
	rawUsage := ClaudeUsage{
		InputTokens:  maxInt(rawInputTokens, 0),
		OutputTokens: maxInt(rawOutputTokens, 0),
	}
	usage, ok := allocateOversizeClaudeUsage(
		rawInputTokens,
		rawOutputTokens,
		analysis.TaskKey,
		analysis.RequestFingerprint,
	)
	if !ok {
		return preparedClaudeUsage{Usage: rawUsage}
	}
	// OK 保持 false，超大截断请求不能提交 Tracker。
	return preparedClaudeUsage{Usage: usage}
}
```

同步和流式成功终态都改为：

```go
if finalUsageComplete || realInputTokens > 0 {
	if payload != nil && payload.WasTruncated {
		prepared = prepareOversizeClaudeUsage(inputTokens, outputTokens, analysis)
	} else {
		prepared = prepareFinalClaudeUsage(
			inputTokens,
			outputTokens,
			analysis,
			snapshot,
			time.Now(),
		)
	}
}
```

保持现有 `if prepared.OK { h.promptCache.Commit(...) }` 不变。正常路径提交，超大路径和原始回退不提交。

- [ ] **Step 4：运行同步、流式和失败状态合同测试**

Run:

```powershell
go test ./proxy -run 'TestClaude(TruncatedPayload|StreamFinalDeltaMatchesNonStreamUsage|FailedUpstreamDoesNotCommitCacheState|MissingFinalUsageDoesNotCommitCacheState)' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交处理器接线**

```powershell
git add proxy/handler.go proxy/claude_usage_contract_test.go
git commit -m "实现：接入Kiro-Go超大请求缓存路径"
```

## Task 5：算法版本、分布门禁与全量验证

**Files:**
- Modify: `proxy/cache_tracker.go`
- Modify: `proxy/cache_tracker_test.go`
- Modify: `proxy/claude_usage_allocator_test.go`

- [ ] **Step 1：写失败测试，锁定 V4 版本和大样本分布**

```go
func TestPromptCacheAlgorithmVersionIsV4(t *testing.T) {
	if promptCacheAlgorithmVersion != "native-high-cache-v4" {
		t.Fatalf("算法版本错误：%s", promptCacheAlgorithmVersion)
	}
}

func TestClaudeUsageV4DistributionGates(t *testing.T) {
	var createCount int
	var readTotal, displayTotal int64
	for i := 0; i < 10_000; i++ {
		var taskKey, fingerprint [32]byte
		taskKey[0] = byte(i)
		taskKey[1] = byte(i >> 8)
		fingerprint[0] = byte(i * 31)
		fingerprint[1] = byte(i >> 4)
		rawInput := representativeV4RawInput(i, 10_000)
		features := buildClaudeUsageFeatures(
			promptCacheSnapshot{TaskKey: taskKey, RequestFingerprint: fingerprint},
			claudeRequestAnalysis{},
			rawInput,
		)
		target := claudeUsageTargetsForFeatures(features)
		if features.CreateCache {
			createCount++
		}
		usage, ok := allocateClaudeUsage(rawInput, 10, ttlForTask(taskKey), target)
		if !ok {
			t.Fatalf("分布样本分配失败：index=%d raw=%d", i, rawInput)
		}
		readTotal += int64(usage.CacheReadInputTokens)
		displayTotal += int64(
			usage.InputTokens +
				usage.CacheReadInputTokens +
				usage.CacheCreationInputTokens,
		)
	}
	createRate := float64(createCount) / 10_000
	if createRate < 0.70 || createRate > 0.80 {
		t.Fatalf("创建请求率越界：%.6f", createRate)
	}
	hitRate := float64(readTotal) / float64(displayTotal)
	if hitRate < 0.923 || hitRate > 0.932 {
		t.Fatalf("正常命中率越界：%.6f", hitRate)
	}
}

func representativeV4RawInput(index, total int) int {
	anchors := [...]struct {
		quantile float64
		tokens   int
	}{
		{0.00, 1_296},
		{0.25, 6_510},
		{0.50, 44_970},
		{0.75, 92_012},
		{0.90, 138_616},
		{0.95, 147_761},
		{1.00, 182_716},
	}
	q := float64(index) / float64(total-1)
	for i := 1; i < len(anchors); i++ {
		if q > anchors[i].quantile {
			continue
		}
		left, right := anchors[i-1], anchors[i]
		fraction := (q - left.quantile) / (right.quantile - left.quantile)
		return int(math.Round(float64(left.tokens) + float64(right.tokens-left.tokens)*fraction))
	}
	return anchors[len(anchors)-1].tokens
}
```

- [ ] **Step 2：运行版本和分布测试，确认版本仍为 V3**

Run:

```powershell
go test ./proxy -run 'TestPromptCacheAlgorithmVersionIsV4|TestClaudeUsageV4DistributionGates' -count=1
```

Expected: FAIL，算法版本仍为 `native-high-cache-v3`。

- [ ] **Step 3：升级算法版本**

在 `proxy/cache_tracker.go` 修改：

```go
const promptCacheAlgorithmVersion = "native-high-cache-v4"
```

`representativeV4RawInput` 的锚点来自固定 500 条正常请求的最小值、P25、P50、P75、P90、P95 和最大值，不允许在实现阶段修改已确认的命中率锚点、读取上限、创建上限或费用方程来迎合测试。

- [ ] **Step 4：运行完整验证**

Run:

```powershell
gofmt -w proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go proxy/handler.go proxy/claude_usage_contract_test.go proxy/cache_tracker.go proxy/cache_tracker_test.go
go test ./proxy -count=1
go test ./... -count=1
go test -race ./proxy -count=1
```

Expected:

- 所有命令退出码为 0。
- 无 `FAIL`。
- Race 测试无 `DATA RACE`。

- [ ] **Step 5：执行固定 500 条只读回放核对**

使用固定快照：

```text
group_id = 84
usage_logs.id <= 3036603
最近 500 条
```

只读取 `input_tokens`、缓存读取/创建 Token 和对应费用字段，恢复原始等价输入后使用 V4 公式回放。不得输出用户正文、API Key、OAuth、Cookie、完整邮箱或 IP。

Expected:

```text
正常请求加权命中率：92.3%–93.2%
Sub2API 全量面板命中率：84.5%–85.5%
正常创建请求率：70%–80%
超大读取：20K–300K
超大创建：费用可行范围内，P95 不高于 350K
```

- [ ] **Step 6：提交版本和验证门禁**

```powershell
git add proxy/cache_tracker.go proxy/cache_tracker_test.go proxy/claude_usage_allocator_test.go
git commit -m "测试：锁定Kiro-Go缓存分布V4门禁"
```

## Task 6：最终审查和工作树确认

**Files:**
- Verify only

- [ ] **Step 1：检查仅修改 Kiro-Go 目标文件**

Run:

```powershell
git status --short
git diff --check HEAD~5..HEAD
git log -8 --oneline --decorate
```

Expected:

- 工作树干净。
- 没有 Sub2API 文件变化。
- 没有空白错误。
- 提交历史包含设计、正常模型、整数范围、超大路径、处理器接线和分布门禁。

- [ ] **Step 2：记录验证结果，不执行部署**

本计划只完成本地实现和验证。构建镜像、推送 GitHub、重建服务器服务和生产流量观察必须作为后续明确授权的发布步骤执行。
