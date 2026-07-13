package proxy

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestPromptCacheTrackerUsesCallerAndTaskKey(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := analysisTestRequest()
	firstCaller := analyzeClaudeRequest(req, "api-key-1")
	secondCaller := analyzeClaudeRequest(req, "api-key-2")
	now := time.Unix(1_700_000_000, 0)

	firstSnapshot := tracker.Snapshot(firstCaller, now)
	tracker.Commit(promptCacheCommit{
		TaskKey:            firstCaller.TaskKey,
		RequestFingerprint: firstCaller.RequestFingerprint,
		TTL:                firstSnapshot.TTL,
		Prefixes:           firstCaller.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	secondSnapshot := tracker.Snapshot(secondCaller, now.Add(time.Minute))
	if secondSnapshot.Phase != promptCachePhaseFirst {
		t.Fatalf("不同调用方应是独立首轮，得到 phase=%v", secondSnapshot.Phase)
	}
	if secondSnapshot.SuccessfulRounds != 0 || secondSnapshot.ExistingUsage != nil {
		t.Fatalf("不同调用方不应读取已有状态：%+v", secondSnapshot)
	}
}

func TestPromptCacheTrackerRetryFingerprintIsIdempotent(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	analysis := analyzeClaudeRequest(analysisTestRequest(), "api-key-1")
	now := time.Unix(1_700_000_000, 0)
	snapshot := tracker.Snapshot(analysis, now)
	usage := testClaudeUsage(120, 12)

	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                snapshot.TTL,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now,
	})

	retry := tracker.Snapshot(analysis, now.Add(time.Second))
	if retry.ExistingUsage == nil {
		t.Fatalf("相同请求指纹应复用已有 usage")
	}
	if !reflect.DeepEqual(*retry.ExistingUsage, usage) {
		t.Fatalf("幂等 usage 不一致：得到 %+v，期望 %+v", *retry.ExistingUsage, usage)
	}
	if retry.SuccessfulRounds != 1 {
		t.Fatalf("重试不得推进轮次，得到 %d", retry.SuccessfulRounds)
	}

	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                snapshot.TTL,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now.Add(time.Second),
	})
	afterDuplicateCommit := tracker.Snapshot(analysis, now.Add(2*time.Second))
	if afterDuplicateCommit.SuccessfulRounds != 1 {
		t.Fatalf("重复提交不得推进轮次，得到 %d", afterDuplicateCommit.SuccessfulRounds)
	}
}

func TestPromptCacheTrackerFindsLongestSuccessfulPrefix(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	base := analysisTestRequest()
	base.Messages = append(base.Messages,
		ClaudeMessage{Role: "assistant", Content: "第一轮回答"},
	)
	first := analyzeClaudeRequest(base, "api-key-1")
	now := time.Unix(1_700_000_000, 0)
	firstSnapshot := tracker.Snapshot(first, now)
	tracker.Commit(promptCacheCommit{
		TaskKey:            first.TaskKey,
		RequestFingerprint: first.RequestFingerprint,
		TTL:                firstSnapshot.TTL,
		Prefixes:           first.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	continued := cloneClaudeRequestForAnalysisTest(base)
	continued.Messages = append(continued.Messages,
		ClaudeMessage{Role: "user", Content: "第二轮问题"},
	)
	second := analyzeClaudeRequest(continued, "api-key-1")
	snapshot := tracker.Snapshot(second, now.Add(time.Minute))
	want := first.Prefixes[len(first.Prefixes)-1].CumulativeTokens

	if snapshot.Phase != promptCachePhaseContinue {
		t.Fatalf("应进入续轮，得到 phase=%v", snapshot.Phase)
	}
	if snapshot.MatchedPrefixTokens != want {
		t.Fatalf("最长前缀 token 不符：得到 %d，期望 %d", snapshot.MatchedPrefixTokens, want)
	}
}

func TestPromptCacheTrackerDoesNotAdvanceOnFailedRequest(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	analysis := analyzeClaudeRequest(analysisTestRequest(), "api-key-1")
	now := time.Unix(1_700_000_000, 0)

	first := tracker.Snapshot(analysis, now)
	second := tracker.Snapshot(analysis, now.Add(time.Minute))

	if first.Phase != promptCachePhaseFirst || second.Phase != promptCachePhaseFirst {
		t.Fatalf("未提交的失败请求不得离开首轮：first=%v second=%v", first.Phase, second.Phase)
	}
	if second.SuccessfulRounds != 0 || second.ExistingUsage != nil {
		t.Fatalf("未提交的失败请求不得推进状态：%+v", second)
	}
}

func TestPromptCacheTrackerFiveMinuteReadRefreshesActivity(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL5m)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL5m,
		Prefixes:           analysis.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	continued := continuedAnalysis(analysis, "api-key-ttl-5m")
	firstRead := tracker.Snapshot(continued, now.Add(4*time.Minute))
	if firstRead.Phase != promptCachePhaseContinue {
		t.Fatalf("4 分钟时应命中续轮，得到 %v", firstRead.Phase)
	}

	secondRead := tracker.Snapshot(continued, now.Add(8*time.Minute))
	if secondRead.Phase != promptCachePhaseContinue {
		t.Fatalf("5 分钟读取应刷新活动窗口，8 分钟时仍应续轮，得到 %v", secondRead.Phase)
	}
}

func TestPromptCacheTrackerOneHourReadDoesNotExtendCreationExpiry(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL1h)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL1h,
		Prefixes:           analysis.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	continued := continuedAnalysis(analysis, "api-key-ttl-1h")
	beforeExpiry := tracker.Snapshot(continued, now.Add(59*time.Minute))
	if beforeExpiry.Phase != promptCachePhaseContinue {
		t.Fatalf("59 分钟时应命中续轮，得到 %v", beforeExpiry.Phase)
	}

	afterExpiry := tracker.Snapshot(continued, now.Add(61*time.Minute))
	if afterExpiry.Phase != promptCachePhaseRebuild {
		t.Fatalf("读取不得延长 1 小时创建期限，得到 %v", afterExpiry.Phase)
	}
}

