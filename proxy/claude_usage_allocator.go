package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
)

const (
	maxClaudeUsageCandidates      = 64
	firstRoundReadJitterThreshold = -0.60
	minClaudeReadCreateRatio      = 2.0
	maxClaudeReadCreateRatio      = 5.0
)

type claudeUsageFeatures struct {
	Phase        promptCachePhase
	ReuseRatio   float64
	GrowthRatio  float64
	AgeRatio     float64
	RoundFactor  float64
	SizeFactor   float64
	ToolRatio    float64
	StableJitter float64
}

type claudeUsageTargets struct {
	InputShare  float64
	ReadShare   float64
	CreateShare float64
}

func buildClaudeUsageFeatures(
	snapshot promptCacheSnapshot,
	analysis claudeRequestAnalysis,
	rawInputTokens int,
) claudeUsageFeatures {
	currentCacheable := maxInt(snapshot.CurrentCacheable, 0)
	matchedPrefix := minInt(maxInt(snapshot.MatchedPrefixTokens, 0), currentCacheable)

	reuseRatio := 0.0
	growthRatio := 0.0
	toolRatio := 0.0
	if currentCacheable > 0 {
		reuseRatio = float64(matchedPrefix) / float64(currentCacheable)
		growthRatio = float64(currentCacheable-matchedPrefix) / float64(currentCacheable)
		toolRatio = float64(maxInt(analysis.ToolTokens, 0)) / float64(currentCacheable)
	}

	return claudeUsageFeatures{
		Phase:        snapshot.Phase,
		ReuseRatio:   clampFloat(reuseRatio, 0, 1),
		GrowthRatio:  clampFloat(growthRatio, 0, 1),
		AgeRatio:     clampFloat(snapshot.AgeRatio, 0, 1),
		RoundFactor:  clampFloat(math.Log2(1+float64(maxInt(snapshot.SuccessfulRounds, 0)))/4, 0, 1),
		SizeFactor:   clampFloat(math.Log2(1+float64(maxInt(rawInputTokens, 0)))/20, 0, 1),
		ToolRatio:    clampFloat(toolRatio, 0, 1),
		StableJitter: stableClaudeUsageJitter(snapshot.TaskKey, snapshot.RequestFingerprint),
	}
}

func stableClaudeUsageJitter(taskKey, requestFingerprint [32]byte) float64 {
	hasher := sha256.New()
	_, _ = hasher.Write(taskKey[:])
	_, _ = hasher.Write(requestFingerprint[:])
	_, _ = hasher.Write([]byte(promptCacheAlgorithmVersion))
	sum := hasher.Sum(nil)
	return 2*float64(binary.BigEndian.Uint16(sum[:2]))/65535 - 1
}

func claudeUsageTargetsForFeatures(features claudeUsageFeatures) claudeUsageTargets {
	reuseRatio := clampFloat(features.ReuseRatio, 0, 1)
	sizeFactor := clampFloat(features.SizeFactor, 0, 1)
	toolRatio := clampFloat(features.ToolRatio, 0, 1)
	stableJitter := clampFloat(features.StableJitter, -1, 1)

	inputRaw := 0.03 -
		0.015*reuseRatio +
		0.010*toolRatio +
		0.005*(1-sizeFactor) +
		0.003*stableJitter
	inputShare := clampFloat(inputRaw, 0.01, 0.05)
	cacheTotal := 1 - inputShare

	if !shouldAllocateClaudeCacheRead(features) {
		return claudeUsageTargets{
			InputShare:  inputShare,
			ReadShare:   0,
			CreateShare: cacheTotal,
		}
	}

	ratio := claudeReadCreateRatioForFeatures(features)
	createShare := cacheTotal / (1 + ratio)

	return claudeUsageTargets{
		InputShare:  inputShare,
		ReadShare:   cacheTotal - createShare,
		CreateShare: createShare,
	}
}

func shouldAllocateClaudeCacheRead(features claudeUsageFeatures) bool {
	return features.Phase == promptCachePhaseContinue ||
		clampFloat(features.StableJitter, -1, 1) >= firstRoundReadJitterThreshold
}

