package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
)

const (
	maxClaudeUsageCandidates = 64
	minClaudeReadHitRate     = 0.85
	maxClaudeReadHitRate     = 0.95
)

type claudeUsageRequestClass uint8

const (
	claudeUsageReadCreate claudeUsageRequestClass = iota
	claudeUsageReadOnly
)

type claudeUsageFeatures struct {
	Phase                promptCachePhase
	ReuseRatio           float64
	GrowthRatio          float64
	AgeRatio             float64
	RoundFactor          float64
	SizeFactor           float64
	ToolRatio            float64
	StableHitJitter      float64
	StableCreationJitter float64
	CreateCache          bool
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
		Phase:       snapshot.Phase,
		ReuseRatio:  clampFloat(reuseRatio, 0, 1),
		GrowthRatio: clampFloat(growthRatio, 0, 1),
		AgeRatio:    clampFloat(snapshot.AgeRatio, 0, 1),
		RoundFactor: clampFloat(math.Log2(1+float64(maxInt(snapshot.SuccessfulRounds, 0)))/4, 0, 1),
		SizeFactor:  clampFloat(math.Log2(1+float64(maxInt(rawInputTokens, 0)))/20, 0, 1),
		ToolRatio:   clampFloat(toolRatio, 0, 1),
		StableHitJitter: stableClaudeUsageJitterFor(
			snapshot.TaskKey,
			snapshot.RequestFingerprint,
			"hit",
		),
		StableCreationJitter: stableClaudeUsageJitterFor(
			snapshot.TaskKey,
			snapshot.RequestFingerprint,
			"creation",
		),
		CreateCache: claudeUsageRequestClassFor(
			snapshot.TaskKey,
			snapshot.RequestFingerprint,
		) == claudeUsageReadCreate,
	}
}

func claudeUsageRequestClassFor(taskKey, requestFingerprint [32]byte) claudeUsageRequestClass {
	probability := clampFloat(
		0.75+0.02*stableClaudeUsageJitterFor(
			taskKey,
			requestFingerprint,
			"create-probability",
		),
		0.70,
		0.80,
	)
	if stableClaudeUsageUnit(taskKey, requestFingerprint, "create-draw") < probability {
		return claudeUsageReadCreate
	}
	return claudeUsageReadOnly
}

func stableClaudeUsageUnit(taskKey, requestFingerprint [32]byte, salt string) float64 {
	hasher := sha256.New()
	_, _ = hasher.Write(taskKey[:])
	_, _ = hasher.Write(requestFingerprint[:])
	_, _ = hasher.Write([]byte(promptCacheAlgorithmVersion))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(salt))
	sum := hasher.Sum(nil)
	return float64(binary.BigEndian.Uint64(sum[:8])) / float64(^uint64(0))
}

func stableClaudeUsageJitterFor(taskKey, requestFingerprint [32]byte, salt string) float64 {
	return 2*stableClaudeUsageUnit(taskKey, requestFingerprint, salt) - 1
}

func claudeUsageTargetsForFeatures(features claudeUsageFeatures) claudeUsageTargets {
	sizeFactor := clampFloat(features.SizeFactor, 0, 1)
	stableHitJitter := clampFloat(features.StableHitJitter, -1, 1)
	stableCreationJitter := clampFloat(features.StableCreationJitter, -1, 1)
	sizePressure := smoothstep(0.60, 0.875, sizeFactor)

	targetHitRate := clampFloat(
		claudeBaseHitRate(sizePressure)+0.010*stableHitJitter,
		minClaudeReadHitRate,
		maxClaudeReadHitRate,
	)
	nonReadShare := 1 - targetHitRate
	createShare := 0.0
	if features.CreateCache {
		creationFraction := clampFloat(
			0.32+
				0.38*math.Pow(sizePressure, 1.2)+
				0.035*stableCreationJitter,
			0.26,
			0.75,
		)
		createShare = nonReadShare * creationFraction
	}

	return claudeUsageTargets{
		InputShare:  nonReadShare - createShare,
		ReadShare:   targetHitRate,
		CreateShare: createShare,
	}
}

func claudeBaseHitRate(sizePressure float64) float64 {
	anchors := [...]struct {
		pressure float64
		hit      float64
	}{
		{pressure: 0.00, hit: 0.950},
		{pressure: 0.40, hit: 0.950},
		{pressure: 0.70, hit: 0.950},
		{pressure: 0.88, hit: 0.948},
		{pressure: 1.00, hit: 0.900},
	}
	p := clampFloat(sizePressure, 0, 1)
	for i := 1; i < len(anchors); i++ {
		if p > anchors[i].pressure {
			continue
		}
		left := anchors[i-1]
		right := anchors[i]
		t := smoothstep01((p - left.pressure) / (right.pressure - left.pressure))
		return left.hit + (right.hit-left.hit)*t
	}
	return anchors[len(anchors)-1].hit
}

func smoothstep(edge0, edge1, value float64) float64 {
	if edge1 <= edge0 {
		return 0
	}
	return smoothstep01((value - edge0) / (edge1 - edge0))
}

func smoothstep01(value float64) float64 {
	t := clampFloat(value, 0, 1)
	return t * t * (3 - 2*t)
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
	if ok || target.CreateShare <= 0 {
		return usage, ok
	}

	fallbackTarget := claudeUsageTargets{
		InputShare:  1 - target.ReadShare,
		ReadShare:   target.ReadShare,
		CreateShare: 0,
	}
	fallbackUsage, fallbackOK, _ := allocateClaudeUsageWithCandidateCount(
		rawInputTokens,
		rawOutputTokens,
		ttl,
		fallbackTarget,
	)
	return fallbackUsage, fallbackOK
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
	if target.InputShare < 0.01 || target.InputShare > 0.15 {
		return false
	}
	if target.ReadShare < minClaudeReadHitRate ||
		target.ReadShare > maxClaudeReadHitRate {
		return false
	}
	if target.CreateShare < 0 ||
		target.CreateShare > 1-target.ReadShare {
		return false
	}
	return true
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
	if inputShare < 0.01 || inputShare > 0.15 ||
		readShare < minClaudeReadHitRate || readShare > maxClaudeReadHitRate {
		return false
	}
	if target.CreateShare == 0 {
		if usage.CacheCreationInputTokens != 0 {
			return false
		}
	} else {
		if usage.CacheReadInputTokens <= 0 || usage.CacheCreationInputTokens <= 0 {
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
