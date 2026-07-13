package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const anonymousCallerScope = "anonymous"

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

type claudeAnalysisValueStats struct {
	EstimatedTokens int
	CacheableTokens int
	ToolTokens      int
	Meaningful      bool
}

type claudeAnalysisScratch struct {
	stringBuffer *bufio.Writer
}

type analysisWriterPair struct {
	fingerprint io.Writer
	token       io.Writer
}

func (p analysisWriterPair) Write(data []byte) (int, error) {
	if p.fingerprint != nil {
		if _, err := p.fingerprint.Write(data); err != nil {
			return 0, err
		}
	}
	if p.token != nil {
		if _, err := p.token.Write(data); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (p analysisWriterPair) WriteString(value string) (int, error) {
	if p.fingerprint != nil {
		if _, err := writeAnalysisWriterString(p.fingerprint, value); err != nil {
			return 0, err
		}
	}
	if p.token != nil {
		if _, err := writeAnalysisWriterString(p.token, value); err != nil {
			return 0, err
		}
	}
	return len(value), nil
}

func writeAnalysisWriterString(w io.Writer, value string) (int, error) {
	if stringWriter, ok := w.(io.StringWriter); ok {
		return stringWriter.WriteString(value)
	}
	return w.Write([]byte(value))
}

func analyzeClaudeRequest(req *ClaudeRequest, callerScope string) claudeRequestAnalysis {
	if req == nil {
		return claudeRequestAnalysis{}
	}

	callerScope = strings.TrimSpace(callerScope)
	if callerScope == "" {
		callerScope = anonymousCallerScope
	}

	scratch := &claudeAnalysisScratch{}
	taskHasher := sha256.New()
	contextHasher := sha256.New()
	taskWriter := bufio.NewWriterSize(taskHasher, 1024)
	contextWriter := bufio.NewWriterSize(contextHasher, 1024)
	contentWriter := bufio.NewWriterSize(io.Discard, 1024)
	commonWriter := analysisWriterPair{
		fingerprint: taskWriter,
		token:       contextWriter,
	}

	writeAnalysisString(taskWriter, "caller_scope", callerScope)
	model := strings.ToLower(strings.TrimSpace(req.Model))
	writeAnalysisStringTo(commonWriter, "model", model)

	analysis := claudeRequestAnalysis{
		Prefixes: make([]claudePrefixPoint, 0, len(req.Messages)),
	}

	for _, tool := range req.Tools {
		writeAnalysisFieldStart(commonWriter, "tool")
		nameTokens := writeCanonicalAnalysisString(commonWriter, tool.Name)
		descriptionTokens := writeCanonicalAnalysisString(commonWriter, tool.Description)
		schemaTokens := writeCanonicalAnalysisJSONAndCount(commonWriter, tool.InputSchema, scratch)
		writeAnalysisFieldEnd(commonWriter)

		toolTotal := nameTokens + descriptionTokens + schemaTokens
		analysis.EstimatedInputTokens += toolTotal
		analysis.CacheableTokens += toolTotal
	}

	writeAnalysisFieldStart(commonWriter, "system")
	systemStats := writeClaudeAnalysisValue(commonWriter, req.System, true, scratch)
	writeAnalysisFieldEnd(commonWriter)
	analysis.EstimatedInputTokens += systemStats.EstimatedTokens
	analysis.CacheableTokens += systemStats.CacheableTokens
	analysis.ToolTokens += systemStats.ToolTokens

	firstUserWritten := false
	for _, message := range req.Messages {
		writeAnalysisFieldStart(contextWriter, "message")
		writeAnalysisStringTo(contextWriter, "role", strings.ToLower(strings.TrimSpace(message.Role)))
		writeAnalysisFieldStart(contextWriter, "content")

		contentHasher := sha256.New()
		contentWriter.Reset(contentHasher)
		messageStats := writeClaudeAnalysisValue(
			analysisWriterPair{
				fingerprint: contextWriter,
				token:       contentWriter,
			},
			message.Content,
			true,
			scratch,
		)

		writeAnalysisFieldEnd(contextWriter)
		writeAnalysisFieldEnd(contextWriter)
		_ = contentWriter.Flush()
		contentWriter.Reset(io.Discard)

		if !firstUserWritten &&
			strings.EqualFold(strings.TrimSpace(message.Role), "user") &&
			messageStats.Meaningful {
			writeAnalysisDigest(taskWriter, "first_user", contentHasher.Sum(nil))
			firstUserWritten = true
		}

		analysis.EstimatedInputTokens += messageStats.EstimatedTokens
		analysis.CacheableTokens += messageStats.CacheableTokens
		analysis.ToolTokens += messageStats.ToolTokens

		_ = contextWriter.Flush()
		var prefixFingerprint [32]byte
		copy(prefixFingerprint[:], contextHasher.Sum(nil))
		analysis.Prefixes = append(analysis.Prefixes, claudePrefixPoint{
			Fingerprint:      prefixFingerprint,
			CumulativeTokens: analysis.CacheableTokens,
		})
	}

	writeAnalysisFieldStart(contextWriter, "request_options")
	writeClaudeRequestOptions(contextWriter, req, scratch)
	writeAnalysisFieldEnd(contextWriter)

	_ = taskWriter.Flush()
	_ = contextWriter.Flush()
	copy(analysis.TaskKey[:], taskHasher.Sum(nil))
	copy(analysis.RequestFingerprint[:], contextHasher.Sum(nil))
	return analysis
}

func writeClaudeRequestOptions(
	w io.Writer,
	req *ClaudeRequest,
	scratch *claudeAnalysisScratch,
) {
	writers := analysisWriterPair{fingerprint: w}
	writeAnalysisPairString(writers, "{")
	fieldCount := 0
	writeField := func(name string, value interface{}) {
		if fieldCount > 0 {
			writeAnalysisPairString(writers, ",")
		}
		writeCanonicalAnalysisJSONString(writers, name, scratch, nil)
		writeAnalysisPairString(writers, ":")
		writeCanonicalAnalysisJSONPair(w, nil, value, scratch)
		fieldCount++
	}
	writeField("max_tokens", req.MaxTokens)
	writeField("temperature", req.Temperature)
	writeField("thinking", req.Thinking)
	writeField("tool_choice", req.ToolChoice)
	writeField("top_p", req.TopP)
	writeAnalysisPairString(writers, "}")
}

func writeAnalysisString(h io.Writer, kind, value string) {
	writeAnalysisStringTo(h, kind, value)
}

func writeAnalysisStringTo(w io.Writer, kind, value string) {
	writeAnalysisFieldStart(w, kind)
	writeCanonicalAnalysisString(w, value)
	writeAnalysisFieldEnd(w)
}

func writeAnalysisValue(h io.Writer, kind string, value interface{}) {
	writeAnalysisFieldStart(h, kind)
	writeCanonicalAnalysisJSON(h, value)
	writeAnalysisFieldEnd(h)
}

func writeAnalysisDigest(w io.Writer, kind string, digest []byte) {
	writeAnalysisFieldStart(w, kind)
	_, _ = w.Write(digest)
	writeAnalysisFieldEnd(w)
}

func writeAnalysisFieldStart(w io.Writer, kind string) {
	_, _ = io.WriteString(w, "\x1e")
	_, _ = io.WriteString(w, strconv.Itoa(len(kind)))
	_, _ = io.WriteString(w, ":")
	_, _ = io.WriteString(w, kind)
	_, _ = io.WriteString(w, "\x1f")
}

func writeAnalysisFieldEnd(w io.Writer) {
	_, _ = io.WriteString(w, "\x1d")
}

func writeClaudeAnalysisValue(
	w io.Writer,
	value interface{},
	filterBillingBlock bool,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	if filterBillingBlock && isAnthropicBillingHeaderBlock(value) {
		_, _ = io.WriteString(w, "null")
		return claudeAnalysisValueStats{
			EstimatedTokens: estimateClaudeValueTokens(value),
		}
	}

	switch typed := value.(type) {
	case nil:
		_, _ = io.WriteString(w, "null")
		return claudeAnalysisValueStats{}
	case string:
		tokens, meaningful := writeCanonicalAnalysisStringStats(w, typed, scratch)
		return claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	case []interface{}:
		return writeClaudeAnalysisSlice(w, typed, filterBillingBlock, scratch)
	case []string:
		_, _ = io.WriteString(w, "[")
		stats := claudeAnalysisValueStats{}
		for i, item := range typed {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			tokens, meaningful := writeCanonicalAnalysisStringStats(w, item, scratch)
			stats.EstimatedTokens += tokens
			stats.CacheableTokens += tokens
			stats.Meaningful = stats.Meaningful || meaningful
		}
		_, _ = io.WriteString(w, "]")
		return stats
	case []ClaudeContentBlock:
		return writeClaudeTypedAnalysisSlice(w, typed, filterBillingBlock, scratch)
	case ClaudeContentBlock:
		return writeClaudeTypedAnalysisBlockValue(w, typed, filterBillingBlock, scratch)
	case map[string]interface{}:
		return writeClaudeAnalysisMap(w, typed, filterBillingBlock, scratch)
	default:
		tokens, meaningful := writeCanonicalAnalysisJSONAndStats(w, typed, scratch)
		return claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}
}

func writeClaudeTypedAnalysisSlice(
	w io.Writer,
	blocks []ClaudeContentBlock,
	filterBillingBlocks bool,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	fullCounter := &analysisApproxTokenWriter{}
	cacheCounter := &analysisApproxTokenWriter{}
	fingerprintWriter := analysisWriterPair{
		fingerprint: w,
		token:       cacheCounter,
	}

	_, _ = fingerprintWriter.WriteString("[")
	_, _ = fullCounter.WriteString("[")
	stats := claudeAnalysisValueStats{}
	fingerprintCount := 0
	for index, block := range blocks {
		if index > 0 {
			_, _ = fullCounter.WriteString(",")
		}

		if filterBillingBlocks && isClaudeTypedBillingHeaderBlock(block) {
			writeClaudeTypedAnalysisBlock(
				analysisWriterPair{token: fullCounter},
				block,
				scratch,
			)
			continue
		}

		if fingerprintCount > 0 {
			_, _ = fingerprintWriter.WriteString(",")
		}
		child := writeClaudeTypedAnalysisBlock(
			analysisWriterPair{
				fingerprint: fingerprintWriter,
				token:       fullCounter,
			},
			block,
			scratch,
		)
		stats.ToolTokens += child.ToolTokens
		stats.Meaningful = stats.Meaningful || child.Meaningful
		fingerprintCount++
	}
	_, _ = fingerprintWriter.WriteString("]")
	_, _ = fullCounter.WriteString("]")

	stats.EstimatedTokens = fullCounter.tokens()
	if fingerprintCount > 0 {
		stats.CacheableTokens = cacheCounter.tokens()
	}
	return stats
}

func writeClaudeTypedAnalysisBlockValue(
	w io.Writer,
	block ClaudeContentBlock,
	filterBillingBlock bool,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	fullCounter := &analysisApproxTokenWriter{}
	if filterBillingBlock && isClaudeTypedBillingHeaderBlock(block) {
		_, _ = writeAnalysisWriterString(w, "null")
		writeClaudeTypedAnalysisBlock(
			analysisWriterPair{token: fullCounter},
			block,
			scratch,
		)
		return claudeAnalysisValueStats{
			EstimatedTokens: fullCounter.tokens(),
		}
	}

	cacheCounter := &analysisApproxTokenWriter{}
	child := writeClaudeTypedAnalysisBlock(
		analysisWriterPair{
			fingerprint: analysisWriterPair{
				fingerprint: w,
				token:       cacheCounter,
			},
			token: fullCounter,
		},
		block,
		scratch,
	)
	child.EstimatedTokens = fullCounter.tokens()
	child.CacheableTokens = cacheCounter.tokens()
	return child
}

func writeClaudeTypedAnalysisBlock(
	writers analysisWriterPair,
	block ClaudeContentBlock,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	writeAnalysisPairString(writers, "{")
	fieldCount := 0
	var contentStats, inputStats, nameStats, textStats, thinkingStats claudeAnalysisValueStats
	sourceMeaningful := false

	if block.Content != nil {
		writeCanonicalAnalysisObjectFieldStart(writers, "content", &fieldCount, scratch)
		contentStats = writeClaudeAnalysisValue(writers, block.Content, false, scratch)
	}
	if block.ID != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "id", &fieldCount, scratch)
		writeCanonicalAnalysisJSONString(writers, block.ID, scratch, nil)
	}
	if block.Input != nil {
		writeCanonicalAnalysisObjectFieldStart(writers, "input", &fieldCount, scratch)
		tokens, meaningful := writeCanonicalAnalysisJSONAndStats(writers, block.Input, scratch)
		inputStats = claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}
	if block.Name != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "name", &fieldCount, scratch)
		tokens, meaningful := writeCanonicalAnalysisStringStats(writers, block.Name, scratch)
		nameStats = claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}
	if block.Signature != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "signature", &fieldCount, scratch)
		writeCanonicalAnalysisJSONString(writers, block.Signature, scratch, nil)
	}
	if block.Source != nil {
		writeCanonicalAnalysisObjectFieldStart(writers, "source", &fieldCount, scratch)
		sourceMeaningful = writeClaudeImageSource(writers, block.Source, scratch)
	}
	if block.Text != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "text", &fieldCount, scratch)
		tokens, meaningful := writeCanonicalAnalysisStringStats(writers, block.Text, scratch)
		textStats = claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}
	if block.Thinking != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "thinking", &fieldCount, scratch)
		tokens, meaningful := writeCanonicalAnalysisStringStats(writers, block.Thinking, scratch)
		thinkingStats = claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}
	if block.ToolUseID != "" {
		writeCanonicalAnalysisObjectFieldStart(writers, "tool_use_id", &fieldCount, scratch)
		writeCanonicalAnalysisJSONString(writers, block.ToolUseID, scratch, nil)
	}
	writeCanonicalAnalysisObjectFieldStart(writers, "type", &fieldCount, scratch)
	writeCanonicalAnalysisJSONString(writers, block.Type, scratch, nil)
	writeAnalysisPairString(writers, "}")

	typeName := strings.ToLower(strings.TrimSpace(block.Type))
	semanticTokens := 0
	meaningful := false
	switch typeName {
	case "text":
		semanticTokens = textStats.EstimatedTokens
		meaningful = textStats.Meaningful
	case "thinking":
		semanticTokens = thinkingStats.EstimatedTokens
		meaningful = thinkingStats.Meaningful
	case "tool_use":
		semanticTokens = nameStats.EstimatedTokens + inputStats.EstimatedTokens
		meaningful = nameStats.Meaningful || inputStats.Meaningful
	case "tool_result":
		semanticTokens = contentStats.EstimatedTokens
		meaningful = contentStats.Meaningful
	default:
		semanticTokens = textStats.EstimatedTokens +
			thinkingStats.EstimatedTokens +
			contentStats.EstimatedTokens
		meaningful = textStats.Meaningful ||
			thinkingStats.Meaningful ||
			contentStats.Meaningful ||
			inputStats.Meaningful ||
			nameStats.Meaningful ||
			sourceMeaningful
	}

	toolTokens := contentStats.ToolTokens
	if typeName == "tool_use" || typeName == "tool_result" {
		toolTokens = semanticTokens
	}
	return claudeAnalysisValueStats{
		ToolTokens: toolTokens,
		Meaningful: meaningful,
	}
}

