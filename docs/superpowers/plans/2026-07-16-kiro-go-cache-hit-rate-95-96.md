# Kiro-Go 真实缓存命中率 95%–96% 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**目标：** 将 Kiro-Go 成功完整 Anthropic 请求的真实读取缓存 Token 命中率控制在 95%–96%，读取请求接近 100%，创建请求约占 50%。

**架构：** 在现有 Claude usage 分配器中增加稳定的读取/读写请求类别。所有正常请求都允许读取；约一半请求只读取，约一半请求读取并创建少量新增缓存。继续使用现有整数费用守恒、单一 TTL、请求指纹幂等和进程内任务追踪，不引入外部状态。

**技术栈：** Go 1.21、标准库 crypto/sha256、现有 proxy 测试、Docker Compose。

---

## 文件范围

- 修改：proxy/claude_usage_allocator.go
- 修改：proxy/claude_usage_allocator_test.go
- 修改：proxy/claude_usage_contract_test.go
- 修改：proxy/cache_tracker.go
- 不修改：Sub2API、数据库、Redis、DNS 和其他服务。

## 任务 1：测试先行锁定新指标

**文件：** proxy/claude_usage_allocator_test.go

- [ ] **步骤 1：替换旧的首轮读取测试。**

删除依赖 firstRoundReadJitterThreshold 的首轮 80% 读取和少数创建-only测试，新增：

~~~go
func TestClaudeUsageTargetsAlwaysReadAndHalfCreate(t *testing.T) {
    var readOnly, readCreate int
    const samples = 10_000
    for i := 0; i < samples; i++ {
        features := representativeClaudeUsageFeatures(i)
        features.CreateCache = i%2 == 0
        target := claudeUsageTargetsForFeatures(features)
        assertTargetHitRate(t, target.ReadShare)
        if target.CreateShare == 0 {
            readOnly++
        } else {
            readCreate++
        }
    }
    if readOnly != samples/2 || readCreate != samples/2 {
        t.Fatalf("请求类别分布错误：只读=%d 读写=%d", readOnly, readCreate)
    }
}
~~~

- [ ] **步骤 2：新增目标命中率断言和输入规模方向测试。**

~~~go
func assertTargetHitRate(t *testing.T, hitRate float64) {
    t.Helper()
    if hitRate < minClaudeReadHitRate || hitRate > maxClaudeReadHitRate {
        t.Fatalf("真实读取命中率越界：%.6f", hitRate)
    }
}
~~~

测试小、中、大输入以及不同工具占比，要求目标命中率位于 0.95–0.96，且大输入目标不低于小输入。

另外新增稳定性测试：同一个 taskKey 和 requestFingerprint 连续计算 100 次必须得到相同类别；10,000 组不同指纹的创建类别必须接近 50%。

- [ ] **步骤 3：运行测试确认失败。**

~~~powershell
go test ./proxy -run '^TestClaudeUsageTargetsAlwaysReadAndHalfCreate$' -count=1 -v
~~~

预期：因当前特征没有 CreateCache、旧模型仍有首轮不读取分支而失败。

- [ ] **步骤 4：提交测试基线。**

~~~powershell
git add proxy/claude_usage_allocator_test.go
git commit -m "测试：锁定Kiro-Go真实缓存命中率目标"
~~~

## 任务 2：实现稳定请求类别和目标比例

**文件：** proxy/claude_usage_allocator.go

- [ ] **步骤 1：新增常量和特征字段。**

~~~go
const (
    maxClaudeUsageCandidates = 64
    minClaudeReadHitRate     = 0.95
    maxClaudeReadHitRate     = 0.96
)

type claudeUsageRequestClass uint8

const (
    claudeUsageReadCreate claudeUsageRequestClass = iota
    claudeUsageReadOnly
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
    CreateCache  bool
}
~~~

删除 firstRoundReadJitterThreshold、minClaudeReadCreateRatio 和 maxClaudeReadCreateRatio。

