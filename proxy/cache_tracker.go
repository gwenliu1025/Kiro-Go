package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPromptCacheTTL = 5 * time.Minute
const promptCacheAlgorithmVersion = "native-high-cache-v1"
const promptCacheShardCount = 32
const promptCacheRetention = 70 * time.Minute

type promptCachePhase uint8

const (
	promptCachePhaseFirst promptCachePhase = iota
	promptCachePhaseContinue
	promptCachePhaseRebuild
)

type promptCacheTTL uint8

const (
	promptCacheTTL5m promptCacheTTL = iota + 1
	promptCacheTTL1h
)

type promptCacheSnapshot struct {
	TaskKey             [32]byte
	RequestFingerprint  [32]byte
	TTL                 promptCacheTTL
	Phase               promptCachePhase
	MatchedPrefixTokens int
	CurrentCacheable    int
	SuccessfulRounds    int
	AgeRatio            float64
	ExistingUsage       *ClaudeUsage
}

type promptCacheCommit struct {
	TaskKey            [32]byte
	RequestFingerprint [32]byte
	TTL                promptCacheTTL
	Prefixes           []claudePrefixPoint
	Usage              ClaudeUsage
	SuccessfulAt       time.Time
}

type promptCacheUsageRecord struct {
	Usage        ClaudeUsage
	SuccessfulAt time.Time
}

type promptCacheTaskState struct {
	TTL              promptCacheTTL
	SuccessfulRounds int
	LastSuccessfulAt time.Time
	LastActivityAt   time.Time
	LastCreationAt   time.Time
	Prefixes         map[[32]byte]int
	Usages           map[[32]byte]promptCacheUsageRecord
}

type promptCacheShard struct {
	mu    sync.Mutex
	tasks map[[32]byte]*promptCacheTaskState
}

type promptCacheBreakpoint struct {
	Fingerprint      [32]byte
	CumulativeTokens int
	TTL              time.Duration
}

type promptCacheProfile struct {
	Breakpoints      []promptCacheBreakpoint
	TotalInputTokens int
	Model            string
}

type promptCacheTracker struct {
	shards [promptCacheShardCount]promptCacheShard
}

func newPromptCacheTracker(_ time.Duration) *promptCacheTracker {
	tracker := &promptCacheTracker{}
	for i := range tracker.shards {
		tracker.shards[i].tasks = make(map[[32]byte]*promptCacheTaskState)
	}
	return tracker
}

func ttlForTask(taskKey [32]byte) promptCacheTTL {
	hasher := sha256.New()
	_, _ = hasher.Write(taskKey[:])
	_, _ = hasher.Write([]byte(promptCacheAlgorithmVersion))
	sum := hasher.Sum(nil)
	if binary.BigEndian.Uint16(sum[:2])%100 < 20 {
		return promptCacheTTL5m
	}
	return promptCacheTTL1h
}

func (t *promptCacheTracker) Snapshot(analysis claudeRequestAnalysis, now time.Time) promptCacheSnapshot {
	snapshot := promptCacheSnapshot{
		TaskKey:            analysis.TaskKey,
		RequestFingerprint: analysis.RequestFingerprint,
		TTL:                ttlForTask(analysis.TaskKey),
		Phase:              promptCachePhaseFirst,
		CurrentCacheable:   analysis.CacheableTokens,
	}
	if t == nil {
		return snapshot
	}

	shard := t.shardForTask(analysis.TaskKey)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	t.pruneShardLocked(shard, now)

	state := shard.tasks[analysis.TaskKey]
	if state == nil {
		return snapshot
	}

	snapshot.TTL = state.TTL
	snapshot.SuccessfulRounds = state.SuccessfulRounds
	active, ageRatio := promptCacheTaskActive(state, now)
	snapshot.AgeRatio = ageRatio
	if !active {
		snapshot.Phase = promptCachePhaseRebuild
		return snapshot
	}

	if record, ok := state.Usages[analysis.RequestFingerprint]; ok {
		usage := cloneClaudeUsage(record.Usage)
		snapshot.ExistingUsage = &usage
		snapshot.Phase = promptCachePhaseContinue
		return snapshot
	}

	for _, prefix := range analysis.Prefixes {
		storedTokens, ok := state.Prefixes[prefix.Fingerprint]
		if !ok {
			continue
		}
		matched := minInt(storedTokens, prefix.CumulativeTokens)
		if matched > snapshot.MatchedPrefixTokens {
			snapshot.MatchedPrefixTokens = matched
		}
	}

	if snapshot.MatchedPrefixTokens > 0 {
		snapshot.Phase = promptCachePhaseContinue
	}
	return snapshot
}