func writeCanonicalAnalysisObjectFieldStart(
	writers analysisWriterPair,
	name string,
	fieldCount *int,
	scratch *claudeAnalysisScratch,
) {
	if *fieldCount > 0 {
		writeAnalysisPairString(writers, ",")
	}
	writeCanonicalAnalysisJSONString(writers, name, scratch, nil)
	writeAnalysisPairString(writers, ":")
	*fieldCount = *fieldCount + 1
}

func writeClaudeImageSource(
	writers analysisWriterPair,
	source *ImageSource,
	scratch *claudeAnalysisScratch,
) bool {
	writeAnalysisPairString(writers, "{")
	fieldCount := 0
	writeCanonicalAnalysisObjectFieldStart(writers, "data", &fieldCount, scratch)
	writeCanonicalAnalysisJSONString(writers, source.Data, scratch, nil)
	writeCanonicalAnalysisObjectFieldStart(writers, "media_type", &fieldCount, scratch)
	writeCanonicalAnalysisJSONString(writers, source.MediaType, scratch, nil)
	writeCanonicalAnalysisObjectFieldStart(writers, "type", &fieldCount, scratch)
	writeCanonicalAnalysisJSONString(writers, source.Type, scratch, nil)
	writeAnalysisPairString(writers, "}")
	return strings.TrimSpace(source.Data) != ""
}

