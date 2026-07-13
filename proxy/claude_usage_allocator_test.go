package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
)

func TestClaudeUsageTargetsFirstRoundHasCreationOnly(t *testing.T) {
	target := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:       promptCachePhaseFirst,
		GrowthRatio: 1,
		SizeFactor:  0.7,
		ToolRatio:   0.1,
	})

	if target.ReadShare != 0 {
		t.Fatalf("首轮不得包含缓存读取，得到 %.6f", target.ReadShare)
	}
	if target.CreateShare <= 0 {
		t.Fatalf("首轮应以缓存创建为主")
	}
	assertTargetShares(t, target)
}

func TestClaudeUsageTargetsGrowthRaisesCreationShare(t *testing.T) {
	base := claudeUsageFeatures{
		Phase:        promptCachePhaseContinue,
		ReuseRatio:   0.8,
		AgeRatio:     0.2,
		RoundFactor:  0.3,
		SizeFactor:   0.7,
		ToolRatio:    0.1,
		StableJitter: 0.1,
	}
	lowGrowth := base
	lowGrowth.GrowthRatio = 0.005
	highGrowth := base
	highGrowth.GrowthRatio = 0.12

	low := claudeUsageTargetsForFeatures(lowGrowth)
	high := claudeUsageTargetsForFeatures(highGrowth)

	if high.CreateShare <= low.CreateShare {
		t.Fatalf("增长比例提高时创建占比不应下降：低=%.6f 高=%.6f", low.CreateShare, high.CreateShare)
	}
}

func TestClaudeUsageTargetsReuseRaisesReadShare(t *testing.T) {
	base := claudeUsageFeatures{
		Phase:        promptCachePhaseContinue,
		GrowthRatio:  0.04,
		AgeRatio:     0.2,
		RoundFactor:  0.3,
		SizeFactor:   0.7,
		ToolRatio:    0.1,
		StableJitter: 0.1,
	}
	lowReuse := base
	lowReuse.ReuseRatio = 0.4
	highReuse := base
	highReuse.ReuseRatio = 0.9

	low := claudeUsageTargetsForFeatures(lowReuse)
	high := claudeUsageTargetsForFeatures(highReuse)

	if high.ReadShare <= low.ReadShare {
		t.Fatalf("复用比例提高时读取占比不应下降：低=%.6f 高=%.6f", low.ReadShare, high.ReadShare)
	}
}

func TestClaudeUsageTargetsRoundAgeToolAndJitterAffectResult(t *testing.T) {
	baseFeatures := claudeUsageFeatures{
		Phase:        promptCachePhaseContinue,
		ReuseRatio:   0.75,
		GrowthRatio:  0.03,
		AgeRatio:     0.2,
		RoundFactor:  0.2,
		SizeFactor:   0.7,
		ToolRatio:    0.1,
		StableJitter: 0,
	}
	base := claudeUsageTargetsForFeatures(baseFeatures)

	cases := map[string]claudeUsageFeatures{}
	round := baseFeatures
	round.RoundFactor = 0.8
	cases["轮次"] = round
	age := baseFeatures
	age.AgeRatio = 0.8
	cases["年龄"] = age
	tool := baseFeatures
	tool.ToolRatio = 0.35
	cases["工具"] = tool
	jitter := baseFeatures
	jitter.StableJitter = 0.8
	cases["微扰"] = jitter

	for name, features := range cases {
		got := claudeUsageTargetsForFeatures(features)
		if got == base {
			t.Fatalf("%s 特征变化后目标比例不应完全相同", name)
		}
		assertTargetShares(t, got)
	}
}

func TestClaudeUsageTargetsProduceDiverseBuckets(t *testing.T) {
	buckets := make(map[string]int)
	for i := 0; i < 5_000; i++ {
		features := representativeClaudeUsageFeatures(i)
		target := claudeUsageTargetsForFeatures(features)
		key := fmt.Sprintf("%.3f/%.3f/%.3f", target.InputShare, target.ReadShare, target.CreateShare)
		buckets[key]++
	}

	if len(buckets) < 500 {
		t.Fatalf("0.1%% 分桶后应至少有 500 种组合，得到 %d", len(buckets))
	}
	for bucket, count := range buckets {
		if float64(count)/5_000 > 0.05 {
			t.Fatalf("单一比例桶 %s 占比超过 5%%：%.2f%%", bucket, float64(count)/50)
		}
	}
}

