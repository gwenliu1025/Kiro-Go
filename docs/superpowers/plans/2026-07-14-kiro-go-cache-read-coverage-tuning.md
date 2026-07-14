# Kiro-Go 缓存读取覆盖率微调实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不改变缓存状态架构的前提下，提高首轮读取覆盖率，并把所有读写 usage 的读取/创建倍数收敛到 `2–5`、均值约 `3`。

**Architecture:** 继续使用现有 `claudeUsageFeatures`、任务跟踪器和整数费用守恒求解器。只在目标比例函数中增加稳定首轮读取门禁，并由目标倍数反解读取和创建比例；整数结果增加倍数校验。

**Tech Stack:** Go、现有 `testing`、Docker、PostgreSQL 只读 UAT 查询。

---

### Task 1: 用失败测试锁定首轮读取覆盖率

**Files:**
- Modify: `proxy/claude_usage_allocator_test.go`

- [ ] **Step 1: 改写首轮目标测试**

把原 `TestClaudeUsageTargetsFirstRoundHasCreationOnly` 拆成：

```go
func TestClaudeUsageTargetsPromotesMostFirstRoundsToRead(t *testing.T) {
	var reads int
	const samples = 5_000
	for i := 0; i < samples; i++ {
		features := representativeClaudeUsageFeatures(i)
		features.Phase = promptCachePhaseFirst
		target := claudeUsageTargetsForFeatures(features)
		if target.ReadShare > 0 {
			reads++
		}
	}
	rate := float64(reads) / samples
	if rate < 0.78 || rate > 0.82 {
		t.Fatalf("首轮读取覆盖率应接近 80%%，得到 %.2f%%", rate*100)
	}
}

func TestClaudeUsageTargetsKeepsMinorityFirstRoundsCreationOnly(t *testing.T) {
	target := claudeUsageTargetsForFeatures(claudeUsageFeatures{
		Phase:        promptCachePhaseFirst,
		GrowthRatio:  1,
		SizeFactor:   0.7,
		StableJitter: -0.9,
	})
	if target.ReadShare != 0 || target.CreateShare <= 0 {
		t.Fatalf("低门禁首轮应只创建：%+v", target)
	}
}
```

- [ ] **Step 2: 运行测试并确认失败**

Run:

```powershell
go test ./proxy -run '^TestClaudeUsageTargets(PromotesMostFirstRoundsToRead|KeepsMinorityFirstRoundsCreationOnly)$' -count=1 -v
```

Expected: `PromotesMostFirstRoundsToRead` 失败，当前读取覆盖率为 `0%`。

### Task 2: 用失败测试锁定读写倍数

**Files:**
- Modify: `proxy/claude_usage_allocator_test.go`

- [ ] **Step 1: 新增目标倍数分布测试**

```go
func TestClaudeUsageTargetsKeepReadCreateRatioNearThree(t *testing.T) {
	var sum float64
	const samples = 5_000
	buckets := make(map[string]struct{})
	for i := 0; i < samples; i++ {
		features := representativeClaudeUsageFeatures(i)
		features.Phase = promptCachePhaseContinue
		target := claudeUsageTargetsForFeatures(features)
		ratio := target.ReadShare / target.CreateShare
		if ratio < 2 || ratio > 5 {
			t.Fatalf("目标倍数越界：%.6f", ratio)
		}
		sum += ratio
		buckets[fmt.Sprintf("%.2f", ratio)] = struct{}{}
	}
	average := sum / samples
	if average < 2.8 || average > 3.2 {
		t.Fatalf("平均目标倍数应接近 3，得到 %.6f", average)
	}
	if len(buckets) < 100 {
		t.Fatalf("目标倍数过于统一，仅有 %d 个 0.01 分桶", len(buckets))
	}
}

func TestClaudeUsageTargetsLargerInputReadsMore(t *testing.T) {
	base := claudeUsageFeatures{
		Phase:        promptCachePhaseContinue,
		ReuseRatio:   0.6,
		GrowthRatio:  0.2,
		StableJitter: 0.1,
	}
	small := base
	small.SizeFactor = 0.2
	large := base
	large.SizeFactor = 0.9
	smallTarget := claudeUsageTargetsForFeatures(small)
	largeTarget := claudeUsageTargetsForFeatures(large)
	if largeTarget.ReadShare/largeTarget.CreateShare <=
		smallTarget.ReadShare/smallTarget.CreateShare {
		t.Fatalf("大输入的读取倍数应更高")
	}
}
```