func isClaudeTypedBillingHeaderBlock(block ClaudeContentBlock) bool {
	typeName := strings.ToLower(strings.TrimSpace(block.Type))
	if typeName != "" && typeName != "text" {
		return false
	}
	trimmed := strings.TrimLeft(block.Text, " \t\r\n")
	return strings.HasPrefix(
		strings.ToLower(trimmed),
		"x-anthropic-billing-header:",
	)
}

func writeClaudeAnalysisSlice(
	w io.Writer,
	values []interface{},
	filterBillingBlocks bool,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	_, _ = io.WriteString(w, "[")
	stats := claudeAnalysisValueStats{}
	written := 0

	for _, value := range values {
		if filterBillingBlocks && isAnthropicBillingHeaderBlock(value) {
			stats.EstimatedTokens += estimateClaudeValueTokens(value)
			continue
		}
		if written > 0 {
			_, _ = io.WriteString(w, ",")
		}
		child := writeClaudeAnalysisValue(w, value, false, scratch)
		stats.EstimatedTokens += child.EstimatedTokens
		stats.CacheableTokens += child.CacheableTokens
		stats.ToolTokens += child.ToolTokens
		stats.Meaningful = stats.Meaningful || child.Meaningful
		written++
	}

	_, _ = io.WriteString(w, "]")
	return stats
}

