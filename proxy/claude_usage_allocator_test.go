package proxy

import (
	"encoding/json"
	"math"
	"testing"
)

func TestClaudeUsageTargetsAlwaysReadAndSupportBothClasses(t *testing.T) {
	var readOnly, readCreate int
	const samples = 10_000
	for i := 0; i < samples; i++ {
		features := representativeClaudeUsageFeatures(i)
		features.CreateCache = i%2 == 0
		target := claudeUsageTargetsForFeatures(features)
		assertTargetHitRate(t, target.ReadShare)
		if target.CreateShare == 0 {
			readOnly++
		} else {
			readCreate++
		}
	}
	if readOnly != samples/2 || readCreate != samples/2 {
		t.Fatalf("请求类别分布错误：只读=%d 读写=%d", readOnly, readCreate)
	}
}

func TestClaudeUsageRequestClassIsStableAndSeventyFivePercentCreate(t *testing.T) {
	var taskKey, requestFingerprint [32]byte
	taskKey[0] = 7
	requestFingerprint[0] = 11
	want := claudeUsageRequestClassFor(taskKey, requestFingerprint)
	for i := 0; i < 100; i++ {
		if got := claudeUsageRequestClassFor(taskKey, requestFingerprint); got != want {
			t.Fatalf("同一请求类别不稳定：第 %d 次=%d，期望=%d", i, got, want)
		}
	}

	var createCount int
	for i := 0; i < 10_000; i++ {
		requestFingerprint[0] = byte(i)
		requestFingerprint[1] = byte(i >> 8)
		if claudeUsageRequestClassFor(taskKey, requestFingerprint) == claudeUsageReadCreate {
			createCount++
		}
	}
	if createCount < 7_000 || createCount > 8_000 {
		t.Fatalf("创建类别应位于 70%%–80%%：%d/10000", createCount)
	}
}

func assertTargetHitRate(t *testing.T, hitRate float64) {
	t.Helper()
	if hitRate < minClaudeReadHitRate || hitRate > maxClaudeReadHitRate {
		t.Fatalf("真实读取命中率越界：%.6f", hitRate)
	}
}

func assertUsageHitRate(t *testing.T, usage ClaudeUsage) {
	t.Helper()
	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total <= 0 {
		t.Fatalf("输入侧总 Token 必须为正")
	}
	assertTargetHitRate(t, float64(usage.CacheReadInputTokens)/float64(total))
}

func TestClaudeUsageTargetsFirstRoundsAlwaysRead(t *testing.T) {
	var reads int
	const samples = 5_000
	for i := 0; i < samples; i++ {
		features := representativeClaudeUsageFeatures(i)
		features.Phase = promptCachePhaseFirst
		target := claudeUsageTargetsForFeatures(features)
		assertTargetHitRate(t, target.ReadShare)
		reads++
	}

	rate := float64(reads) / samples
	if rate != 1 {
		t.Fatalf("正常首轮读取请求占比应为 100%%，得到 %.2f%%", rate*100)
	}
}

func TestClaudeUsageTargetsSupportsReadOnlyRequests(t *testing.T) {
	target := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:                promptCachePhaseFirst,
		GrowthRatio:          1,
		SizeFactor:           0.7,
		ToolRatio:            0.1,
		StableHitJitter:      -0.9,
		StableCreationJitter: -0.5,
		CreateCache:          false,
	})

	if target.ReadShare <= 0 || target.CreateShare != 0 {
		t.Fatalf("只读取请求比例错误：%+v", target)
	}
	assertTargetHitRate(t, target.ReadShare)
	assertTargetShares(t, target)
}

func TestClaudeUsageTargetsReuseDoesNotBreakHitRate(t *testing.T) {
	base := claudeUsageFeatures{
		Phase:                promptCachePhaseContinue,
		GrowthRatio:          0.04,
		AgeRatio:             0.2,
		RoundFactor:          0.3,
		SizeFactor:           0.7,
		ToolRatio:            0.1,
		StableHitJitter:      0.1,
		StableCreationJitter: 0.1,
	}
	lowReuse := base
	lowReuse.ReuseRatio = 0.4
	highReuse := base
	highReuse.ReuseRatio = 0.9

	low := claudeUsageTargetsForFeatures(lowReuse)
	high := claudeUsageTargetsForFeatures(highReuse)

	assertTargetHitRate(t, low.ReadShare)
	assertTargetHitRate(t, high.ReadShare)
}

