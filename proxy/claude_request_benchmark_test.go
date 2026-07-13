package proxy

import (
	"strings"
	"testing"
	"time"
)

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

func BenchmarkLegacyClaudeAnalysis1KB(b *testing.B) {
	benchmarkLegacyClaudeAnalysis(b, 1<<10)
}

func BenchmarkLegacyClaudeAnalysis64KB(b *testing.B) {
	benchmarkLegacyClaudeAnalysis(b, 64<<10)
}

func BenchmarkLegacyClaudeAnalysis512KB(b *testing.B) {
	benchmarkLegacyClaudeAnalysis(b, 512<<10)
}

func BenchmarkLegacyClaudeAnalysis2MB(b *testing.B) {
	benchmarkLegacyClaudeAnalysis(b, 2<<20)
}