- [ ] **Step 2: 新增整数结果倍数测试**

```go
func TestAllocateClaudeUsageKeepsActualReadCreateRatioBounded(t *testing.T) {
	for _, ttl := range []promptCacheTTL{promptCacheTTL5m, promptCacheTTL1h} {
		for rawInput := 100; rawInput <= 100_000; rawInput += 997 {
			target := claudeUsageTargetsForFeatures(claudeUsageFeatures{
				Phase:        promptCachePhaseContinue,
				ReuseRatio:   0.7,
				GrowthRatio:  0.2,
				SizeFactor:   math.Log2(1+float64(rawInput)) / 20,
				StableJitter: float64(rawInput%201)/100 - 1,
			})
			usage, ok := allocateClaudeUsage(rawInput, 10, ttl, target)
			if !ok {
				continue
			}
			ratio := float64(usage.CacheReadInputTokens) /
				float64(usage.CacheCreationInputTokens)
			if ratio < 2 || ratio > 5 {
				t.Fatalf("整数倍数越界：ttl=%v input=%d ratio=%.6f", ttl, rawInput, ratio)
			}
		}
	}
}
```

- [ ] **Step 3: 运行测试并确认失败**

Run:

```powershell
go test ./proxy -run '^(TestClaudeUsageTargetsKeepReadCreateRatioNearThree|TestClaudeUsageTargetsLargerInputReadsMore|TestAllocateClaudeUsageKeepsActualReadCreateRatioBounded)$' -count=1 -v
```

Expected: 当前平均倍数或上限测试失败，现有最大目标约为 `11.25`。

### Task 3: 实现最小目标比例调整

**Files:**
- Modify: `proxy/claude_usage_allocator.go`
- Modify: `proxy/cache_tracker.go`

- [ ] **Step 1: 增加门禁和倍数常量**

```go
const (
	firstRoundReadJitterThreshold = -0.60
	minClaudeReadCreateRatio      = 2.0
	maxClaudeReadCreateRatio      = 5.0
)
```

- [ ] **Step 2: 用稳定门禁选择读取分支**

```go
func shouldAllocateClaudeCacheRead(features claudeUsageFeatures) bool {
	return features.Phase == promptCachePhaseContinue ||
		clampFloat(features.StableJitter, -1, 1) >= firstRoundReadJitterThreshold
}
```

首轮或重建且未通过门禁时继续返回只创建目标。

- [ ] **Step 3: 用目标倍数反解比例**

```go
func claudeReadCreateRatioForFeatures(features claudeUsageFeatures) float64 {
	ratioRaw := 2.40 +
		0.95*clampFloat(features.SizeFactor, 0, 1) +
		0.25*clampFloat(features.ReuseRatio, 0, 1) -
		0.15*clampFloat(features.GrowthRatio, 0, 1) +
		0.35*clampFloat(features.StableJitter, -1, 1)
	return clampFloat(ratioRaw, minClaudeReadCreateRatio, maxClaudeReadCreateRatio)
}
```

在读取分支中：

```go
ratio := claudeReadCreateRatioForFeatures(features)
createShare := cacheTotal / (1 + ratio)
return claudeUsageTargets{
	InputShare:  inputShare,
	ReadShare:   cacheTotal - createShare,
	CreateShare: createShare,
}
```