- [ ] **步骤 2：实现稳定类别函数并接入 buildClaudeUsageFeatures。**

~~~go
func claudeUsageRequestClassFor(taskKey, requestFingerprint [32]byte) claudeUsageRequestClass {
    hasher := sha256.New()
    _, _ = hasher.Write(taskKey[:])
    _, _ = hasher.Write(requestFingerprint[:])
    _, _ = hasher.Write([]byte(promptCacheAlgorithmVersion))
    sum := hasher.Sum(nil)
    if binary.BigEndian.Uint16(sum[:2])%100 < 50 {
        return claudeUsageReadCreate
    }
    return claudeUsageReadOnly
}
~~~

返回特征时写入：

~~~go
CreateCache: claudeUsageRequestClassFor(
    snapshot.TaskKey,
    snapshot.RequestFingerprint,
) == claudeUsageReadCreate,
~~~

类别只能由任务键、请求指纹和算法版本决定，不能使用进程随机数、上游账号 ID 或 API Key 原文。

- [ ] **步骤 3：替换 claudeUsageTargetsForFeatures 的核心计算。**

~~~go
targetHitRate := clampFloat(
    0.952+
        0.004*sizeFactor+
        0.001*toolRatio+
        0.001*stableJitter,
    minClaudeReadHitRate,
    maxClaudeReadHitRate,
)
nonReadShare := 1 - targetHitRate

createShare := 0.0
if features.CreateCache {
    creationFraction := clampFloat(
        0.40+
            0.20*clampFloat(features.GrowthRatio, 0, 1)+
            0.05*toolRatio+
            0.05*stableJitter,
        0.40,
        0.65,
    )
    createShare = nonReadShare * creationFraction
}

return claudeUsageTargets{
    InputShare:  nonReadShare - createShare,
    ReadShare:  targetHitRate,
    CreateShare: createShare,
}
~~~

所有正常请求都有读取目标；只有读写类别有创建目标。删除 shouldAllocateClaudeCacheRead 和 claudeReadCreateRatioForFeatures。

- [ ] **步骤 4：运行目标测试并提交。**

~~~powershell
go test ./proxy -run '^TestClaudeUsageTargets' -count=1 -v
git add proxy/claude_usage_allocator.go
git commit -m "调整：将Kiro-Go真实缓存命中率控制在95到96%"
~~~

预期：新的命中率和类别测试通过；旧的读取/创建倍数测试等待任务 4 改写。

## 任务 3：修改整数求解器和候选校验

**文件：** proxy/claude_usage_allocator.go

- [ ] **步骤 1：将目标校验改为真实读取命中率。**

validClaudeUsageTarget 必须保留现有的非负值和数值有限性校验，接受 CreateShare 为 0，并验证：

~~~go
if target.ReadShare < minClaudeReadHitRate ||
    target.ReadShare > maxClaudeReadHitRate {
    return false
}
if target.InputShare < 0.01 || target.InputShare > 0.05 {
    return false
}
if target.CreateShare < 0 ||
    target.CreateShare > 1-target.ReadShare {
    return false
}
if math.Abs(target.InputShare+target.ReadShare+target.CreateShare-1) > 1e-9 {
    return false
}
return true
~~~

不得再使用 read + create 作为缓存命中指标。

- [ ] **步骤 2：将实际结果校验改为 read / total。**

~~~go
total := usage.InputTokens +
    usage.CacheReadInputTokens +
    usage.CacheCreationInputTokens
hitRate := float64(usage.CacheReadInputTokens) / float64(total)
~~~

要求实际命中率为 0.95–0.96。只读目标要求创建为 0；读写目标要求创建大于 0；删除读取/创建 2–5 倍硬限制。

- [ ] **步骤 3：保持费用守恒和单一 TTL。**

继续满足：

