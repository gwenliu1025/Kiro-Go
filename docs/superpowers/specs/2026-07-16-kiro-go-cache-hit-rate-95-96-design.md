# Kiro-Go 真实缓存命中率 95%–96% 设计

日期：2026-07-16

状态：设计已确认，待规格复核

## 1. 结论

本次只调整 Kiro-Go 的 Anthropic usage 分配模型，不修改 Sub2API 的缓存逻辑、数据库、Redis、缓存命名空间或下游协议转换。

Kiro-Go 将“缓存命中率”定义为真正被 `cache_read_input_tokens` 覆盖的输入 Token 比例：

```text
真实缓存命中率
= cache_read_input_tokens
  / (input_tokens + cache_read_input_tokens + cache_creation_input_tokens)
```

对成功且用量完整的请求，目标为：

```text
95% <= 真实缓存命中率 <= 96%
```

请求条数采用简单的两档分配：约一半请求只读取缓存，约一半请求读取缓存并创建少量新增缓存。这样读取缓存请求占比接近 100%，创建缓存请求占比约 50%，同时保持创建 Token 只占输入侧的小部分。

## 2. 背景与问题

当前 Kiro-Go 已经有整数用量分配和费用守恒能力，但现有约束把以下两个概念混在了一起：

1. `(cache_read + cache_creation) / 总输入侧 Token` 的缓存 Token 覆盖率。
2. `cache_read / 总输入侧 Token` 的真正读取命中率。

现有模型可以让缓存创建 Token 也参与 95%–99% 的缓存覆盖率，因此即使很多请求在创建缓存，面板仍会显示很高的缓存比例。这不符合下游要求的“真正缓存读取命中率 95%–96%”。

现有读取/创建 Token 倍数 2–5 倍的限制也与新目标冲突。若读取命中率达到 95%–96%，创建 Token 只占 1%–3%，读取/创建 Token 倍数必然达到几十倍，不能继续使用 2–5 倍作为硬约束。

## 3. 目标与非目标

### 3.1 目标

- 只在 Kiro-Go 端实现真实读取缓存命中率控制。
- 对完整成功请求将真实读取命中率控制在 95%–96%。
- 读取缓存请求占比接近 100%。
- 创建缓存请求占比约 50%，允许在 40%–60% 的观察窗口内波动。
- 创建缓存只代表少量新增上下文，不再占输入侧的大部分 Token。
- 原始输入规模越大，读取缓存比例不低于小输入请求。
- 保留现有 5 分钟或 1 小时单一 TTL 规则。
- 保留现有费用守恒、同步/流式一致性和请求重试幂等性。
- 不增加外部状态服务，不创建新的缓存命名空间。

### 3.2 非目标

- 不声称 Kiro 上游真实提供 Anthropic Prompt Cache。
- 不修改 Sub2API 的定价、扣费、数据库迁移或 Redis 状态。
- 不修改 OpenAI、Gemini、图片、视频和其他非 Anthropic usage 链路。
- 不为了追求比例而把错误、截断或不完整用量伪装成缓存命中。
- 不继续以读取/创建 Token 2–5 倍作为新模型的硬约束。

## 4. 指标合同

### 4.1 请求级真实命中率

每条成功且最终用量完整的请求计算：

```text
hit_rate
= R / (I + R + C)
```

其中：

- `I`：`input_tokens`。
- `R`：`cache_read_input_tokens`。
- `C`：`cache_creation_input_tokens`，等于 5 分钟和 1 小时创建明细之和。

正常请求必须满足：

```text
0.95 <= hit_rate <= 0.96
```

整数 Token 分配只允许产生极小的取整偏差；测试使用足够大的输入样本，保证最终值仍落在目标区间。

### 4.2 窗口累计命中率

真实流量验收使用加权累计值，而不是逐条简单平均：

```text
window_hit_rate
= sum(cache_read_input_tokens)
  / sum(input_tokens + cache_read_input_tokens + cache_creation_input_tokens)
```

UAT 和生产观察窗口均要求累计值位于 95%–96%。

### 4.3 请求条数比例

对正常完整请求单独计算：