func TestClaudeUsageTargetsJitterAffectsResult(t *testing.T) {
	baseFeatures := claudeUsageFeatures{
		Phase:                promptCachePhaseContinue,
		ReuseRatio:           0.75,
		GrowthRatio:          0.03,
		SizeFactor:           0.875,
		StableHitJitter:      -0.5,
		StableCreationJitter: 0,
		CreateCache:          true,
	}
	base := claudeUsageTargetsForFeatures(baseFeatures)

	jitter := baseFeatures
	jitter.StableHitJitter = 0.5
	jitter.StableCreationJitter = 0.8
	got := claudeUsageTargetsForFeatures(jitter)
	if got == base {
		t.Fatalf("稳定微扰变化后目标比例不应完全相同")
	}
	assertTargetShares(t, got)
}

func TestClaudeUsageTargetsStayWithinHitRateBounds(t *testing.T) {
	for i := 0; i < 5_000; i++ {
		features := representativeClaudeUsageFeatures(i)
		target := claudeUsageTargetsForFeatures(features)
		assertTargetHitRate(t, target.ReadShare)
	}
}

func TestClaudeUsageTargetsCreationFractionStaysWithinBounds(t *testing.T) {
	const samples = 5_000
	for i := 0; i < samples; i++ {
		features := representativeClaudeUsageFeatures(i)
		features.Phase = promptCachePhaseContinue
		features.CreateCache = true
		target := claudeUsageTargetsForFeatures(features)
		assertTargetHitRate(t, target.ReadShare)
		nonReadShare := 1 - target.ReadShare
		creationFraction := target.CreateShare / nonReadShare
		if creationFraction < 0.26 || creationFraction > 0.75 {
			t.Fatalf("创建份额应位于 26%%–75%%：%.6f", creationFraction)
		}
	}
}