~~~go
usage.CacheCreationInputTokens ==
    usage.CacheCreation.Ephemeral5mInputTokens+
        usage.CacheCreation.Ephemeral1hInputTokens
~~~

并继续使用 20、2、25、40 权重。5 分钟和 1 小时字段不能同时大于 0。

- [ ] **步骤 4：为小输入增加只读取降级。**

创建目标求解失败时，用同一个 InputShare 重试：

~~~go
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
    InputShare:  target.InputShare,
    ReadShare:   1 - target.InputShare,
    CreateShare: 0,
}
fallbackUsage, fallbackOK, _ := allocateClaudeUsageWithCandidateCount(
    rawInputTokens,
    rawOutputTokens,
    ttl,
    fallbackTarget,
)
return fallbackUsage, fallbackOK
~~~

只有只读取也无法整数守恒时才返回原始 usage。

- [ ] **步骤 5：运行求解器测试并提交。**

~~~powershell
go test ./proxy -run '^(TestAllocateClaudeUsage|TestClaudeUsageTargets)' -count=1 -v
git add proxy/claude_usage_allocator.go
git commit -m "修复：支持Kiro-Go高读取低创建整数分配"
~~~

## 任务 4：更新同步/流式合同测试

**文件：** proxy/claude_usage_allocator_test.go、proxy/claude_usage_contract_test.go

- [ ] **步骤 1：替换旧的 2–5 倍测试。**

新增：

~~~go
func assertUsageHitRate(t *testing.T, usage ClaudeUsage) {
    t.Helper()
    total := usage.InputTokens +
        usage.CacheReadInputTokens +
        usage.CacheCreationInputTokens
    if total <= 0 {
        t.Fatalf("输入侧总 Token 必须为正")
    }
    hitRate := float64(usage.CacheReadInputTokens) / float64(total)
    if hitRate < minClaudeReadHitRate ||
        hitRate > maxClaudeReadHitRate {
        t.Fatalf("真实读取命中率越界：%.6f usage=%+v", hitRate, usage)
    }
}
~~~

保留读取大于创建的方向性断言，不再设置 5 倍上限。

- [ ] **步骤 2：更新完整同步/流式请求断言。**

把“首轮不得读取”改为完整请求必须读取并通过 assertUsageHitRate；截断、失败、最终 usage 缺失仍必须不提交任务状态、不伪造缓存字段。

- [ ] **步骤 3：添加同步/流式相同 usage 断言。**

同一分析结果和原始输入走同步、流式最终 usage，比较 input、read、creation、5m、1h、output 全部字段，并验证创建聚合字段等于两个 TTL 明细之和。

- [ ] **步骤 4：运行合同测试并提交。**

~~~powershell
go test ./proxy -run '^(TestClaudeUsage|TestClaudeNonStream|TestClaudeStream|TestClaudeUsageRetry|TestClaudeFailed|TestClaudeTruncated)' -count=1 -v
git add proxy/claude_usage_allocator_test.go proxy/claude_usage_contract_test.go
git commit -m "测试：覆盖Kiro-Go真实命中率和流式合同"
~~~

## 任务 5：升级算法版本并完成构建验证

**文件：** proxy/cache_tracker.go

- [ ] **步骤 1：升级算法版本。**

~~~go
const promptCacheAlgorithmVersion = "native-high-cache-v3"
~~~

这只影响稳定类别、TTL 哈希和任务指纹版本，不迁移历史内存状态，也不改写数据库历史记录。

- [ ] **步骤 2：运行完整测试、构建和 Compose 校验。**

~~~powershell
go test ./... -count=1
go build ./...
docker compose config
~~~

预期：全部退出码为 0，Compose 继续使用宿主机 8321、容器健康检查 8321。

- [ ] **步骤 3：在 Linux 环境执行竞态测试。**

~~~bash
go test -race ./...
~~~

Windows 缺少有效 CGO 编译器时，必须在毕业机或 CI 执行，不能用普通测试替代。

