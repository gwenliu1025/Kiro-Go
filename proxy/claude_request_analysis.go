package proxy

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
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
}

func analyzeClaudeRequest(req *ClaudeRequest, callerScope string) claudeRequestAnalysis {
	if req == nil {
		return claudeRequestAnalysis{}
	}

	callerScope = strings.TrimSpace(callerScope)
	if callerScope == "" {
		callerScope = anonymousCallerScope
	}

	taskHasher := sha256.New()
	requestHasher := sha256.New()
	commonHasher := io.MultiWriter(taskHasher, requestHasher)

	writeAnalysisString(taskHasher, "caller_scope", callerScope)
	model := strings.ToLower(strings.TrimSpace(req.Model))
	writeAnalysisStringTo(commonHasher, "model", model)

	analysis := claudeRequestAnalysis{
		Prefixes: make([]claudePrefixPoint, 0, len(req.Messages)),
	}

	for _, tool := range req.Tools {
		writeAnalysisFieldStart(commonHasher, "tool")

		nameTokens := writeCanonicalAnalysisString(commonHasher, tool.Name)
		descriptionTokens := writeCanonicalAnalysisString(commonHasher, tool.Description)
		schemaTokens := writeCanonicalAnalysisJSONAndCount(commonHasher, tool.InputSchema)

		writeAnalysisFieldEnd(commonHasher)
		toolTotal := nameTokens + descriptionTokens + schemaTokens
		analysis.EstimatedInputTokens += toolTotal
		analysis.CacheableTokens += toolTotal
	}

	writeAnalysisFieldStart(commonHasher, "system")
	systemStats := writeClaudeAnalysisValue(commonHasher, req.System)
	writeAnalysisFieldEnd(commonHasher)
	analysis.EstimatedInputTokens += systemStats.EstimatedTokens
	analysis.CacheableTokens += systemStats.CacheableTokens
	analysis.ToolTokens += systemStats.ToolTokens

	firstUserWritten := false
	for _, message := range req.Messages {
		writeAnalysisFieldStart(requestHasher, "message")
		writeAnalysisStringTo(requestHasher, "role", strings.ToLower(strings.TrimSpace(message.Role)))

		writeAnalysisFieldStart(requestHasher, "content")
		valueWriter := io.Writer(requestHasher)
		writeFirstUser := !firstUserWritten &&
			strings.EqualFold(strings.TrimSpace(message.Role), "user") &&
			hasMeaningfulClaudeAnalysisValue(message.Content)
		if writeFirstUser {
			writeAnalysisFieldStart(taskHasher, "first_user")
			valueWriter = io.MultiWriter(requestHasher, taskHasher)
		}

		messageStats := writeClaudeAnalysisValue(valueWriter, message.Content)

		if writeFirstUser {
			writeAnalysisFieldEnd(taskHasher)
			firstUserWritten = true
		}
		writeAnalysisFieldEnd(requestHasher)
		writeAnalysisFieldEnd(requestHasher)

		analysis.EstimatedInputTokens += messageStats.EstimatedTokens
		analysis.CacheableTokens += messageStats.CacheableTokens
		analysis.ToolTokens += messageStats.ToolTokens

		var prefixFingerprint [32]byte
		copy(prefixFingerprint[:], requestHasher.Sum(nil))
		analysis.Prefixes = append(analysis.Prefixes, claudePrefixPoint{
			Fingerprint:      prefixFingerprint,
			CumulativeTokens: analysis.CacheableTokens,
		})
	}

	copy(analysis.TaskKey[:], taskHasher.Sum(nil))
	copy(analysis.RequestFingerprint[:], requestHasher.Sum(nil))
	return analysis
}

func writeAnalysisString(h hash.Hash, kind, value string) {
	writeAnalysisStringTo(h, kind, value)
}

func writeAnalysisStringTo(w io.Writer, kind, value string) {
	writeAnalysisFieldStart(w, kind)
	writeCanonicalAnalysisString(w, value)
	writeAnalysisFieldEnd(w)
}