func writeClaudeAnalysisMap(
	w io.Writer,
	value map[string]interface{},
	filterBillingBlock bool,
	scratch *claudeAnalysisScratch,
) claudeAnalysisValueStats {
	if filterBillingBlock && isAnthropicBillingHeaderBlock(value) {
		_, _ = io.WriteString(w, "null")
		return claudeAnalysisValueStats{
			EstimatedTokens: estimateClaudeValueTokens(value),
		}
	}

	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))
	_, hasText := value["text"]
	_, hasThinking := value["thinking"]
	_, hasContent := value["content"]
	semanticMap := typeName == "text" ||
		typeName == "thinking" ||
		typeName == "tool_use" ||
		typeName == "tool_result" ||
		hasText || hasThinking || hasContent
	if !semanticMap {
		tokens, meaningful := writeCanonicalAnalysisJSONAndStats(w, value, scratch)
		return claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
			Meaningful:      meaningful,
		}
	}

	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var textStats, thinkingStats, contentStats, nameStats, inputStats claudeAnalysisValueStats
	nestedToolTokens := 0
	genericMeaningful := false
	jsonCounter := &analysisApproxTokenWriter{}
	_, _ = io.WriteString(w, "{")
	_, _ = io.WriteString(jsonCounter, "{")
	fingerprintCount := 0
	for tokenCount, key := range keys {
		if tokenCount > 0 {
			_, _ = io.WriteString(jsonCounter, ",")
		}
		writeCanonicalAnalysisString(jsonCounter, key)
		_, _ = io.WriteString(jsonCounter, ":")

		fingerprintWriter := io.Writer(nil)
		if key != "cache_control" {
			if fingerprintCount > 0 {
				_, _ = io.WriteString(w, ",")
			}
			writeCanonicalAnalysisString(w, key)
			_, _ = io.WriteString(w, ":")
			fingerprintWriter = w
			fingerprintCount++
		}

		if key == "cache_control" {
			writeCanonicalAnalysisJSONPair(nil, jsonCounter, value[key], scratch)
			continue
		}

		childWriter := analysisWriterPair{
			fingerprint: fingerprintWriter,
			token:       jsonCounter,
		}
		var child claudeAnalysisValueStats
		if typeName == "tool_use" && key == "input" {
			tokens, meaningful := writeCanonicalAnalysisJSONAndStats(
				childWriter,
				value[key],
				scratch,
			)
			child = claudeAnalysisValueStats{
				EstimatedTokens: tokens,
				CacheableTokens: tokens,
				Meaningful:      meaningful,
			}
		} else {
			child = writeClaudeAnalysisValue(childWriter, value[key], false, scratch)
		}
		nestedToolTokens += child.ToolTokens
		if key != "type" {
			genericMeaningful = genericMeaningful || child.Meaningful
		}
		switch key {
		case "text":
			textStats = child
		case "thinking":
			thinkingStats = child
		case "content":
			contentStats = child
		case "name":
			nameStats = child
		case "input":
			inputStats = child
		}
	}
	_, _ = io.WriteString(w, "}")
	_, _ = io.WriteString(jsonCounter, "}")

	fallbackToJSON := func() (int, bool) {
		return jsonCounter.tokens(), genericMeaningful
	}
	estimated := 0
	meaningful := false
	switch typeName {
	case "text":
		if _, ok := value["text"].(string); !ok {
			estimated, meaningful = fallbackToJSON()
			break
		}
		estimated = textStats.EstimatedTokens
		meaningful = textStats.Meaningful
	case "thinking":
		if _, ok := value["thinking"].(string); !ok {
			estimated, meaningful = fallbackToJSON()
			break
		}
		estimated = thinkingStats.EstimatedTokens
		meaningful = thinkingStats.Meaningful
	case "tool_use":
		if _, ok := value["name"].(string); ok {
			estimated += nameStats.EstimatedTokens
			meaningful = meaningful || nameStats.Meaningful
		}
		if _, ok := value["input"]; ok {
			estimated += inputStats.EstimatedTokens
			meaningful = meaningful || inputStats.Meaningful
		}
		if estimated == 0 {
			estimated, meaningful = fallbackToJSON()
		}
	case "tool_result":
		if _, ok := value["content"]; !ok {
			estimated, meaningful = fallbackToJSON()
			break
		}
		estimated = contentStats.EstimatedTokens
		meaningful = contentStats.Meaningful
	default:
		if _, ok := value["text"].(string); ok {
			estimated += textStats.EstimatedTokens
			meaningful = meaningful || textStats.Meaningful
		}
		if _, ok := value["thinking"].(string); ok {
			estimated += thinkingStats.EstimatedTokens
			meaningful = meaningful || thinkingStats.Meaningful
		}
		if _, ok := value["content"]; ok {
			estimated += contentStats.EstimatedTokens
			meaningful = meaningful || contentStats.Meaningful
		}
		if estimated == 0 {
			estimated, meaningful = fallbackToJSON()
		}
	}

	toolTokens := nestedToolTokens
	if typeName == "tool_use" || typeName == "tool_result" {
		toolTokens = estimated
	}
	return claudeAnalysisValueStats{
		EstimatedTokens: estimated,
		CacheableTokens: estimated,
		ToolTokens:      toolTokens,
		Meaningful:      meaningful,
	}
}

