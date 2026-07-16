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
	tracker.Commit(promptCacheCommit{
		TaskKey:            continued.TaskKey,
		RequestFingerprint: continued.RequestFingerprint,
		TTL:                firstRead.TTL,
		Prefixes:           continued.Prefixes,
		Usage:              testClaudeUsage(110, 10),
		SuccessfulAt:       now.Add(4 * time.Minute),
	})

	secondRead := tracker.Snapshot(continued, now.Add(8*time.Minute))
	if secondRead.Phase != promptCachePhaseContinue {
		t.Fatalf("5 分钟读取应刷新活动窗口，8 分钟时仍应续轮，得到 %v", secondRead.Phase)
	}
}

func TestPromptCacheTrackerFiveMinuteFailedReadDoesNotRefreshActivity(t *testing.T) {
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
	beforeFailure := tracker.Snapshot(continued, now.Add(4*time.Minute))
	if beforeFailure.Phase != promptCachePhaseContinue {
		t.Fatalf("4 分钟时应命中续轮，得到 %v", beforeFailure.Phase)
	}

	afterFailure := tracker.Snapshot(continued, now.Add(6*time.Minute))
	if afterFailure.Phase != promptCachePhaseRebuild {
		t.Fatalf("失败读取不得刷新活动窗口，得到 %v", afterFailure.Phase)
	}
}

func TestPromptCacheTrackerFiveMinuteSuccessfulRetryRefreshesWithoutAdvancingRound(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL5m)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	usage := testClaudeUsage(100, 10)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL5m,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now,
	})

	retry := tracker.Snapshot(analysis, now.Add(4*time.Minute))
	if retry.ExistingUsage == nil {
		t.Fatalf("4 分钟时应命中幂等 usage")
	}
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL5m,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now.Add(4 * time.Minute),
	})

	afterRetry := tracker.Snapshot(analysis, now.Add(8*time.Minute))
	if afterRetry.Phase != promptCachePhaseContinue || afterRetry.ExistingUsage == nil {
		t.Fatalf("成功幂等重试应刷新 5 分钟活动窗口：%+v", afterRetry)
	}
	if afterRetry.SuccessfulRounds != 1 {
		t.Fatalf("成功幂等重试不得推进轮次：得到 %d", afterRetry.SuccessfulRounds)
	}
}

func TestPromptCacheTrackerFiveMinuteIdempotentRetryCrossingTTLKeepsPrefixes(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL5m)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	usage := testClaudeUsage(100, 10)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL5m,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now,
	})

	retry := tracker.Snapshot(analysis, now.Add(4*time.Minute))
	if retry.ExistingUsage == nil {
		t.Fatalf("4 分钟时应命中幂等 usage")
	}
	prepared := prepareFinalClaudeUsage(10_000, 20, analysis, retry, now.Add(6*time.Minute))
	if !prepared.OK {
		t.Fatalf("幂等重试应生成刷新提交：%+v", prepared)
	}
	tracker.Commit(prepared.Commit)

	continued := continuedAnalysis(analysis, "api-key-ttl-5m-crossing")
	afterRetry := tracker.Snapshot(continued, now.Add(10*time.Minute))
	if afterRetry.Phase != promptCachePhaseContinue || afterRetry.MatchedPrefixTokens == 0 {
		t.Fatalf("跨过旧 5 分钟边界的成功幂等重试应保留前缀并刷新活动窗口：%+v", afterRetry)
	}
	if afterRetry.SuccessfulRounds != 1 {
		t.Fatalf("跨 TTL 幂等重试不得推进轮次：得到 %d", afterRetry.SuccessfulRounds)
	}
}

func TestPromptCacheTrackerOneHourIdempotentRetryCrossingTTLDoesNotRenewCreation(t *testing.T) {
	analysis := analysisForTTL(t, promptCacheTTL1h)
	tracker := newPromptCacheTracker(time.Hour)
	now := time.Unix(1_700_000_000, 0)
	usage := testClaudeUsage(100, 10)
	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL1h,
		Prefixes:           analysis.Prefixes,
		Usage:              usage,
		SuccessfulAt:       now,
	})

	retry := tracker.Snapshot(analysis, now.Add(59*time.Minute))
	if retry.ExistingUsage == nil {
		t.Fatalf("59 分钟时应命中幂等 usage")
	}
	prepared := prepareFinalClaudeUsage(10_000, 20, analysis, retry, now.Add(61*time.Minute))
	if !prepared.OK {
		t.Fatalf("幂等重试应生成刷新提交：%+v", prepared)
	}
	tracker.Commit(prepared.Commit)

	afterRetry := tracker.Snapshot(analysis, now.Add(61*time.Minute+time.Second))
	if afterRetry.Phase != promptCachePhaseRebuild || afterRetry.ExistingUsage != nil {
		t.Fatalf("跨过 1 小时创建期限的幂等重试不得续期创建时间：%+v", afterRetry)
	}
	if afterRetry.SuccessfulRounds != 1 {
		t.Fatalf("跨 TTL 幂等重试不得推进轮次：得到 %d", afterRetry.SuccessfulRounds)
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

func TestPromptCacheTrackerExpiredRefreshDoesNotRecreatePrunedTask(t *testing.T) {
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

	staleSnapshot := tracker.Snapshot(analysis, now.Add(59*time.Minute))
	if staleSnapshot.ExistingUsage == nil {
		t.Fatalf("59 分钟时应取得可复用 usage")
	}

	tracker.Commit(promptCacheCommit{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                promptCacheTTL1h,
		SuccessfulAt:       now.Add(71 * time.Minute),
		RefreshExisting:    true,
	})

	if tracker.taskCount() != 0 {
		t.Fatalf("70 分钟清理后的旧快照刷新不得重建任务")
	}
	afterRefresh := tracker.Snapshot(analysis, now.Add(71*time.Minute))
	if afterRefresh.Phase != promptCachePhaseFirst || afterRefresh.ExistingUsage != nil {
		t.Fatalf("旧快照刷新后应保持新任务状态：%+v", afterRefresh)
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

func TestPromptCacheAlgorithmVersionIsV4(t *testing.T) {
	if promptCacheAlgorithmVersion != "native-high-cache-v4" {
		t.Fatalf("算法版本错误：%s", promptCacheAlgorithmVersion)
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