- [ ] **步骤 4：运行性能回归。**

~~~powershell
go test ./proxy -run '^$' -bench '^(BenchmarkAllocateClaudeUsage|BenchmarkBuildFeaturesAndAllocateClaudeUsage)$' -benchmem -count=5
~~~

预期：分配器保持纯内存计算，不增加网络、磁盘或外部锁等待。

- [ ] **步骤 5：提交算法版本。**

~~~powershell
git add proxy/cache_tracker.go
git commit -m "版本：升级Kiro-Go缓存命中率算法到v3"
~~~

## 任务 6：构建第一版资产并发布 Kiro-Go 代码

- [ ] **步骤 1：检查提交范围。**

~~~powershell
git status --short --branch
git log --oneline --decorate -8
git diff origin/feat/kiro-native-high-cache...HEAD --stat
~~~

预期：只包含本次 Kiro-Go 算法、测试、版本和文档。

- [ ] **步骤 2：推送 Kiro-Go 分支。**

~~~powershell
git push origin HEAD:feat/kiro-native-high-cache
~~~

不使用强制推送，不覆盖其他分支。

- [ ] **步骤 3：在毕业机构建第一版镜像归档。**

~~~bash
TAG=cache-hit-rate-95-96-$(git rev-parse --short HEAD)
docker build --pull=false -t local/kiro-go:$TAG .
docker image inspect local/kiro-go:$TAG --format '{{.Id}}|{{index .Config.Labels "org.opencontainers.image.revision"}}'
docker save local/kiro-go:$TAG | zstd -T0 -19 -o /home/ubuntu/staging/kiro-go-$TAG.tar.zst
sha256sum /home/ubuntu/staging/kiro-go-$TAG.tar.zst
~~~

预期：镜像构建成功、端口仍为 8321、归档有 SHA256，不推送浮动 latest。

- [ ] **步骤 4：部署前记录回滚信息。**

在两台机器只读记录当前 Kiro-Go 镜像 ID、源码 revision、启动时间和 Compose 校验和；部署时只重建 kiro-go-pr131，保留旧镜像和 Compose 备份。

## 任务 7：服务器迁移完成后的真实流量验证

**前置条件：** 服务器迁移、生产切换和第一版 Kiro-Go 部署已完成；本任务不设置独立 UAT 验收。

- [ ] **步骤 1：读取迁移后的真实完整 usage。**

只保留有最终 input_tokens、cache_read_input_tokens、cache_creation_input_tokens 的成功记录；不得输出 API Key、用户正文或 OAuth 内容。

- [ ] **步骤 2：计算加权命中率和请求比例。**

~~~text
sum(cache_read_input_tokens)
/
sum(input_tokens + cache_read_input_tokens + cache_creation_input_tokens)

count(cache_read_input_tokens > 0) / count(eligible_requests)
count(cache_creation_input_tokens > 0) / count(eligible_requests)
fallback_count / total_completed_requests
~~~

预期：真实命中率 95%–96%，读取请求接近 100%，创建请求约 40%–60%；fallback 单独报告。

- [ ] **步骤 3：仅在真实数据偏离时调整下一版。**

只调整命中率和创建比例参数，递增算法版本并保留当前镜像回滚点；不得回写历史 usage 或修改 Sub2API 历史数据。

## 计划自审

- 指标定义由任务 1、2、3、4、7 覆盖。
- 稳定 50% 创建类别由任务 1、2 覆盖。
- 费用守恒、单一 TTL、同步/流式一致性、失败回退和幂等由任务 3、4、5 覆盖。
- 取消独立 UAT 由任务 6、7 的前置条件体现。
- 没有引入 Sub2API、数据库、Redis 或 DNS 改动。
- 新增的 claudeUsageRequestClassFor、CreateCache、minClaudeReadHitRate、maxClaudeReadHitRate 均在任务 2 定义。
- 计划没有使用 TODO、TBD 或未定义的后续动作。