func writeCanonicalAnalysisJSON(w io.Writer, value interface{}) {
	scratch := &claudeAnalysisScratch{}
	writeCanonicalAnalysisJSONPair(w, nil, value, scratch)
}

func writeCanonicalAnalysisJSONAndCount(
	w io.Writer,
	value interface{},
	scratch *claudeAnalysisScratch,
) int {
	tokens, _ := writeCanonicalAnalysisJSONAndStats(w, value, scratch)
	return tokens
}

func writeCanonicalAnalysisJSONAndStats(
	w io.Writer,
	value interface{},
	scratch *claudeAnalysisScratch,
) (int, bool) {
	counter := &analysisApproxTokenWriter{}
	meaningful := writeCanonicalAnalysisJSONPair(w, counter, value, scratch)
	return counter.tokens(), meaningful
}

func writeCanonicalAnalysisJSONPair(
	fingerprintWriter io.Writer,
	tokenWriter io.Writer,
	value interface{},
	scratch *claudeAnalysisScratch,
) bool {
	writers := analysisWriterPair{
		fingerprint: fingerprintWriter,
		token:       tokenWriter,
	}
	if meaningful, ok := writeCanonicalAnalysisNativeJSON(writers, value, scratch); ok {
		return meaningful
	}

	// HTTP JSON 解码后的值都会命中上面的原生分支；这里只为程序化构造的
	// map[int]、[]byte、json.Marshaler 和带特殊标签的结构体保留标准语义。
	raw, err := json.Marshal(value)
	if err != nil {
		writeAnalysisPairString(writers, "null")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var normalized interface{}
	if err := decoder.Decode(&normalized); err != nil {
		writeAnalysisPairString(writers, "null")
		return false
	}
	meaningful, _ := writeCanonicalAnalysisNativeJSON(writers, normalized, scratch)
	return meaningful
}

func writeCanonicalAnalysisNativeJSON(
	writers analysisWriterPair,
	value interface{},
	scratch *claudeAnalysisScratch,
) (bool, bool) {
	switch typed := value.(type) {
	case nil:
		writeAnalysisPairString(writers, "null")
		return false, true
	case string:
		writeCanonicalAnalysisJSONString(writers, typed, scratch, nil)
		return strings.TrimSpace(typed) != "", true
	case bool:
		if typed {
			writeAnalysisPairString(writers, "true")
		} else {
			writeAnalysisPairString(writers, "false")
		}
		return true, true
	case json.Number:
		writeAnalysisPairString(writers, typed.String())
		return true, true
	case int:
		writeAnalysisPairString(writers, strconv.FormatInt(int64(typed), 10))
		return true, true
	case int8:
		writeAnalysisPairString(writers, strconv.FormatInt(int64(typed), 10))
		return true, true
	case int16:
		writeAnalysisPairString(writers, strconv.FormatInt(int64(typed), 10))
		return true, true
	case int32:
		writeAnalysisPairString(writers, strconv.FormatInt(int64(typed), 10))
		return true, true
	case int64:
		writeAnalysisPairString(writers, strconv.FormatInt(typed, 10))
		return true, true
	case uint:
		writeAnalysisPairString(writers, strconv.FormatUint(uint64(typed), 10))
		return true, true
	case uint8:
		writeAnalysisPairString(writers, strconv.FormatUint(uint64(typed), 10))
		return true, true
	case uint16:
		writeAnalysisPairString(writers, strconv.FormatUint(uint64(typed), 10))
		return true, true
	case uint32:
		writeAnalysisPairString(writers, strconv.FormatUint(uint64(typed), 10))
		return true, true
	case uint64:
		writeAnalysisPairString(writers, strconv.FormatUint(typed, 10))
		return true, true
	case float32:
		writeCanonicalAnalysisFloat(writers, float64(typed), 32)
		return true, true
	case float64:
		writeCanonicalAnalysisFloat(writers, typed, 64)
		return true, true
	case []interface{}:
		if typed == nil {
			writeAnalysisPairString(writers, "null")
			return false, true
		}
		writeAnalysisPairString(writers, "[")
		meaningful := false
		for i, item := range typed {
			if i > 0 {
				writeAnalysisPairString(writers, ",")
			}
			childMeaningful := writeCanonicalAnalysisJSONPair(
				writers.fingerprint,
				writers.token,
				item,
				scratch,
			)
			meaningful = meaningful || childMeaningful
		}
		writeAnalysisPairString(writers, "]")
		return meaningful, true
	case []string:
		if typed == nil {
			writeAnalysisPairString(writers, "null")
			return false, true
		}
		writeAnalysisPairString(writers, "[")
		meaningful := false
		for i, item := range typed {
			if i > 0 {
				writeAnalysisPairString(writers, ",")
			}
			writeCanonicalAnalysisJSONString(writers, item, scratch, nil)
			meaningful = meaningful || strings.TrimSpace(item) != ""
		}
		writeAnalysisPairString(writers, "]")
		return meaningful, true
	case map[string]interface{}:
		if typed == nil {
			writeAnalysisPairString(writers, "null")
			return false, true
		}
		return writeCanonicalAnalysisNativeMap(writers, typed, scratch), true
	default:
		return false, false
	}
}

func writeCanonicalAnalysisNativeMap(
	writers analysisWriterPair,
	value map[string]interface{},
	scratch *claudeAnalysisScratch,
) bool {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	if writers.fingerprint != nil {
		_, _ = writeAnalysisWriterString(writers.fingerprint, "{")
	}
	if writers.token != nil {
		_, _ = writeAnalysisWriterString(writers.token, "{")
	}

	fingerprintCount := 0
	tokenCount := 0
	meaningful := false
	for _, key := range keys {
		if writers.token != nil {
			if tokenCount > 0 {
				_, _ = writeAnalysisWriterString(writers.token, ",")
			}
			writeCanonicalAnalysisJSONString(
				analysisWriterPair{fingerprint: writers.token},
				key,
				scratch,
				nil,
			)
			_, _ = writeAnalysisWriterString(writers.token, ":")
			tokenCount++
		}

		fingerprintValueWriter := writers.fingerprint
		if key == "cache_control" {
			fingerprintValueWriter = nil
		} else if writers.fingerprint != nil {
			if fingerprintCount > 0 {
				_, _ = writeAnalysisWriterString(writers.fingerprint, ",")
			}
			writeCanonicalAnalysisJSONString(
				analysisWriterPair{fingerprint: writers.fingerprint},
				key,
				scratch,
				nil,
			)
			_, _ = writeAnalysisWriterString(writers.fingerprint, ":")
			fingerprintCount++
		}

		childMeaningful := writeCanonicalAnalysisJSONPair(
			fingerprintValueWriter,
			writers.token,
			value[key],
			scratch,
		)
		if key != "cache_control" && key != "type" {
			meaningful = meaningful || childMeaningful
		}
	}
	if writers.fingerprint != nil {
		_, _ = writeAnalysisWriterString(writers.fingerprint, "}")
	}
	if writers.token != nil {
		_, _ = writeAnalysisWriterString(writers.token, "}")
	}
	return meaningful
}

func writeCanonicalAnalysisFloat(
	writers analysisWriterPair,
	value float64,
	bits int,
) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		writeAnalysisPairString(writers, "null")
		return
	}

	format := byte('f')
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	var buffer [32]byte
	encoded := strconv.AppendFloat(buffer[:0], value, format, -1, bits)
	if format == 'e' {
		n := len(encoded)
		if n >= 4 && encoded[n-4] == 'e' && encoded[n-3] == '-' &&
			encoded[n-2] == '0' {
			encoded[n-2] = encoded[n-1]
			encoded = encoded[:n-1]
		}
	}
	_, _ = writers.Write(encoded)
}