func (t *promptCacheTracker) Commit(commit promptCacheCommit) {
	if t == nil || commit.SuccessfulAt.IsZero() {
		return
	}

	stableTTL := ttlForTask(commit.TaskKey)
	shard := t.shardForTask(commit.TaskKey)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	t.pruneShardLocked(shard, commit.SuccessfulAt)

	state := shard.tasks[commit.TaskKey]
	if state != nil {
		active, _ := promptCacheTaskActive(state, commit.SuccessfulAt)
		if !active {
			delete(shard.tasks, commit.TaskKey)
			state = nil
		}
	}
	if state == nil {
		state = &promptCacheTaskState{
			TTL:      stableTTL,
			Prefixes: make(map[[32]byte]int, len(commit.Prefixes)),
			Usages:   make(map[[32]byte]promptCacheUsageRecord),
		}
		shard.tasks[commit.TaskKey] = state
	}

	if record, exists := state.Usages[commit.RequestFingerprint]; exists {
		if commit.SuccessfulAt.After(state.LastSuccessfulAt) {
			state.LastSuccessfulAt = commit.SuccessfulAt
		}
		if state.TTL == promptCacheTTL5m &&
			commit.SuccessfulAt.After(state.LastActivityAt) {
			state.LastActivityAt = commit.SuccessfulAt
		}
		if commit.SuccessfulAt.After(record.SuccessfulAt) {
			record.SuccessfulAt = commit.SuccessfulAt
			state.Usages[commit.RequestFingerprint] = record
		}
		return
	}

	state.TTL = stableTTL
	state.SuccessfulRounds++
	state.LastSuccessfulAt = commit.SuccessfulAt
	state.LastActivityAt = commit.SuccessfulAt
	if commit.Usage.CacheCreationInputTokens > 0 {
		state.LastCreationAt = commit.SuccessfulAt
	}
	if state.LastCreationAt.IsZero() {
		state.LastCreationAt = commit.SuccessfulAt
	}

	for _, prefix := range commit.Prefixes {
		if existing := state.Prefixes[prefix.Fingerprint]; prefix.CumulativeTokens > existing {
			state.Prefixes[prefix.Fingerprint] = prefix.CumulativeTokens
		}
	}
	state.Usages[commit.RequestFingerprint] = promptCacheUsageRecord{
		Usage:        cloneClaudeUsage(commit.Usage),
		SuccessfulAt: commit.SuccessfulAt,
	}
}

func (t *promptCacheTracker) shardForTask(taskKey [32]byte) *promptCacheShard {
	index := binary.BigEndian.Uint32(taskKey[:4]) % promptCacheShardCount
	return &t.shards[index]
}

func (t *promptCacheTracker) pruneShardLocked(shard *promptCacheShard, now time.Time) {
	for taskKey, state := range shard.tasks {
		if now.Sub(state.LastSuccessfulAt) > promptCacheRetention {
			delete(shard.tasks, taskKey)
		}
	}
}

func (t *promptCacheTracker) taskCount() int {
	if t == nil {
		return 0
	}
	total := 0
	for i := range t.shards {
		shard := &t.shards[i]
		shard.mu.Lock()
		total += len(shard.tasks)
		shard.mu.Unlock()
	}
	return total
}

func promptCacheTaskActive(state *promptCacheTaskState, now time.Time) (bool, float64) {
	if state == nil {
		return false, 1
	}

	var base time.Time
	var ttl time.Duration
	switch state.TTL {
	case promptCacheTTL5m:
		base = state.LastActivityAt
		ttl = 5 * time.Minute
	case promptCacheTTL1h:
		base = state.LastCreationAt
		ttl = time.Hour
	default:
		return false, 1
	}
	if base.IsZero() {
		return false, 1
	}

	elapsed := now.Sub(base)
	if elapsed < 0 {
		elapsed = 0
	}
	ageRatio := float64(elapsed) / float64(ttl)
	if ageRatio > 1 {
		ageRatio = 1
	}
	return elapsed <= ttl, ageRatio
}

func cloneClaudeUsage(usage ClaudeUsage) ClaudeUsage {
	return usage
}