func TestClaudeUsageTargetsLargeInputsReadLessAndCreateMore(t *testing.T) {
	base := claudeUsageFeatures{
		Phase:                promptCachePhaseContinue,
		ReuseRatio:           0.6,
		GrowthRatio:          0.2,
		CreateCache:          true,
		StableHitJitter:      0,
		StableCreationJitter: 0,
	}
	small := base
	small.SizeFactor = 0.65
	large := base
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

func TestAllocateClaudeUsageConservesFiveMinuteCost(t *testing.T) {
	target := continuationTarget()
	usage, ok := allocateClaudeUsage(10_000, 777, promptCacheTTL5m, target)
	if !ok {
		t.Fatalf("5 分钟目标应存在合法整数解")
	}
	assertClaudeUsageConserved(t, 10_000, usage)
}

func TestAllocateClaudeUsageConservesOneHourCost(t *testing.T) {
	target := continuationTarget()
	usage, ok := allocateClaudeUsage(10_000, 777, promptCacheTTL1h, target)
	if !ok {
		t.Fatalf("1 小时目标应存在合法整数解")
	}
	assertClaudeUsageConserved(t, 10_000, usage)
}

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

func TestAllocateClaudeUsageKeepsActualHitRateBounded(t *testing.T) {
	for _, ttl := range []promptCacheTTL{promptCacheTTL5m, promptCacheTTL1h} {
		for rawInput := 100; rawInput <= 100_000; rawInput += 997 {
			target := claudeUsageTargetsForFeatures(claudeUsageFeatures{
				Phase:                promptCachePhaseContinue,
				ReuseRatio:           0.95,
				GrowthRatio:          0.0001,
				RoundFactor:          1,
				SizeFactor:           math.Log2(1+float64(rawInput)) / 20,
				StableHitJitter:      -1,
				StableCreationJitter: -1,
				CreateCache:          true,
			})
			usage, ok := allocateClaudeUsage(rawInput, 10, ttl, target)
			if !ok {
				t.Fatalf("代表性输入必须存在整数解：ttl=%v input=%d", ttl, rawInput)
			}
			assertUsageHitRate(t, usage)
		}
	}
}

func TestAllocateClaudeUsagePreservesFirstAndRebuildHitRate(t *testing.T) {
	for _, phase := range []promptCachePhase{
		promptCachePhaseFirst,
		promptCachePhaseRebuild,
	} {
		const samples = 5_000
		for i := 0; i < samples; i++ {
			features := representativeClaudeUsageFeatures(i)
			features.Phase = phase
			features.CreateCache = i%2 == 0
			target := claudeUsageTargetsForFeatures(features)
			ttl := promptCacheTTL5m
			if i%5 != 0 {
				ttl = promptCacheTTL1h
			}
			usage, ok := allocateClaudeUsage(10_000+i%1_000, 10, ttl, target)
			if !ok {
				t.Fatalf("门禁样本必须存在整数解：phase=%v index=%d", phase, i)
			}
			assertUsageHitRate(t, usage)
			if usage.CacheReadInputTokens <= 0 {
				t.Fatalf("正常请求必须读取缓存：phase=%v index=%d", phase, i)
			}
			if features.CreateCache && usage.CacheCreationInputTokens <= 0 {
				t.Fatalf("读写请求必须创建少量缓存：phase=%v index=%d", phase, i)
			}
			if !features.CreateCache && usage.CacheCreationInputTokens != 0 {
				t.Fatalf("只读请求不得创建缓存：phase=%v index=%d", phase, i)
			}
		}
	}
}

func TestAllocateClaudeUsageUsesSingleTTL(t *testing.T) {
	target := continuationTarget()

	fiveMinute, ok := allocateClaudeUsage(10_000, 100, promptCacheTTL5m, target)
	if !ok {
		t.Fatalf("5 分钟分配失败")
	}
	if fiveMinute.CacheCreation.Ephemeral5mInputTokens == 0 ||
		fiveMinute.CacheCreation.Ephemeral1hInputTokens != 0 {
		t.Fatalf("5 分钟任务必须只使用 5m 创建：%+v", fiveMinute.CacheCreation)
	}

	oneHour, ok := allocateClaudeUsage(10_000, 100, promptCacheTTL1h, target)
	if !ok {
		t.Fatalf("1 小时分配失败")
	}
	if oneHour.CacheCreation.Ephemeral1hInputTokens == 0 ||
		oneHour.CacheCreation.Ephemeral5mInputTokens != 0 {
		t.Fatalf("1 小时任务必须只使用 1h 创建：%+v", oneHour.CacheCreation)
	}
}

func TestAllocateClaudeUsageKeepsOutputTokens(t *testing.T) {
	usage, ok := allocateClaudeUsage(10_000, 1234, promptCacheTTL1h, continuationTarget())
	if !ok {
		t.Fatalf("分配失败")
	}
	if usage.OutputTokens != 1234 {
		t.Fatalf("输出 token 不得变化：得到 %d", usage.OutputTokens)
	}
}

func TestAllocateClaudeUsageSupportsReadOnlyAndReadCreate(t *testing.T) {
	readOnlyTarget := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:       promptCachePhaseFirst,
		SizeFactor:  0.8,
		ToolRatio:   0.1,
		CreateCache: false,
	})
	readOnly, ok := allocateClaudeUsage(20_000, 100, promptCacheTTL1h, readOnlyTarget)
	if !ok {
		t.Fatalf("只读取分配失败")
	}
	if readOnly.CacheReadInputTokens <= 0 || readOnly.CacheCreationInputTokens != 0 {
		t.Fatalf("只读取 usage 错误：%+v", readOnly)
	}
	assertUsageRatios(t, readOnly, false)

	readCreateTarget := readOnlyTarget
	readCreateTarget.CreateShare = 0.02
	readCreateTarget.InputShare = 1 - readCreateTarget.ReadShare - readCreateTarget.CreateShare
	readCreate, ok := allocateClaudeUsage(20_000, 100, promptCacheTTL1h, readCreateTarget)
	if !ok {
		t.Fatalf("读取并创建分配失败")
	}
	if readCreate.CacheReadInputTokens <= 0 || readCreate.CacheCreationInputTokens <= 0 {
		t.Fatalf("读取并创建 usage 错误：%+v", readCreate)
	}
	assertUsageRatios(t, readCreate, true)
}