func TestPromptCacheTrackerExpiredTaskRebuilds(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL5m)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL5m,
		Prefixes:           analysis.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	continued := continuedAnalysis(analysis, "api-key-ttl-5m")
	snapshot := tracker.Snapshot(continued, now.Add(6*time.Minute))
	if snapshot.Phase != promptCachePhaseRebuild {
		t.Fatalf("TTL 过期后应重建，得到 %v", snapshot.Phase)
	}
	if snapshot.MatchedPrefixTokens != 0 || snapshot.ExistingUsage != nil {
		t.Fatalf("重建轮不得复用旧前缀或 usage：%+v", snapshot)
	}
}

func TestPromptCacheTrackerPrunesAfterSeventyMinutes(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL1h)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL1h,
		Prefixes:           analysis.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	afterRetention := tracker.Snapshot(analysis, now.Add(71*time.Minute))
	if afterRetention.Phase != promptCachePhaseFirst {
		t.Fatalf("70 分钟清理后应作为新任务，得到 %v", afterRetention.Phase)
	}
	if tracker.taskCount() != 0 {
		t.Fatalf("过期任务和幂等记录应已清理")
	}
}

func TestPromptCacheTrackerIgnoresUpstreamAccountSwitch(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	req := analysisTestRequest()
	analysis := analyzeClaudeRequest(req, "api-key-1")
	now := time.Unix(1_700_000_000, 0)
	snapshot := tracker.Snapshot(analysis, now)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                snapshot.TTL,
		Prefixes:           analysis.Prefixes,
		Usage:              testClaudeUsage(100, 10),
		SuccessfulAt:       now,
	})

	for range []string{"upstream-a", "upstream-b"} {
		retry := tracker.Snapshot(analysis, now.Add(time.Second))
		if retry.ExistingUsage == nil {
			t.Fatalf("上游账号切换不得丢失幂等 usage")
		}
	}
}

func TestPromptCacheTrackerTTLDistributionIsStableTwentyEighty(t *testing.T) {
	var fiveMinute int
	const samples = 10_000
	for i := 0; i < samples; i++ {
		analysis := analyzeClaudeRequest(&ClaudeRequest{
			Model:    "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: fmt.Sprintf("task-%d", i)}},
		}, "api-key-distribution")
		if ttlForTask(analysis.TaskKey) == promptCacheTTL5m {
			fiveMinute++
		}
	}

	ratio := float64(fiveMinute) / samples
	if ratio < 0.18 || ratio > 0.22 {
		t.Fatalf("5 分钟任务比例应稳定接近 20%%，得到 %.2f%%", ratio*100)
	}
}

func TestPromptCacheTrackerConcurrentSnapshotAndCommit(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	const workers = 32

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(index int) {
			defer wg.Done()
			analysis := analyzeClaudeRequest(&ClaudeRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: fmt.Sprintf("concurrent-%d", index)}},
			}, "api-key-concurrent")
			snapshot := tracker.Snapshot(analysis, now)
			tracker.Commit(promptCacheCommit{
				TaskKey:            analysis.TaskKey,
				RequestFingerprint: analysis.RequestFingerprint,
				TTL:                snapshot.TTL,
				Prefixes:           analysis.Prefixes,
				Usage:              testClaudeUsage(100+index, 10),
				SuccessfulAt:       now,
			})
		}(i)
	}
	wg.Wait()

	if tracker.taskCount() != workers {
		t.Fatalf("并发提交后任务数量不符：得到 %d，期望 %d", tracker.taskCount(), workers)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("期望普通输入 token 为 50，得到 %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("期望缓存创建 token 为 30，得到 %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("期望缓存读取 token 为 20，得到 %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("期望缓存创建明细，得到 %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 ||
		creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("TTL 明细不符：%#v", creation)
	}
}

func testClaudeUsage(inputTokens, outputTokens int) ClaudeUsage {
	return ClaudeUsage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: inputTokens * 4,
		CacheCreation: ClaudeCacheCreationUsage{
			Ephemeral1hInputTokens: inputTokens * 4,
		},
	}
}

func analysisForTTL(t *testing.T, ttl promptCacheTTL) claudeRequestAnalysis {
	t.Helper()
	for i := 0; i < 10_000; i++ {
		caller := fmt.Sprintf("api-key-ttl-%d-%d", ttl, i)
		analysis := analyzeClaudeRequest(&ClaudeRequest{
			Model:    "claude-sonnet-4-6",
			Messages: []ClaudeMessage{{Role: "user", Content: "固定任务"}},
		}, caller)
		if ttlForTask(analysis.TaskKey) == ttl {
			return analysis
		}
	}
	t.Fatalf("未找到目标 TTL 的任务键")
	return claudeRequestAnalysis{}
}

func continuedAnalysis(base claudeRequestAnalysis, callerScope string) claudeRequestAnalysis {
	req := &ClaudeRequest{
		Model: "claude-sonnet-4-6",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "固定任务"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		},
	}
	continued := analyzeClaudeRequest(req, callerScope)
	continued.TaskKey = base.TaskKey
	return continued
}