func (t *promptCacheTracker) BuildClaudeProfile(req *ClaudeRequest, totalInputTokens int) *promptCacheProfile {
	blocks := flattenClaudeCacheBlocks(req)
	if len(blocks) == 0 {
		return nil
	}

	hasher := sha256.New()
	breakpoints := make([]promptCacheBreakpoint, 0)
	cumulativeTokens := 0
	var activeTTL time.Duration

	for _, block := range blocks {
		canonical := canonicalizeCacheValue(block.Value)
		writeHashChunk(hasher, canonical)
		cumulativeTokens += block.Tokens

		// Determine whether this block acts as a cache breakpoint:
		//   1) Explicit cache_control on the block itself.
		//   2) Once any explicit breakpoint has been seen, every message-end
		//      boundary becomes an implicit breakpoint so that multi-turn
		//      conversations can hit earlier stored prefixes.
		breakpointTTL := time.Duration(0)
		if block.TTL > 0 {
			breakpointTTL = block.TTL
			activeTTL = block.TTL
		} else if block.IsMessageEnd && activeTTL > 0 {
			breakpointTTL = activeTTL
		}

		if breakpointTTL <= 0 {
			continue
		}

		var fingerprint [32]byte
		copy(fingerprint[:], hasher.Sum(nil))
		breakpoints = append(breakpoints, promptCacheBreakpoint{
			Fingerprint:      fingerprint,
			CumulativeTokens: cumulativeTokens,
			TTL:              breakpointTTL,
		})
	}

	if len(breakpoints) == 0 {
		return nil
	}

	if totalInputTokens < cumulativeTokens {
		totalInputTokens = cumulativeTokens
	}

	return &promptCacheProfile{
		Breakpoints:      breakpoints,
		TotalInputTokens: totalInputTokens,
		Model:            req.Model,
	}
}

type cacheablePromptBlock struct {
	Value        interface{}
	Tokens       int
	TTL          time.Duration
	IsMessageEnd bool
}

func flattenClaudeCacheBlocks(req *ClaudeRequest) []cacheablePromptBlock {
	blocks := make([]cacheablePromptBlock, 0)
	blocks = append(blocks, buildCachePreludeBlock(req))

	for toolIndex, tool := range req.Tools {
		toolValue := map[string]interface{}{
			"kind":         "tool",
			"tool_index":   toolIndex,
			"name":         tool.Name,
			"description":  tool.Description,
			"input_schema": tool.InputSchema,
		}
		fingerprintValue := stripCachePositionKeys(toolValue)
		blocks = append(blocks, cacheablePromptBlock{
			Value:  fingerprintValue,
			Tokens: estimateApproxTokens(canonicalizeCacheValue(fingerprintValue)),
			TTL:    normalizePromptCacheTTL(extractPromptCacheTTL(tool)),
		})
	}

	appendSystemCacheBlocks(&blocks, req.System)

	for messageIndex, msg := range req.Messages {
		appendMessageCacheBlocks(&blocks, messageIndex, msg)
	}

	return blocks
}

func buildCachePreludeBlock(req *ClaudeRequest) cacheablePromptBlock {
	prelude := map[string]interface{}{
		"kind":        "request_prelude",
		"model":       req.Model,
		"tool_choice": req.ToolChoice,
	}
	return cacheablePromptBlock{
		Value:  prelude,
		Tokens: estimateApproxTokens(canonicalizeCacheValue(prelude)),
	}
}

func appendSystemCacheBlocks(blocks *[]cacheablePromptBlock, system interface{}) {
	switch v := system.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":         "system",
			"system_index": 0,
			"block": map[string]interface{}{
				"type": "text",
				"text": v,
			},
		}, false)
	case []interface{}:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block":        block,
			}, false)
		}
	case []string:
		for i, block := range v {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":         "system",
				"system_index": i,
				"block": map[string]interface{}{
					"type": "text",
					"text": block,
				},
			}, false)
		}
	}
}

func appendMessageCacheBlocks(blocks *[]cacheablePromptBlock, messageIndex int, msg ClaudeMessage) {
	role := msg.Role
	switch content := msg.Content.(type) {
	case string:
		appendPromptBlock(blocks, map[string]interface{}{
			"kind":          "message",
			"message_index": messageIndex,
			"role":          role,
			"block_index":   0,
			"block": map[string]interface{}{
				"type": "text",
				"text": content,
			},
		}, true)
	case []interface{}:
		lastIdx := len(content) - 1
		for blockIndex, block := range content {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   blockIndex,
				"block":         block,
			}, blockIndex == lastIdx)
		}
	default:
		if content != nil {
			appendPromptBlock(blocks, map[string]interface{}{
				"kind":          "message",
				"message_index": messageIndex,
				"role":          role,
				"block_index":   0,
				"block":         content,
			}, true)
		}
	}
}