func TestAllocateClaudeUsageFallsBackForTinyOrImpossibleInputs(t *testing.T) {
	for _, rawInput := range []int{-1, 0, 1} {
		if usage, ok := allocateClaudeUsage(rawInput, 10, promptCacheTTL1h, continuationTarget()); ok {
			t.Fatalf("输入 %d 应回退，得到 %+v", rawInput, usage)
		}
	}

	impossible := claudeUsageTargets{InputShare: 0.5, ReadShare: 0.5}
	if usage, ok := allocateClaudeUsage(10_000, 10, promptCacheTTL1h, impossible); ok {
		t.Fatalf("越界目标应回退，得到 %+v", usage)
	}
}

func TestAllocateClaudeUsageFallsBackToReadOnlyWhenCreationHasNoIntegerSolution(t *testing.T) {
	target := continuationTarget()
	var found bool
	for rawInput := 2; rawInput <= 20_000; rawInput++ {
		_, creationOK, _ := allocateClaudeUsageWithCandidateCount(
			rawInput,
			10,
			promptCacheTTL1h,
			target,
		)
		if creationOK {
			continue
		}

		usage, ok := allocateClaudeUsage(rawInput, 10, promptCacheTTL1h, target)
		if !ok {
			continue
		}
		found = true
		if usage.CacheReadInputTokens <= 0 || usage.CacheCreationInputTokens != 0 {
			t.Fatalf("降级结果必须只读取缓存：input=%d usage=%+v", rawInput, usage)
		}
		assertUsageHitRate(t, usage)
		break
	}
	if !found {
		t.Fatalf("应至少找到一个创建整数解失败但只读降级成功的输入")
	}
}

func TestAllocateClaudeUsageRejectsOverflow(t *testing.T) {
	if usage, ok := allocateClaudeUsage(math.MaxInt, 10, promptCacheTTL1h, continuationTarget()); ok {
		t.Fatalf("可能溢出的输入应回退，得到 %+v", usage)
	}
}

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

func TestAllocateClaudeUsageChecksAtMostSixtyFourCandidates(t *testing.T) {
	_, _, checked := allocateClaudeUsageWithCandidateCount(
		100_000,
		100,
		promptCacheTTL1h,
		continuationTarget(),
	)
	if checked <= 0 || checked > maxClaudeUsageCandidates {
		t.Fatalf("候选数量必须位于 1..64，得到 %d", checked)
	}
}

func TestAllocateClaudeUsagePreservesOpusSonnetAndHaikuBaseCost(t *testing.T) {
	const rawInput = 100_000
	usage, ok := allocateClaudeUsage(rawInput, 999, promptCacheTTL1h, continuationTarget())
	if !ok {
		t.Fatalf("分配失败")
	}

	for model, inputPrice := range map[string]float64{
		"opus":   5,
		"sonnet": 3,
		"haiku":  1,
	} {
		original := float64(rawInput) * inputPrice
		allocated := float64(usage.InputTokens)*inputPrice +
			float64(usage.CacheReadInputTokens)*(0.10*inputPrice) +
			float64(usage.CacheCreation.Ephemeral5mInputTokens)*(1.25*inputPrice) +
			float64(usage.CacheCreation.Ephemeral1hInputTokens)*(2.00*inputPrice)
		if math.Abs(original-allocated) > 1e-9 {
			t.Fatalf("%s 基础费用不守恒：原始=%f 分配后=%f", model, original, allocated)
		}
	}
}

