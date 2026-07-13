package proxy

import "testing"

func BenchmarkAllocateClaudeUsage(b *testing.B) {
	target := continuationTarget()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		usage, ok := allocateClaudeUsage(100_000+i%1_000, 2_000, promptCacheTTL1h, target)
		if !ok {
			b.Fatal("分配失败")
		}
		_ = usage
	}
}

func BenchmarkBuildFeaturesAndAllocateClaudeUsage(b *testing.B) {
	analysis := analyzeClaudeRequest(benchmarkClaudeRequest(512<<10), "benchmark-api-key")
	snapshot := promptCacheSnapshot{
		TaskKey:             analysis.TaskKey,
		RequestFingerprint:  analysis.RequestFingerprint,
		TTL:                 promptCacheTTL1h,
		Phase:               promptCachePhaseContinue,
		MatchedPrefixTokens: analysis.CacheableTokens * 8 / 10,
		CurrentCacheable:    analysis.CacheableTokens,
		SuccessfulRounds:    8,
		AgeRatio:            0.4,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		features := buildClaudeUsageFeatures(snapshot, analysis, 100_000+i%1_000)
		target := claudeUsageTargetsForFeatures(features)
		usage, ok := allocateClaudeUsage(100_000+i%1_000, 2_000, snapshot.TTL, target)
		if !ok {
			b.Fatal("分配失败")
		}
		_ = usage
	}
}