func writeCanonicalAnalysisString(w io.Writer, value string) int {
	tokens, _ := writeCanonicalAnalysisStringStats(w, value, &claudeAnalysisScratch{})
	return tokens
}

func writeCanonicalAnalysisStringStats(
	w io.Writer,
	value string,
	scratch *claudeAnalysisScratch,
) (int, bool) {
	counter := &analysisApproxTokenWriter{}
	writeCanonicalAnalysisJSONString(
		analysisWriterPair{fingerprint: w},
		value,
		scratch,
		counter,
	)
	return counter.tokens(), counter.meaningful
}

func writeCanonicalAnalysisJSONString(
	writers analysisWriterPair,
	value string,
	scratch *claudeAnalysisScratch,
	rawCounter *analysisApproxTokenWriter,
) {
	output := io.Writer(writers)
	var buffered *bufio.Writer
	if len(value) > 4*1024 {
		if scratch.stringBuffer == nil {
			scratch.stringBuffer = bufio.NewWriterSize(io.Discard, 32*1024)
		}
		scratch.stringBuffer.Reset(output)
		buffered = scratch.stringBuffer
		output = buffered
	}

	_, _ = io.WriteString(output, `"`)
	chunkStart := 0
	for index, r := range value {
		if rawCounter != nil {
			rawCounter.addRune(r)
		}

		escaped := ""
		switch r {
		case '"':
			escaped = `\"`
		case '\\':
			escaped = `\\`
		case '\b':
			escaped = `\b`
		case '\f':
			escaped = `\f`
		case '\n':
			escaped = `\n`
		case '\r':
			escaped = `\r`
		case '\t':
			escaped = `\t`
		case '<':
			escaped = `\u003c`
		case '>':
			escaped = `\u003e`
		case '&':
			escaped = `\u0026`
		case '\u2028':
			escaped = `\u2028`
		case '\u2029':
			escaped = `\u2029`
		}

		_, runeSize := utf8.DecodeRuneInString(value[index:])
		if r == '\uFFFD' && runeSize == 1 {
			escaped = `\ufffd`
		} else if r < 0x20 && escaped == "" {
			const hex = "0123456789abcdef"
			raw := []byte{'\\', 'u', '0', '0', hex[byte(r)>>4], hex[byte(r)&0x0f]}
			if chunkStart < index {
				_, _ = io.WriteString(output, value[chunkStart:index])
			}
			_, _ = output.Write(raw)
			chunkStart = index + runeSize
			continue
		}

		if escaped == "" {
			continue
		}
		if chunkStart < index {
			_, _ = io.WriteString(output, value[chunkStart:index])
		}
		_, _ = io.WriteString(output, escaped)
		chunkStart = index + runeSize
	}

	if chunkStart < len(value) {
		_, _ = io.WriteString(output, value[chunkStart:])
	}
	_, _ = io.WriteString(output, `"`)
	if buffered != nil {
		_ = buffered.Flush()
		buffered.Reset(io.Discard)
	}
}