```text
read_request_rate
= count(cache_read_input_tokens > 0) / count(all eligible requests)

create_request_rate
= count(cache_creation_input_tokens > 0) / count(all eligible requests)
```

目标为：

- `read_request_rate` 接近 100%。
- `create_request_rate` 约 50%，观察窗口允许 40%–60%。

读取/创建 Token 倍数只作为诊断指标，不再作为 2–5 倍的硬校验。

### 4.4 fallback 指标

以下请求不纳入正常命中率分母：

- 上游失败。
- 响应截断。
- 最终用量缺失。
- 原始输入 Token 无法得到有效值。
- 输入规模过小，无法构造合法整数分配。

这些请求必须返回原始 usage，并单独统计 `fallback_rate`。失败请求不得提交缓存任务状态，也不得推进成功轮次。

## 5. 请求类别分配

### 5.1 稳定两档分配

不再根据“首轮必须创建”决定是否读取。对每个成功候选请求，使用以下稳定信息生成类别：

```text
request_class_seed
= task_key
  + request_fingerprint
  + prompt_cache_algorithm_version
```

对 SHA-256 结果取模 100：

```text
0–49  -> 读取并创建少量新增缓存
50–99 -> 只读取缓存
```

同一请求指纹重复请求必须得到同一类别。不同请求即使输入规模相同，也可以因指纹不同而得到不同结果。

### 5.2 读取规则

两个类别都允许 `cache_read_input_tokens > 0`。这符合当前约定：第一条请求可以读取缓存，不为了模拟首轮而强行制造一条读取为零的记录。

任务追踪器仍然负责任务键、请求指纹、成功提交和重试幂等，但不再决定请求是否必须创建缓存。整个功能继续使用进程内状态，不增加 Redis 或数据库依赖。

## 6. Token 比例模型

### 6.1 目标命中率

输入规模、工具占比和确定性微扰共同生成目标命中率：

```text
target_hit_rate_raw
= 0.952
  + 0.004 * size_factor
  + 0.001 * tool_ratio
  + 0.001 * stable_jitter

target_hit_rate
= clamp(target_hit_rate_raw, 0.95, 0.96)
```

其中：

- `size_factor` 使用现有对数压缩输入规模，范围为 0–1。
- `tool_ratio` 使用现有工具和工具结果 Token 占比，范围为 0–1。
- `stable_jitter` 使用任务键、请求指纹和算法版本产生，范围为 -1–1。
- 微扰只能打散相近请求，不能把结果推离 95%–96%。

较大的输入请求命中率目标更接近 96%，较小的输入请求更接近 95%–95.5%。

### 6.2 非读取 Token

```text
non_read_share = 1 - target_hit_rate
```

因此非读取部分总量约为 4%–5%，由普通输入和缓存创建组成。

### 6.3 只读取类别

只读取类别使用：

```text
create_share = 0
input_share  = non_read_share
read_share   = target_hit_rate
```

这种请求没有缓存创建 Token，但仍然保留标准 `cache_creation` 对象，并将两个 TTL 明细置为 0，确保同步和流式字段形状一致。

### 6.4 读取并创建类别

读取并创建类别将非读取部分按轻量新增上下文信号拆分：

```text
creation_fraction
= clamp(
    0.40
    + 0.20 * growth_ratio
    + 0.05 * tool_ratio
    + 0.05 * stable_jitter,
    0.40,
    0.65,
  )

create_share = non_read_share * creation_fraction
input_share  = non_read_share - create_share
read_share   = target_hit_rate
```

因此创建缓存通常约占总输入侧 Token 的 1.6%–3.25%，普通输入占剩余非读取部分，读取缓存保持在 95%–96%。新增上下文、工具内容越多，创建部分可以在该小范围内增加，但不会主导请求。

## 7. 整数费用守恒

继续使用现有无量纲价格权重：

```text
普通输入       = 20
缓存读取       = 2
5 分钟创建     = 25
1 小时创建     = 40
```

给定原始输入 Token `T`，最终整数分配必须满足：

```text
20 * T
= 20 * I
 + 2 * R
 + 25 * C5
 + 40 * C1
```

并且：