func claudeReadCreateRatioForFeatures(features claudeUsageFeatures) float64 {
	ratioRaw := 2.40 +
		0.95*clampFloat(features.SizeFactor, 0, 1) +
		0.25*clampFloat(features.ReuseRatio, 0, 1) -
		0.15*clampFloat(features.GrowthRatio, 0, 1) +
		0.35*clampFloat(features.StableJitter, -1, 1)
	return clampFloat(ratioRaw, minClaudeReadCreateRatio, maxClaudeReadCreateRatio)
}

func allocateClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	ttl promptCacheTTL,
	target claudeUsageTargets,
) (ClaudeUsage, bool) {
	usage, ok, _ := allocateClaudeUsageWithCandidateCount(
		rawInputTokens,
		rawOutputTokens,
		ttl,
		target,
	)
	return usage, ok
}

func allocateClaudeUsageWithCandidateCount(
	rawInputTokens int,
	rawOutputTokens int,
	ttl promptCacheTTL,
	target claudeUsageTargets,
) (ClaudeUsage, bool, int) {
	if rawInputTokens <= 1 || rawOutputTokens < 0 ||
		rawInputTokens > maxIntValue()/20 ||
		!validClaudeUsageTarget(target) {
		return ClaudeUsage{}, false, 0
	}

	createWeight := 0
	switch ttl {
	case promptCacheTTL5m:
		createWeight = 25
	case promptCacheTTL1h:
		createWeight = 40
	default:
		return ClaudeUsage{}, false, 0
	}

	weightedUnit := 20*target.InputShare +
		2*target.ReadShare +
		float64(createWeight)*target.CreateShare
	if weightedUnit <= 0 || math.IsNaN(weightedUnit) || math.IsInf(weightedUnit, 0) {
		return ClaudeUsage{}, false, 0
	}

	weightedBudget := 20 * rawInputTokens
	displayTotalFloat := float64(weightedBudget) / weightedUnit
	if displayTotalFloat <= 0 || displayTotalFloat > float64(maxIntValue()) {
		return ClaudeUsage{}, false, 0
	}
	displayTotal := int(math.Round(displayTotalFloat))
	targetInput := int(math.Round(target.InputShare * float64(displayTotal)))
	targetCreate := int(math.Round(target.CreateShare * float64(displayTotal)))

	offsets := [...]int{0, -1, 1, -2, 2, -3, 3, 4}
	best := ClaudeUsage{}
	bestDistance := math.Inf(1)
	found := false
	checked := 0

	for _, inputOffset := range offsets {
		for _, createOffset := range offsets {
			if checked >= maxClaudeUsageCandidates {
				break
			}
			checked++

			inputTokens := targetInput + inputOffset
			creationTokens := targetCreate + createOffset
			if inputTokens < 0 || creationTokens < 0 {
				continue
			}
			if ttl == promptCacheTTL5m && creationTokens%2 != 0 {
				continue
			}
			if inputTokens > weightedBudget/20 {
				continue
			}

			remaining := weightedBudget - 20*inputTokens
			if creationTokens > remaining/createWeight {
				continue
			}
			numerator := remaining - createWeight*creationTokens
			if numerator < 0 || numerator%2 != 0 {
				continue
			}
			readTokens := numerator / 2
			if readTokens < 0 ||
				inputTokens > maxIntValue()-readTokens ||
				inputTokens+readTokens > maxIntValue()-creationTokens {
				continue
			}

			candidate := ClaudeUsage{
				InputTokens:              inputTokens,
				OutputTokens:             rawOutputTokens,
				CacheCreationInputTokens: creationTokens,
				CacheReadInputTokens:     readTokens,
			}
			if ttl == promptCacheTTL5m {
				candidate.CacheCreation.Ephemeral5mInputTokens = creationTokens
			} else {
				candidate.CacheCreation.Ephemeral1hInputTokens = creationTokens
			}

			if !validAllocatedClaudeUsage(rawInputTokens, rawOutputTokens, candidate, target) {
				continue
			}

			total := inputTokens + readTokens + creationTokens
			inputShare := float64(inputTokens) / float64(total)
			readShare := float64(readTokens) / float64(total)
			createShare := float64(creationTokens) / float64(total)
			distance := square(inputShare-target.InputShare) +
				square(readShare-target.ReadShare) +
				square(createShare-target.CreateShare)

			if !found || distance < bestDistance ||
				(distance == bestDistance && claudeUsageLess(candidate, best)) {
				best = candidate
				bestDistance = distance
				found = true
			}
		}
	}

	return best, found, checked
}