func writeAnalysisValue(h hash.Hash, kind string, value interface{}) {
	writeAnalysisFieldStart(h, kind)
	writeCanonicalAnalysisJSON(h, value)
	writeAnalysisFieldEnd(h)
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

func writeClaudeAnalysisValue(w io.Writer, value interface{}) claudeAnalysisValueStats {
	switch typed := value.(type) {
	case nil:
		_, _ = io.WriteString(w, "null")
		return claudeAnalysisValueStats{}
	case string:
		tokens := writeCanonicalAnalysisString(w, typed)
		return claudeAnalysisValueStats{
			EstimatedTokens: tokens,
			CacheableTokens: tokens,
		}
	case []interface{}:
		return writeClaudeAnalysisSlice(w, typed)
	case []string:
		items := make([]interface{}, len(typed))
		for i := range typed {
			items[i] = typed[i]
		}
		return writeClaudeAnalysisSlice(w, items)
	case map[string]interface{}:
		return writeClaudeAnalysisMap(w, typed)
	default:
		normalized, ok := normalizeAnalysisValue(typed)
		if !ok {
			_, _ = io.WriteString(w, "null")
			return claudeAnalysisValueStats{}
		}
		return writeClaudeAnalysisValue(w, normalized)
	}
}

func writeClaudeAnalysisSlice(w io.Writer, values []interface{}) claudeAnalysisValueStats {
	_, _ = io.WriteString(w, "[")
	stats := claudeAnalysisValueStats{}
	written := 0

	for _, value := range values {
		if isAnthropicBillingHeaderBlock(value) {
			stats.EstimatedTokens += estimateClaudeValueTokens(value)
			continue
		}
		if written > 0 {
			_, _ = io.WriteString(w, ",")
		}
		child := writeClaudeAnalysisValue(w, value)
		stats.EstimatedTokens += child.EstimatedTokens
		stats.CacheableTokens += child.CacheableTokens
		stats.ToolTokens += child.ToolTokens
		written++
	}

	_, _ = io.WriteString(w, "]")
	return stats
}

func writeClaudeAnalysisMap(w io.Writer, value map[string]interface{}) claudeAnalysisValueStats {
	if isAnthropicBillingHeaderBlock(value) {
		_, _ = io.WriteString(w, "null")
		return claudeAnalysisValueStats{
			EstimatedTokens: estimateClaudeValueTokens(value),
		}
	}

	jsonCounter := &analysisApproxTokenWriter{}
	output := io.MultiWriter(w, jsonCounter)
	typeName, _ := value["type"].(string)
	typeName = strings.ToLower(strings.TrimSpace(typeName))

	keys := make([]string, 0, len(value))
	for key := range value {
		if key == "cache_control" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	_, _ = io.WriteString(output, "{")
	childStats := make(map[string]claudeAnalysisValueStats, len(keys))
	for i, key := range keys {
		if i > 0 {
			_, _ = io.WriteString(output, ",")
		}
		writeCanonicalAnalysisString(output, key)
		_, _ = io.WriteString(output, ":")

		if typeName == "tool_use" && key == "input" {
			tokens := writeCanonicalAnalysisJSONAndCount(output, value[key])
			childStats[key] = claudeAnalysisValueStats{
				EstimatedTokens: tokens,
				CacheableTokens: tokens,
			}
			continue
		}
		childStats[key] = writeClaudeAnalysisValue(output, value[key])
	}
	_, _ = io.WriteString(output, "}")

	estimated := 0
	switch typeName {
	case "text":
		estimated = childStats["text"].EstimatedTokens
	case "thinking":
		estimated = childStats["thinking"].EstimatedTokens
	case "tool_use":
		estimated = childStats["name"].EstimatedTokens + childStats["input"].EstimatedTokens
	case "tool_result":
		estimated = childStats["content"].EstimatedTokens
	default:
		estimated = childStats["text"].EstimatedTokens +
			childStats["thinking"].EstimatedTokens +
			childStats["content"].EstimatedTokens
		if estimated == 0 {
			estimated = jsonCounter.tokens()
		}
	}

	toolTokens := 0
	if typeName == "tool_use" || typeName == "tool_result" {
		toolTokens = estimated
	} else {
		for _, child := range childStats {
			toolTokens += child.ToolTokens
		}
	}

	return claudeAnalysisValueStats{
		EstimatedTokens: estimated,
		CacheableTokens: estimated,
		ToolTokens:      toolTokens,
	}
}

func writeCanonicalAnalysisJSON(w io.Writer, value interface{}) {
	switch typed := value.(type) {
	case nil:
		_, _ = io.WriteString(w, "null")
	case string:
		writeCanonicalAnalysisString(w, typed)
	case bool:
		if typed {
			_, _ = io.WriteString(w, "true")
		} else {
			_, _ = io.WriteString(w, "false")
		}
	case json.Number:
		_, _ = io.WriteString(w, typed.String())
	case float64:
		_, _ = io.WriteString(w, strconv.FormatFloat(typed, 'g', -1, 64))
	case float32:
		_, _ = io.WriteString(w, strconv.FormatFloat(float64(typed), 'g', -1, 32))
	case int:
		_, _ = io.WriteString(w, strconv.Itoa(typed))
	case int8:
		_, _ = io.WriteString(w, strconv.FormatInt(int64(typed), 10))
	case int16:
		_, _ = io.WriteString(w, strconv.FormatInt(int64(typed), 10))
	case int32:
		_, _ = io.WriteString(w, strconv.FormatInt(int64(typed), 10))
	case int64:
		_, _ = io.WriteString(w, strconv.FormatInt(typed, 10))
	case uint:
		_, _ = io.WriteString(w, strconv.FormatUint(uint64(typed), 10))
	case uint8:
		_, _ = io.WriteString(w, strconv.FormatUint(uint64(typed), 10))
	case uint16:
		_, _ = io.WriteString(w, strconv.FormatUint(uint64(typed), 10))
	case uint32:
		_, _ = io.WriteString(w, strconv.FormatUint(uint64(typed), 10))
	case uint64:
		_, _ = io.WriteString(w, strconv.FormatUint(typed, 10))
	case []interface{}:
		_, _ = io.WriteString(w, "[")
		written := 0
		for _, item := range typed {
			if isAnthropicBillingHeaderBlock(item) {
				continue
			}
			if written > 0 {
				_, _ = io.WriteString(w, ",")
			}
			writeCanonicalAnalysisJSON(w, item)
			written++
		}
		_, _ = io.WriteString(w, "]")
	case []string:
		_, _ = io.WriteString(w, "[")
		for i, item := range typed {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			writeCanonicalAnalysisString(w, item)
		}
		_, _ = io.WriteString(w, "]")
	case map[string]interface{}:
		if isAnthropicBillingHeaderBlock(typed) {
			_, _ = io.WriteString(w, "null")
			return
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		_, _ = io.WriteString(w, "{")
		for i, key := range keys {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			writeCanonicalAnalysisString(w, key)
			_, _ = io.WriteString(w, ":")
			writeCanonicalAnalysisJSON(w, typed[key])
		}
		_, _ = io.WriteString(w, "}")
	default:
		normalized, ok := normalizeAnalysisValue(typed)
		if !ok {
			_, _ = io.WriteString(w, "null")
			return
		}
		writeCanonicalAnalysisJSON(w, normalized)
	}
}

func writeCanonicalAnalysisJSONAndCount(w io.Writer, value interface{}) int {
	counter := &analysisApproxTokenWriter{}
	writeCanonicalAnalysisJSON(io.MultiWriter(w, counter), value)
	return counter.tokens()
}

func writeCanonicalAnalysisString(w io.Writer, value string) int {
	output := w
	var buffered *bufio.Writer
	if len(value) > 4*1024 {
		buffered = bufio.NewWriterSize(w, 32*1024)
		output = buffered
	}

	_, _ = io.WriteString(output, `"`)
	counter := analysisApproxTokenWriter{}
	chunkStart := 0

	for index, r := range value {
		counter.addRune(r)

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
	}
	return counter.tokens()
}

type analysisApproxTokenWriter struct {
	regularASCII int
	digits       int
	symbols      int
	nonASCII     int
	length       int
}

func (w *analysisApproxTokenWriter) Write(p []byte) (int, error) {
	for _, r := range string(p) {
		w.addRune(r)
	}
	return len(p), nil
}

func (w *analysisApproxTokenWriter) addRune(r rune) {
	w.length++
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

func normalizeAnalysisValue(value interface{}) (interface{}, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}

	var normalized interface{}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return nil, false
	}
	return normalized, true
}

func hasMeaningfulClaudeAnalysisValue(value interface{}) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return len(typed) > 0
	case []interface{}:
		for _, item := range typed {
			if isAnthropicBillingHeaderBlock(item) {
				continue
			}
			if hasMeaningfulClaudeAnalysisValue(item) {
				return true
			}
		}
		return false
	case []string:
		for _, item := range typed {
			if item != "" {
				return true
			}
		}
		return false
	case map[string]interface{}:
		return !isAnthropicBillingHeaderBlock(typed) && len(typed) > 0
	default:
		rv := reflect.ValueOf(value)
		switch rv.Kind() {
		case reflect.Invalid:
			return false
		case reflect.Pointer, reflect.Interface:
			return !rv.IsNil() && hasMeaningfulClaudeAnalysisValue(rv.Elem().Interface())
		case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
			return rv.Len() > 0
		default:
			return true
		}
	}
}