```text
C = C5 + C1
C5 == 0 或 C1 == 0
```

现有候选搜索器继续负责整数化和费用校验，但需要做以下语义调整：

1. 允许 `R/C` 为几十倍，不再校验 2–5 倍上限。
2. 允许 `C=0` 的只读取候选。
3. 候选排序优先最接近 `target_hit_rate`，其次最接近普通输入和创建目标。
4. 小输入导致创建 Token 取整为 0 时，读取并创建类别降级为只读取类别，不返回失败。
5. 若读取目标或费用方程确实不存在合法整数解，再回退原始 usage。

## 8. TTL 与响应字段

读取并创建类别继续复用当前任务 TTL：

- 任务只选择 5 分钟或 1 小时中的一种创建 TTL。
- 同一请求不能同时产生两类创建明细。
- 现有任务 TTL 哈希和状态保留，避免无关行为变化。

Kiro-Go 同步 JSON 和流式最终 `message_delta.usage` 都返回：

```json
{
  "input_tokens": 0,
  "output_tokens": 0,
  "cache_read_input_tokens": 0,
  "cache_creation_input_tokens": 0,
  "cache_creation": {
    "ephemeral_5m_input_tokens": 0,
    "ephemeral_1h_input_tokens": 0
  }
}
```

缓存创建字段仍然由 Kiro-Go 原样生成。下游经由 Sub2API 的 `/v1/chat/completions` 转换时是否保留 1 小时明细，是独立的协议转换问题，不在本次 Kiro-Go 命中率设计内。

## 9. 错误处理与状态提交

- 只有拿到成功响应和最终原始 Token 后才执行整数分配并提交任务状态。
- 上游失败、客户端断开、流式截断和最终 usage 缺失时不提交状态。
- 已存在的请求指纹继续复用已提交 usage，确保重试不重复推进任务。
- fallback 直接返回当前原始输入和输出 Token，不伪造 `cache_read` 或 `cache_creation`。
- 任何整数溢出、负数、TTL 混用或费用不守恒都必须拒绝该候选。

## 10. 测试与验收

### 10.1 单元测试

- 目标命中率始终位于 95%–96%。
- 读取类别的创建 Token 为 0，读取 Token 大于 0。
- 读取并创建类别的创建 Token 大于 0，读取 Token 大于创建 Token。
- 5 分钟和 1 小时创建分别只产生自身字段。
- 读取/创建 Token 可以超过 5 倍，不再因旧比例限制失败。
- 大输入目标命中率不低于小输入目标命中率。
- 费用守恒方程在两种 TTL 下完全成立。
- 同一请求指纹得到相同类别和相同整数分配。
- 极小输入无法创建新增 Token 时可降级为只读取。

### 10.2 分布测试

使用至少 10,000 个固定请求指纹，验证：

- 加权累计真实命中率位于 95%–96%。
- 读取请求占比接近 100%。
- 创建请求占比位于 45%–55%，允许哈希分布自然误差。
- 单一请求类别不因任务阶段集中到某个边界。
- 不同输入规模的读取比例存在预期方向差异。

### 10.3 同步/流式合同测试

- 同一原始输入和输出在同步、流式最终事件中得到相同 usage。
- `cache_creation_input_tokens` 等于两个 TTL 字段之和。
- 不会同时出现 5 分钟和 1 小时创建。
- 截断请求不生成缓存分配。

### 10.4 UAT 验收

部署到 UAT 后收集至少 500 条完整成功记录，按 Token 加权计算：

```text
sum(cache_read_input_tokens)
/
sum(input_tokens + cache_read_input_tokens + cache_creation_input_tokens)
```

结果必须位于 95%–96%；同时单独报告读取请求占比、创建请求占比和 fallback 率。只有这三类数据都正常，才允许进入生产发布。

## 11. 回滚边界

- 算法版本号递增，旧版本可通过镜像回滚。
- 回滚只需要恢复 Kiro-Go 镜像，不触碰 Sub2API 数据库、Redis 和其他容器。
- 新旧算法产生的历史 usage 不重新改写，回滚后只影响新请求。
- UAT 和生产均保留变更前 Kiro-Go 镜像及 Compose 备份。