func TestClaudeUsageJSONAlwaysIncludesCompleteCacheFields(t *testing.T) {
	raw, err := json.Marshal(ClaudeUsage{})
	if err != nil {
		t.Fatalf("序列化 ClaudeUsage 失败：%v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析 ClaudeUsage JSON 失败：%v", err)
	}
	for _, field := range []string{
		"input_tokens",
		"output_tokens",
		"cache_creation_input_tokens",
		"cache_read_input_tokens",
		"cache_creation",
	} {
		if _, ok := decoded[field]; !ok {
			t.Fatalf("ClaudeUsage 缺少标准字段 %q：%s", field, raw)
		}
	}

	cacheCreation, ok := decoded["cache_creation"].(map[string]interface{})
	if !ok {
		t.Fatalf("cache_creation 应为对象：%s", raw)
	}
	for _, field := range []string{
		"ephemeral_5m_input_tokens",
		"ephemeral_1h_input_tokens",
	} {
		if _, ok := cacheCreation[field]; !ok {
			t.Fatalf("cache_creation 缺少标准字段 %q：%s", field, raw)
		}
	}
}

func representativeClaudeUsageFeatures(index int) claudeUsageFeatures {
	return claudeUsageFeatures{
		Phase:                promptCachePhaseContinue,
		ReuseRatio:           float64((index*37)%901) / 1000,
		GrowthRatio:          0.0001 + float64((index*53)%500)/10_000,
		AgeRatio:             float64((index*71)%1000) / 1000,
		RoundFactor:          float64((index*89)%1000) / 1000,
		SizeFactor:           float64((index*97)%1000) / 1000,
		ToolRatio:            float64((index*113)%400) / 1000,
		StableHitJitter:      float64((index*131)%2001)/1000 - 1,
		StableCreationJitter: float64((index*149)%2001)/1000 - 1,
	}
}

func continuationTarget() claudeUsageTargets {
	return claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:                promptCachePhaseContinue,
		ReuseRatio:           0.8,
		GrowthRatio:          0.03,
		AgeRatio:             0.2,
		RoundFactor:          0.4,
		SizeFactor:           0.8,
		ToolRatio:            0.1,
		StableHitJitter:      0.1,
		StableCreationJitter: 0.1,
		CreateCache:          true,
	})
}

func assertTargetShares(t *testing.T, target claudeUsageTargets) {
	t.Helper()
	sum := target.InputShare + target.ReadShare + target.CreateShare
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("目标比例之和应为 1，得到 %.12f", sum)
	}
	if target.InputShare < 0.01 || target.InputShare > 0.15 {
		t.Fatalf("普通输入目标越界：%.6f", target.InputShare)
	}
	if target.ReadShare < minClaudeReadHitRate || target.ReadShare > maxClaudeReadHitRate {
		t.Fatalf("真实读取命中率目标越界：%.6f", target.ReadShare)
	}
}

func assertClaudeUsageConserved(t *testing.T, rawInput int, usage ClaudeUsage) {
	t.Helper()
	left := 20 * rawInput
	right := 20*usage.InputTokens +
		2*usage.CacheReadInputTokens +
		25*usage.CacheCreation.Ephemeral5mInputTokens +
		40*usage.CacheCreation.Ephemeral1hInputTokens
	if left != right {
		t.Fatalf("费用不守恒：左=%d 右=%d usage=%+v", left, right, usage)
	}
	if usage.CacheCreationInputTokens !=
		usage.CacheCreation.Ephemeral5mInputTokens+usage.CacheCreation.Ephemeral1hInputTokens {
		t.Fatalf("缓存创建汇总与 TTL 明细不一致")
	}
}

func assertUsageRatios(t *testing.T, usage ClaudeUsage, expectRead bool) {
	t.Helper()
	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total <= 0 {
		t.Fatalf("展示 token 总量必须为正")
	}
	inputShare := float64(usage.InputTokens) / float64(total)
	readShare := float64(usage.CacheReadInputTokens) / float64(total)
	if readShare < minClaudeReadHitRate || readShare > maxClaudeReadHitRate {
		t.Fatalf("真实读取命中率越界：%.6f", readShare)
	}
	if inputShare < 0.01 || inputShare > 0.15 {
		t.Fatalf("普通输入比例越界：%.6f", inputShare)
	}
	if expectRead {
		if usage.CacheReadInputTokens <= 0 || usage.CacheCreationInputTokens <= 0 {
			t.Fatalf("读取并创建请求必须同时有读取和创建：%+v", usage)
		}
	} else if usage.CacheCreationInputTokens != 0 {
		t.Fatalf("只读取请求不得创建缓存：%+v", usage)
	}
}