- [ ] **Step 4: 更新目标和整数校验**

当读取大于零时，删除旧 `78%–90%`、`8%–20%` 校验，改为：

```go
ratio := target.ReadShare / target.CreateShare
return target.CreateShare > 0 &&
	ratio >= minClaudeReadCreateRatio &&
	ratio <= maxClaudeReadCreateRatio
```

整数 usage 使用 token 倍数执行相同边界校验。

- [ ] **Step 5: 提升算法版本**

把 `promptCacheAlgorithmVersion` 改为：

```go
const promptCacheAlgorithmVersion = "native-high-cache-v2"
```

- [ ] **Step 6: 运行定向测试**

Run:

```powershell
go test ./proxy -run '^(TestClaudeUsageTargets|TestAllocateClaudeUsage)' -count=1
```

Expected: PASS。

- [ ] **Step 7: 提交实现**

```powershell
git add proxy/claude_usage_allocator.go proxy/claude_usage_allocator_test.go proxy/cache_tracker.go
git commit -m "修复：提高缓存读取覆盖率并收紧读写倍数"
```

### Task 4: 更新合同测试并完成回归

**Files:**
- Modify: `proxy/claude_usage_contract_test.go`
- Modify: `proxy/claude_usage_allocator_test.go`

- [ ] **Step 1: 更新旧首轮只创建断言**

将固定断言“首轮不得读取”改为：

- 低门禁样本仍只创建。
- 通过门禁样本同时读取和创建。
- 同步与流式继续使用同一最终分配函数。

- [ ] **Step 2: 运行完整验证**

Run:

```powershell
go test ./...
go vet ./...
go build ./...
git diff --check
```

Expected: 四条命令均退出码 `0`。

- [ ] **Step 3: 提交测试与文档**

```powershell
git add proxy/claude_usage_contract_test.go proxy/claude_usage_allocator_test.go docs/superpowers
git commit -m "测试：覆盖缓存读取门禁与读写比例"
```

### Task 5: Linux 竞态与镜像构建

**Files:**
- No repository file changes expected.

- [ ] **Step 1: 上传精确源码归档到毕业机 staging**

归档必须排除 `.git`，并记录 SHA256。

- [ ] **Step 2: 运行 Linux 门禁**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

Expected: 全部退出码 `0`，日志中没有 `DATA RACE` 或 `FAIL`。

- [ ] **Step 3: 构建精确镜像**

镜像标签使用：

```text
local/kiro-go:cache-read-tuning-<short_commit>-20260714
```

记录镜像 ID、revision 和归档 SHA256。

### Task 6: 二号机 UAT 与毕业机同步

**Files:**
- Update after acceptance:
  `docs/superpowers/results/2026-07-14-production-kiro-native-high-cache-deployment.md`

- [ ] **Step 1: 记录二号机切换前基线并备份 Compose**

记录所有容器启动时间，只备份 Kiro-Go Compose。

- [ ] **Step 2: 只重建二号机 `kiro-go-pr131`**

不得重建 Sub2API 或其他服务。

- [ ] **Step 3: 验证健康和真实 usage**

至少观察 30 条新记录，统计：

```text
读取请求占比
创建请求占比
连续 10 条读取数量
读写记录最小/平均/最大倍数
缓存率
TTL 分布
旧字段写入
```

UAT 门槛：

```text
读取请求占比：80% 到 95%
读写倍数：全部位于 2 到 5
平均倍数：2.8 到 3.2
缓存率：全部位于 95% 到 99%
双 TTL、override、旧分配写入：0
```

- [ ] **Step 4: UAT 通过后同步同一镜像到毕业机**

核对镜像归档 SHA256，只重建毕业机 `kiro-go-pr131`。

- [ ] **Step 5: 完成最终文档与健康检查**

记录提交、镜像、UAT 样本、回滚路径和毕业机状态，运行：

```powershell
git diff --check
git status --short
```