func writeAnalysisPairString(writers analysisWriterPair, value string) {
	if writers.fingerprint != nil {
		_, _ = io.WriteString(writers.fingerprint, value)
	}
	if writers.token != nil {
		_, _ = io.WriteString(writers.token, value)
	}
}

type analysisApproxTokenWriter struct {
	regularASCII int
	digits       int
	symbols      int
	nonASCII     int
	length       int
	meaningful   bool
	pending      [utf8.UTFMax]byte
	pendingLen   int
}

func (w *analysisApproxTokenWriter) Write(data []byte) (int, error) {
	written := len(data)

	if w.pendingLen > 0 {
		for len(data) > 0 && !utf8.FullRune(w.pending[:w.pendingLen]) {
			w.pending[w.pendingLen] = data[0]
			w.pendingLen++
			data = data[1:]
		}
		if utf8.FullRune(w.pending[:w.pendingLen]) {
			r, _ := utf8.DecodeRune(w.pending[:w.pendingLen])
			w.addRune(r)
			w.pendingLen = 0
		}
	}

	for len(data) > 0 {
		if data[0] < utf8.RuneSelf {
			w.addRune(rune(data[0]))
			data = data[1:]
			continue
		}
		if !utf8.FullRune(data) {
			w.pendingLen = copy(w.pending[:], data)
			break
		}
		r, size := utf8.DecodeRune(data)
		w.addRune(r)
		data = data[size:]
	}
	return written, nil
}

