package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type benchmarkFirstTokenWriter struct {
	header       http.Header
	firstTokenAt time.Time
}

func newBenchmarkFirstTokenWriter() *benchmarkFirstTokenWriter {
	return &benchmarkFirstTokenWriter{header: make(http.Header)}
}

func (w *benchmarkFirstTokenWriter) Header() http.Header {
	return w.header
}

func (w *benchmarkFirstTokenWriter) WriteHeader(int) {}

func (w *benchmarkFirstTokenWriter) Write(p []byte) (int, error) {
	if w.firstTokenAt.IsZero() &&
		(bytes.Contains(p, []byte("event: message_start")) ||
			bytes.Contains(p, []byte("event: content_block_delta"))) {
		w.firstTokenAt = time.Now()
	}
	return len(p), nil
}

func (w *benchmarkFirstTokenWriter) Flush() {}

func benchmarkClaudeFirstToken(b *testing.B, size int) {
	cfgFile := filepath.Join(b.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		b.Fatalf("初始化配置失败：%v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "benchmark-account",
		Enabled:     true,
		AccessToken: "benchmark-token",
		ProfileArn:  "arn:aws:codewhisperer:us-east-1:123456789012:profile/benchmark",
	}); err != nil {
		b.Fatalf("添加基准账户失败：%v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		b.Fatalf("设置基准端点失败：%v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		b.Fatalf("关闭端点回退失败：%v", err)
	}

	pool := accountpool.GetPool()
	pool.Reload()
	if err := config.DeleteAccount("benchmark-account"); err != nil {
		b.Fatalf("移除持久化基准账户失败：%v", err)
	}

	frame := benchmarkAWSEventStreamFrame(b, "assistantResponseEvent", map[string]interface{}{
		"content": "x",
	})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame)
	}))
	defer upstream.Close()

	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    upstream.URL,
		Origin: "AI_EDITOR",
		Name:   "benchmark",
	}}
	defer func() { kiroEndpoints = oldEndpoints }()

	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: false,
		},
	})
	defer kiroHttpStore.Store(oldClient)

	handler := &Handler{
		pool:        pool,
		promptCache: newPromptCacheTracker(time.Hour),
	}
	claudeReq := benchmarkClaudeRequest(size)
	claudeReq.Stream = true
	body, err := json.Marshal(claudeReq)
	if err != nil {
		b.Fatalf("编码基准请求失败：%v", err)
	}

	var firstTokenTotal time.Duration
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		writer := newBenchmarkFirstTokenWriter()
		startedAt := time.Now()
		handler.handleClaudeMessages(writer, req)
		if writer.firstTokenAt.IsZero() {
			b.Fatalf("未捕获 message_start 或内容事件")
		}
		firstTokenTotal += writer.firstTokenAt.Sub(startedAt)
	}

	b.ReportMetric(float64(firstTokenTotal.Nanoseconds())/float64(b.N), "first-token-ns/op")
}

func benchmarkAWSEventStreamFrame(b *testing.B, eventType string, payload map[string]interface{}) []byte {
	b.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		b.Fatalf("编码假上游事件失败：%v", err)
	}

	headerValue := []byte(eventType)
	headers := make([]byte, 0, 1+len(":event-type")+1+2+len(headerValue))
	headers = append(headers, byte(len(":event-type")))
	headers = append(headers, []byte(":event-type")...)
	headers = append(headers, byte(7))
	headers = append(headers, byte(len(headerValue)>>8), byte(len(headerValue)))
	headers = append(headers, headerValue...)

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

func BenchmarkClaudeFirstToken1KB(b *testing.B) {
	benchmarkClaudeFirstToken(b, 1<<10)
}

func BenchmarkClaudeFirstToken64KB(b *testing.B) {
	benchmarkClaudeFirstToken(b, 64<<10)
}

func BenchmarkClaudeFirstToken512KB(b *testing.B) {
	benchmarkClaudeFirstToken(b, 512<<10)
}

func BenchmarkClaudeFirstToken2MB(b *testing.B) {
	benchmarkClaudeFirstToken(b, 2<<20)
}