func TestClaudeUsageTargetsDoNotPileUpAtBounds(t *testing.T) {
	var atMin, atMax int
	const samples = 5_000
	for i := 0; i < samples; i++ {
		target := claudeUsageTargetsForFeatures(representativeClaudeUsageFeatures(i))
		cacheTotal := 1 - target.InputShare
		createMin := math.Max(0.08, cacheTotal-0.90)
		createMax := math.Min(0.20, cacheTotal-0.78)
		if math.Abs(target.CreateShare-createMin) < 1e-12 {
			atMin++
		}
		if math.Abs(target.CreateShare-createMax) < 1e-12 {
			atMax++
		}
	}

	if float64(atMin)/samples > 0.30 {
		t.Fatalf("创建下限命中率超过 30%%：%.2f%%", float64(atMin)*100/samples)
	}
	if float64(atMax)/samples > 0.30 {
		t.Fatalf("创建上限命中率超过 30%%：%.2f%%", float64(atMax)*100/samples)
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

func TestAllocateClaudeUsageRespectsPhaseBounds(t *testing.T) {
	firstTarget := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:       promptCachePhaseFirst,
		GrowthRatio: 1,
		SizeFactor:  0.8,
		ToolRatio:   0.1,
	})
	first, ok := allocateClaudeUsage(20_000, 100, promptCacheTTL1h, firstTarget)
	if !ok {
		t.Fatalf("首轮分配失败")
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("首轮不得读取缓存")
	}
	assertUsageRatios(t, first, false)

	continued, ok := allocateClaudeUsage(20_000, 100, promptCacheTTL1h, continuationTarget())
	if !ok {
		t.Fatalf("续轮分配失败")
	}
	if continued.CacheReadInputTokens == 0 || continued.CacheCreationInputTokens == 0 {
		t.Fatalf("续轮应同时包含读取和创建")
	}
	assertUsageRatios(t, continued, true)
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

func TestAllocateClaudeUsageRejectsOverflow(t *testing.T) {
	if usage, ok := allocateClaudeUsage(math.MaxInt, 10, promptCacheTTL1h, continuationTarget()); ok {
		t.Fatalf("可能溢出的输入应回退，得到 %+v", usage)
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
		Phase:        promptCachePhaseContinue,
		ReuseRatio:   float64((index*37)%901) / 1000,
		GrowthRatio:  0.0001 + float64((index*53)%500)/10_000,
		AgeRatio:     float64((index*71)%1000) / 1000,
		RoundFactor:  float64((index*89)%1000) / 1000,
		SizeFactor:   float64((index*97)%1000) / 1000,
		ToolRatio:    float64((index*113)%400) / 1000,
		StableJitter: float64((index*131)%2001)/1000 - 1,
	}
}

func continuationTarget() claudeUsageTargets {
	return claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:        promptCachePhaseContinue,
		ReuseRatio:   0.8,
		GrowthRatio:  0.03,
		AgeRatio:     0.2,
		RoundFactor:  0.4,
		SizeFactor:   0.8,
		ToolRatio:    0.1,
		StableJitter: 0.1,
	})
}

func assertTargetShares(t *testing.T, target claudeUsageTargets) {
	t.Helper()
	sum := target.InputShare + target.ReadShare + target.CreateShare
	if math.Abs(sum-1) > 1e-12 {
		t.Fatalf("目标比例之和应为 1，得到 %.12f", sum)
	}
	if target.InputShare < 0.01 || target.InputShare > 0.05 {
		t.Fatalf("普通输入目标越界：%.6f", target.InputShare)
	}
	cacheRate := target.ReadShare + target.CreateShare
	if cacheRate < 0.95 || cacheRate > 0.99 {
		t.Fatalf("缓存目标越界：%.6f", cacheRate)
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

func assertUsageRatios(t *testing.T, usage ClaudeUsage, continuation bool) {
	t.Helper()
	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total <= 0 {
		t.Fatalf("展示 token 总量必须为正")
	}
	inputShare := float64(usage.InputTokens) / float64(total)
	readShare := float64(usage.CacheReadInputTokens) / float64(total)
	createShare := float64(usage.CacheCreationInputTokens) / float64(total)
	cacheRate := readShare + createShare
	if cacheRate < 0.95 || cacheRate > 0.99 {
		t.Fatalf("缓存率越界：%.6f", cacheRate)
	}
	if inputShare < 0.01 || inputShare > 0.05 {
		t.Fatalf("普通输入比例越界：%.6f", inputShare)
	}
	if continuation {
		if readShare < 0.78 || readShare > 0.90 {
			t.Fatalf("续轮读取比例越界：%.6f", readShare)
		}
		if createShare < 0.08 || createShare > 0.20 {
			t.Fatalf("续轮创建比例越界：%.6f", createShare)
		}
	}
}