func (w *analysisApproxTokenWriter) WriteString(value string) (int, error) {
	written := len(value)

	if w.pendingLen > 0 {
		for len(value) > 0 && !utf8.FullRune(w.pending[:w.pendingLen]) {
			w.pending[w.pendingLen] = value[0]
			w.pendingLen++
			value = value[1:]
		}
		if utf8.FullRune(w.pending[:w.pendingLen]) {
			r, _ := utf8.DecodeRune(w.pending[:w.pendingLen])
			w.addRune(r)
			w.pendingLen = 0
		}
	}

	for len(value) > 0 {
		if value[0] < utf8.RuneSelf {
			w.addRune(rune(value[0]))
			value = value[1:]
			continue
		}
		if !utf8.FullRuneInString(value) {
			w.pendingLen = copy(w.pending[:], value)
			break
		}
		r, size := utf8.DecodeRuneInString(value)
		w.addRune(r)
		value = value[size:]
	}
	return written, nil
}

func (w *analysisApproxTokenWriter) addRune(r rune) {
	w.length++
	if !unicode.IsSpace(r) {
		w.meaningful = true
	}
	switch {
	case r >= 0x80:
		w.nonASCII++
	case r >= '0' && r <= '9':
		w.digits++
	case (r >= '!' && r <= '/') || (r >= ':' && r <= '@') ||
		(r >= '[' && r <= '`') || (r >= '{' && r <= '~'):
		w.symbols++
	default:
		w.regularASCII++
	}
}

func (w *analysisApproxTokenWriter) tokens() int {
	if w.length == 0 {
		return 0
	}
	if w.length < 5 {
		return maxInt(1, int(math.Ceil(float64(w.length)/3)))
	}
	estimated := int(math.Ceil(
		float64(w.regularASCII)/4.5 +
			float64(w.digits)/2 +
			float64(w.symbols)/1.5 +
			float64(w.nonASCII)/1.5,
	))
	return maxInt(estimated, 1)
}
