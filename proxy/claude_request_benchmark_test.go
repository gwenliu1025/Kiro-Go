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
		tokens := estimateClaudeRequestInputTokensLegacy(req)
		_ = tracker.BuildClaudeProfile(req, tokens)
	}
}

func benchmarkNewClaudeAnalysis(b *testing.B, size int) {
	req := benchmarkClaudeRequest(size)

	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = analyzeClaudeRequest(req, "benchmark-api-key")
	}
}

func benchmarkStructuredClaudeAnalysis(
	b *testing.B,
	req *ClaudeRequest,
	size int,
	legacy bool,
) {
	var tracker *promptCacheTracker
	if legacy {
		tracker = newPromptCacheTracker(time.Hour)
	}

	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if legacy {
			tokens := estimateClaudeRequestInputTokensLegacy(req)
			_ = tracker.BuildClaudeProfile(req, tokens)
			continue
		}
		_ = analyzeClaudeRequest(req, "benchmark-api-key")
	}
}

func structuredClaudeBenchmarkCases() map[string]struct {
	req  *ClaudeRequest
	size int
} {
	const mediumBlockSize = 5 << 10
	mediumText := strings.Repeat("structured benchmark text ", mediumBlockSize/26+1)[:mediumBlockSize]

	contentBlocks := make([]interface{}, 0, 16)
	for i := 0; i < cap(contentBlocks); i++ {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": mediumText,
		})
	}

	manyMediumBlocks := make([]interface{}, 0, 256)
	for i := 0; i < cap(manyMediumBlocks); i++ {
		manyMediumBlocks = append(manyMediumBlocks, map[string]interface{}{
			"type": "text",
			"text": mediumText,
		})
	}

	imageData := strings.Repeat("A", 512<<10)
	toolResult := strings.Repeat("tool result line\n", (256<<10)/17+1)[:256<<10]
	typedText := strings.Repeat("typed content ", (64<<10)/14+1)[:64<<10]
	typedImage := strings.Repeat("B", 64<<10)

	return map[string]struct {
		req  *ClaudeRequest
		size int
	}{
		"ContentBlocks": {
			req: &ClaudeRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: contentBlocks}},
			},
			size: len(mediumText) * len(contentBlocks),
		},
		"Image": {
			req: &ClaudeRequest{
				Model: "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: []interface{}{
					map[string]interface{}{
						"type": "image",
						"source": map[string]interface{}{
							"type":       "base64",
							"media_type": "image/png",
							"data":       imageData,
						},
					},
				}}},
			},
			size: len(imageData),
		},
		"ToolResult": {
			req: &ClaudeRequest{
				Model: "claude-sonnet-4-6",
				Messages: []ClaudeMessage{
					{Role: "user", Content: "读取文件"},
					{Role: "assistant", Content: []interface{}{
						map[string]interface{}{
							"type":  "tool_use",
							"id":    "tool-1",
							"name":  "read_file",
							"input": map[string]interface{}{"path": "large.txt"},
						},
					}},
					{Role: "user", Content: []interface{}{
						map[string]interface{}{
							"type":        "tool_result",
							"tool_use_id": "tool-1",
							"content":     toolResult,
						},
					}},
				},
			},
			size: len(toolResult),
		},
		"TypedContent": {
			req: &ClaudeRequest{
				Model: "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: []ClaudeContentBlock{
					{Type: "text", Text: typedText},
					{
						Type: "image",
						Source: &ImageSource{
							Type:      "base64",
							MediaType: "image/png",
							Data:      typedImage,
						},
					},
				}}},
			},
			size: len(typedText) + len(typedImage),
		},
		"ManyMediumTextBlocks": {
			req: &ClaudeRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []ClaudeMessage{{Role: "user", Content: manyMediumBlocks}},
			},
			size: len(mediumText) * len(manyMediumBlocks),
		},
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

func BenchmarkNewClaudeAnalysis1KB(b *testing.B) {
	benchmarkNewClaudeAnalysis(b, 1<<10)
}

func BenchmarkNewClaudeAnalysis64KB(b *testing.B) {
	benchmarkNewClaudeAnalysis(b, 64<<10)
}

func BenchmarkNewClaudeAnalysis512KB(b *testing.B) {
	benchmarkNewClaudeAnalysis(b, 512<<10)
}

func BenchmarkNewClaudeAnalysis2MB(b *testing.B) {
	benchmarkNewClaudeAnalysis(b, 2<<20)
}

func BenchmarkStructuredClaudeAnalysis(b *testing.B) {
	for name, benchmarkCase := range structuredClaudeBenchmarkCases() {
		b.Run("Legacy/"+name, func(b *testing.B) {
			benchmarkStructuredClaudeAnalysis(
				b,
				benchmarkCase.req,
				benchmarkCase.size,
				true,
			)
		})
		b.Run("New/"+name, func(b *testing.B) {
			benchmarkStructuredClaudeAnalysis(
				b,
				benchmarkCase.req,
				benchmarkCase.size,
				false,
			)
		})
	}
}