func validClaudeUsageTarget(target claudeUsageTargets) bool {
	values := [...]float64{target.InputShare, target.ReadShare, target.CreateShare}
	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	if math.Abs(target.InputShare+target.ReadShare+target.CreateShare-1) > 1e-9 {
		return false
	}
	if target.InputShare < 0.01 || target.InputShare > 0.05 {
		return false
	}
	cacheRate := target.ReadShare + target.CreateShare
	if cacheRate < 0.95 || cacheRate > 0.99 {
		return false
	}
	if target.ReadShare == 0 {
		return target.CreateShare == cacheRate
	}
	if target.CreateShare <= 0 {
		return false
	}
	ratio := target.ReadShare / target.CreateShare
	return ratio >= minClaudeReadCreateRatio &&
		ratio <= maxClaudeReadCreateRatio
}

func validAllocatedClaudeUsage(
	rawInputTokens int,
	rawOutputTokens int,
	usage ClaudeUsage,
	target claudeUsageTargets,
) bool {
	if usage.InputTokens < 0 ||
		usage.OutputTokens != rawOutputTokens ||
		usage.CacheReadInputTokens < 0 ||
		usage.CacheCreationInputTokens < 0 {
		return false
	}
	if usage.CacheCreationInputTokens !=
		usage.CacheCreation.Ephemeral5mInputTokens+
			usage.CacheCreation.Ephemeral1hInputTokens {
		return false
	}
	if usage.CacheCreation.Ephemeral5mInputTokens > 0 &&
		usage.CacheCreation.Ephemeral1hInputTokens > 0 {
		return false
	}

	total := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	if total <= 0 {
		return false
	}
	inputShare := float64(usage.InputTokens) / float64(total)
	readShare := float64(usage.CacheReadInputTokens) / float64(total)
	createShare := float64(usage.CacheCreationInputTokens) / float64(total)
	cacheRate := readShare + createShare
	if inputShare < 0.01 || inputShare > 0.05 ||
		cacheRate < 0.95 || cacheRate > 0.99 {
		return false
	}
	if target.ReadShare == 0 {
		if usage.CacheReadInputTokens != 0 {
			return false
		}
	} else {
		if usage.CacheCreationInputTokens <= 0 {
			return false
		}
		ratio := float64(usage.CacheReadInputTokens) /
			float64(usage.CacheCreationInputTokens)
		if ratio < minClaudeReadCreateRatio ||
			ratio > maxClaudeReadCreateRatio {
			return false
		}
	}
	return claudeUsageCostConserved(rawInputTokens, usage)
}

func claudeUsageCostConserved(rawInputTokens int, usage ClaudeUsage) bool {
	if rawInputTokens < 0 || rawInputTokens > maxIntValue()/20 {
		return false
	}
	left := 20 * rawInputTokens
	if usage.InputTokens < 0 || usage.InputTokens > left/20 {
		return false
	}
	right := 20 * usage.InputTokens

	if usage.CacheReadInputTokens < 0 ||
		usage.CacheReadInputTokens > (maxIntValue()-right)/2 {
		return false
	}
	right += 2 * usage.CacheReadInputTokens

	fiveMinute := usage.CacheCreation.Ephemeral5mInputTokens
	if fiveMinute < 0 || fiveMinute > (maxIntValue()-right)/25 {
		return false
	}
	right += 25 * fiveMinute

	oneHour := usage.CacheCreation.Ephemeral1hInputTokens
	if oneHour < 0 || oneHour > (maxIntValue()-right)/40 {
		return false
	}
	right += 40 * oneHour
	return left == right
}

func claudeUsageLess(left, right ClaudeUsage) bool {
	if left.InputTokens != right.InputTokens {
		return left.InputTokens < right.InputTokens
	}
	if left.CacheReadInputTokens != right.CacheReadInputTokens {
		return left.CacheReadInputTokens < right.CacheReadInputTokens
	}
	return left.CacheCreationInputTokens < right.CacheCreationInputTokens
}

func clampFloat(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func square(value float64) float64 {
	return value * value
}

func maxIntValue() int {
	return int(^uint(0) >> 1)
}