func appendPromptBlock(blocks *[]cacheablePromptBlock, wrapper map[string]interface{}, isMessageEnd bool) {
	blockValue := wrapper["block"]
	ttl := normalizePromptCacheTTL(extractPromptCacheTTL(blockValue))

	// Drop volatile billing metadata from the cache fingerprint. Claude Code's
	// x-anthropic-billing-header can drift, appear, or disappear across
	// otherwise identical requests, and it does not change model semantics.
	if isAnthropicBillingHeaderBlock(blockValue) {
		return
	}

	fingerprintValue := stripCachePositionKeys(wrapper)
	canonical := canonicalizeCacheValue(fingerprintValue)
	*blocks = append(*blocks, cacheablePromptBlock{
		Value:        fingerprintValue,
		Tokens:       estimateApproxTokens(canonical),
		TTL:          ttl,
		IsMessageEnd: isMessageEnd,
	})
}

func stripCachePositionKeys(value map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(value))
	for key, item := range value {
		if isCachePositionKey(key) {
			continue
		}
		cloned[key] = item
	}
	return cloned
}

func isAnthropicBillingHeaderBlock(value interface{}) bool {
	blockMap, ok := value.(map[string]interface{})
	if !ok {
		return false
	}

	// Only normalize text blocks (or blocks without an explicit type but containing text).
	if t, ok := blockMap["type"].(string); ok && t != "" && t != "text" {
		return false
	}

	text, ok := blockMap["text"].(string)
	if !ok {
		return false
	}

	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(strings.ToLower(trimmed), "x-anthropic-billing-header:")
}

func extractPromptCacheTTL(value interface{}) time.Duration {
	block, ok := value.(map[string]interface{})
	if !ok {
		if raw, err := json.Marshal(value); err == nil {
			var decoded map[string]interface{}
			if json.Unmarshal(raw, &decoded) == nil {
				block = decoded
				ok = true
			}
		}
	}
	if !ok {
		return 0
	}

	rawCache, ok := block["cache_control"]
	if !ok {
		return 0
	}
	cacheControl, ok := rawCache.(map[string]interface{})
	if !ok {
		return 0
	}
	cacheType, _ := cacheControl["type"].(string)
	if !strings.EqualFold(cacheType, "ephemeral") {
		return 0
	}

	if ttl, ok := parsePromptCacheTTLValue(cacheControl["ttl"]); ok {
		return ttl
	}
	return defaultPromptCacheTTL
}

func parsePromptCacheTTLValue(value interface{}) (time.Duration, bool) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		if trimmed == "" {
			return 0, false
		}
		if d, err := time.ParseDuration(trimmed); err == nil {
			return d, true
		}
		if seconds, err := strconv.Atoi(trimmed); err == nil {
			return time.Duration(seconds) * time.Second, true
		}
	case float64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	case int64:
		if v > 0 {
			return time.Duration(v) * time.Second, true
		}
	}
	return 0, false
}

func normalizePromptCacheTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 0
	}
	if ttl > time.Hour {
		return time.Hour
	}
	if ttl > defaultPromptCacheTTL {
		return time.Hour
	}
	return defaultPromptCacheTTL
}

func canonicalizeCacheValue(value interface{}) string {
	var buf bytes.Buffer
	writeCanonicalJSON(&buf, value)
	return buf.String()
}

func writeCanonicalJSON(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case string:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	case []interface{}:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalJSON(buf, item)
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		keys := make([]string, 0, len(v))
		for key := range v {
			if key == "cache_control" {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for i, key := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			buf.Write(encoded)
			buf.WriteByte(':')
			writeCanonicalJSON(buf, v[key])
		}
		buf.WriteByte('}')
	default:
		encoded, _ := json.Marshal(v)
		buf.Write(encoded)
	}
}

func isCachePositionKey(key string) bool {
	switch key {
	case "tool_index", "system_index", "message_index", "block_index":
		return true
	default:
		return false
	}
}

func writeHashChunk(hasher hashWriter, chunk string) {
	length := strconv.Itoa(len(chunk))
	hasher.Write([]byte(length))
	hasher.Write([]byte{0})
	hasher.Write([]byte(chunk))
	hasher.Write([]byte{0})
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
