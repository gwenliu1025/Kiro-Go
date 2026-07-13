# Kiro-Go 原生高缓存用量与 Sub2API 旧实现清退设计

日期：2026-07-14

状态：设计方向已确认，等待书面规格复核

## 1. 结论

本次改造拆成两个方向，但共同遵守一条职责边界：

1. Kiro-Go 负责把 Kiro 原始输入用量转换为标准 Anthropic 高缓存 `usage`。
2. Sub2API 不再进行任何 Equivalent Cache V1/V2 二次分配，只解析 Kiro-Go 返回的标准字段，并沿用原生价格、分组倍率和扣费链路。

目标数据流为：

```text
客户端
  -> Sub2API
  -> Kiro-Go
  -> Kiro 上游
  -> Kiro-Go 生成标准 Anthropic usage
  -> Sub2API 原生解析 usage
  -> 按模型价格计算基础费用
  -> 应用用户和分组有效倍率
  -> 返回并记录最终费用
```

这样可以同时满足：

- 下游 Sub2API、NewAPI 或其他兼容面板能收到真实存在于 HTTP 响应中的缓存字段。
- 同步与流式请求使用同一套转换结果。
- 账号切换分组只改变分组倍率，不改变高缓存资格。
- Kiro-Go 账号池换源不再导致缓存生命周期中断。
- Sub2API 后续合并原作者更新时，不需要持续维护一整套侵入式缓存分配器。

## 2. 背景

现有实现把高缓存分配放在 Sub2API 内部，已经产生以下问题：

- V1/V2 逻辑侵入响应改写、异步计费、账号 Extra、Redis 状态、用量审计和 Ent 生成代码。
- 同步与流式路径曾经出现不同执行时点，导致流式请求没有按最终用量拆分。
- V2 资格曾与分组价格档案强绑定，账号切换分组后静默失效。
- 下游只能看到 Sub2API 自己改写后的结果，职责边界混乱。
- 每次新增账号、调整资格或合并上游，都可能要求重新修改并发布 Sub2API。

Kiro-Go 当前已经具备 Anthropic usage 构造能力，但存在以下限制：

- 只有请求显式携带 `cache_control` 时才建立缓存断点。
- 状态按选中的 Kiro 上游账号 ID 隔离，账号池换源会丢失命中关系。
- 当前算法直接按提示词 token 划分，没有按官方相对价格保持原始输入费用不变。
- 同步与流式分别组装 usage，缺少统一的最终分配对象。
- 默认监听端口仍为 `8080`，与本项目已经确认的 `8321` 拓扑不一致。

## 3. 目标

### 3.1 Kiro-Go

- 所有 Anthropic `/v1/messages` 请求默认启用高缓存 usage，不增加账号开关或功能开关。
- OpenAI Chat、OpenAI Responses 和其他协议不受影响。
- 不要求客户端传递 `cache_control`。
- 自动识别同一调用方的 Agent 任务和增长中的对话前缀。
- 首轮以缓存创建为主，后续请求读取旧前缀并创建新增上下文。
- 每个任务只使用一种缓存创建 TTL。
- 约 `20%` 的任务使用 5 分钟创建，约 `80%` 的任务使用 1 小时创建。
- 缓存相关 token 占输入侧展示 token 的 `95%` 到 `99%`。
- 输出 token 始终保持 Kiro-Go 原始值。
- 按 Anthropic 官方相对价格单位精确守恒原始输入费用。
- 同步 JSON 和流式最终 `message_delta.usage` 返回相同的标准字段。
- 状态按 Kiro-Go API Key 调用方隔离，不按 Kiro 上游账号隔离。
- 容器内监听、Docker 内网访问和宿主机回环端口统一为 `8321`。

### 3.2 Sub2API

- 删除 Equivalent Cache V1/V2 的运行时代码、配置入口、状态管理、响应改写、锁价和专属审计写入。
- 保留原生 Anthropic usage 解析、模型定价、分组倍率和正常扣费。
- 不删除已经执行过的生产数据库列，不新增破坏性 `DROP COLUMN` 迁移。
- Kiro 账号关闭 `cache_ttl_override`，避免 Sub2API 再次改变 5 分钟和 1 小时分类。
- 清理 README、运维说明、旧设计和旧实施计划中的废弃 V1/V2 内容。
- 不回退原作者合并、Docker 更新器、中文发布规范和其他 Fork 功能。

## 4. 非目标

- 不让 Kiro 上游真正获得 Anthropic Prompt Cache 能力。
- 不修改 Kiro 模型输出、正文、thinking、工具调用或停止原因。
- 不让 Sub2API 根据账号、分组或响应头再次生成高缓存用量。
- 不为 Kiro-Go 增加数据库、Redis 或外部状态服务。
- 不让一个任务同时分配 5 分钟和 1 小时缓存创建。
- 不修改 OpenAI、Gemini、图片、视频或按次计费链路。
- 不创建 Sub2API `0.1.153` 版本。
- 不删除生产中已经存在的 Equivalent Cache 审计列。

## 5. 官方 usage 与价格合同

本设计采用 Anthropic Prompt Caching 的标准 usage 结构：

```text
usage.input_tokens
usage.output_tokens
usage.cache_read_input_tokens
usage.cache_creation_input_tokens
usage.cache_creation.ephemeral_5m_input_tokens
usage.cache_creation.ephemeral_1h_input_tokens
```

必须满足：

```text
cache_creation_input_tokens
= ephemeral_5m_input_tokens
+ ephemeral_1h_input_tokens
```

费用守恒采用以下相对价格整数单位：

```text
普通输入             = 20
缓存读取             = 2
5 分钟缓存创建       = 25
1 小时缓存创建       = 40
```

它们分别对应基础输入价格的：

```text
普通输入             = 1.00 倍
缓存读取             = 0.10 倍
5 分钟缓存创建       = 1.25 倍
1 小时缓存创建       = 2.00 倍
```

Anthropic 官方文档核验日期为 2026-07-14：

`https://platform.claude.com/docs/en/build-with-claude/prompt-caching`

Sub2API 中目标模型的五类价格必须保持上述相对关系。以 Opus 4.6 为例，应使用：

```text
输入                 = $5.00 / MTok
输出                 = $25.00 / MTok
缓存读取             = $0.50 / MTok
5 分钟缓存创建       = $6.25 / MTok
1 小时缓存创建       = $10.00 / MTok
```

不得保留此前为 V2 单独设置的 Opus 缓存读取 `$0.60 / MTok` 覆盖，否则 Go 端按官方比例守恒后，Sub2API 会按非官方比例重新定价，基础费用将不再相等。

## 6. 两仓库职责边界

### 6.1 Kiro-Go 是 usage 的唯一生成方

Kiro-Go 从 Kiro 上游得到原始输入和输出 token 后，生成一份最终 `ClaudeUsage`。该对象是同步和流式响应的唯一数据源。

Kiro-Go 不知道用户位于 Sub2API 的哪个分组，也不接收分组倍率。它只保证：

```text
官方相对价格下的新输入侧费用
=
原始普通输入费用
```

### 6.2 Sub2API 是价格和倍率的唯一执行方

Sub2API 收到标准 Anthropic usage 后：

1. 按当前模型价格计算输入、输出、缓存读取和缓存创建基础费用。
2. 计算用户专属、分组默认和系统默认中的有效倍率。
3. 在基础费用之后应用有效倍率。

公式为：

```text
用户实际费用 = 守恒后的基础费用 × 有效分组倍率
```

例如同一条请求的守恒基础费用为 4 元：

```text
0.1 倍分组 -> 0.4 元
0.2 倍分组 -> 0.8 元
```

这正是用户被放入更高倍率分组后收费更高的预期行为。高缓存资格不与分组绑定，只有最终价格倍率与分组有关。

## 7. Kiro-Go 任务生命周期设计

### 7.1 自动建立缓存轮廓

重构现有 `promptCacheTracker`，不再要求显式 `cache_control` 才建立断点。

按 Anthropic 前缀顺序归一化请求：

```text
model
-> tools
-> system
-> messages
```

系统提示、工具定义和每条消息结束位置都可以成为自动前缀断点。归一化时：

- 保留会影响上下文语义的模型、工具、系统提示、消息、图片、工具调用和工具结果。
- 排除 `cache_control`，因为客户端标记不再决定本功能是否启用或使用哪种 TTL。
- 排除用于内部定位的数组索引、临时请求 ID、计费头和其他不影响语义的瞬时字段。
- 使用稳定 JSON 键顺序和 SHA-256 哈希。
- 只保存哈希和 token 估算，不保存提示词原文、图片内容、API Key 或其他敏感数据。

现有最小缓存 token 阈值不再作为高缓存 usage 的启用门槛。这里生成的是 Kiro 的等价高缓存 usage，不是声称 Kiro 上游真实创建了 Anthropic 缓存。极小请求是否能够转换，只由整数守恒求解是否存在合法解决定。

### 7.2 调用方隔离

一级隔离键使用已经由认证中间件写入 Context 的 Kiro-Go API Key ID：

```text
caller_scope = api_key_id
```

选中的 Kiro 上游账号 ID 不得参与任务键、缓存命中或 TTL 类型计算。一次任务即使在账号池中换源，生命周期也必须连续。

缺少 API Key ID 的兼容调用使用固定匿名作用域，并继续由任务根哈希隔离。日志不得输出原始 API Key。

### 7.3 任务键

任务键使用以下稳定信息：

```text
API Key ID
+ 归一化模型
+ 归一化工具定义
+ 归一化系统提示
+ 首条非空用户消息
```

最终只保存：

```text
task_key = SHA-256(上述内容)
```

工具定义、系统提示或首条用户消息发生变化时，视为新任务并重新进入首轮创建。后续消息增长不会改变任务键。

### 7.4 请求指纹与重试幂等

每次请求还生成完整请求指纹：

```text
request_fingerprint = SHA-256(完整归一化请求)
```

同一任务内相同指纹在有效期内重复出现时，复用已生成的高缓存分配结果：

- Kiro 上游账号失败后的内部换源不改变 usage。
- 客户端因网络问题重试同一请求时，不把同一轮误判为下一轮。
- 同步和流式使用相同纯函数时，同一输入得到相同结果。

只有 Kiro 请求成功并取得最终原始输入 token 后，才提交任务状态。失败请求不得推进轮次、创建时间或前缀状态。

### 7.5 最长旧前缀匹配

后续请求在同一调用方和任务内查找最长的已成功前缀：

```text
旧前缀估算 token -> 缓存读取候选
新增上下文估算 token -> 缓存创建候选
最后未缓存部分 -> 普通输入候选
```

如果没有任何旧前缀，则按首轮创建处理。前缀匹配遵守精确哈希，不做模糊文本匹配。

## 8. TTL 分配与状态过期

### 8.1 每个任务只选择一种创建 TTL

TTL 类型由任务键和算法版本进行稳定哈希：

```text
hash(task_key + algorithm_version) % 100
```

分配规则：

```text
0  到 19 -> 5 分钟缓存创建
20 到 99 -> 1 小时缓存创建
```

因此长期任务分布约为：

```text
5 分钟 : 1 小时 = 2 : 8
```

同一任务生命周期内不得改变 TTL，也不得在同一请求中同时返回两种创建明细。

### 8.2 有效期

- 5 分钟任务只有在距上次成功缓存活动不超过 5 分钟时才能继续读取。
- 5 分钟任务命中读取后刷新 5 分钟活动窗口。
- 1 小时任务只有在距最近一次成功创建不超过 60 分钟时才能读取。
- 超出任务 TTL 后，下一次请求重新按首轮创建处理。
- 内存状态最多保留 70 分钟，用于垃圾回收和幂等记录清理。

服务重启后内存状态清空，下一次请求按首轮创建处理。这是可接受降级，不引入 Redis 或数据库依赖。

## 9. 动态比例设计

### 9.1 缓存总比例

每条成功转换请求必须满足：

```text
cache_rate
= (cache_read + cache_creation)
  / (input + cache_read + cache_creation)
```

并且：

```text
95% <= cache_rate <= 99%
```

具体目标值由任务键、请求指纹和算法版本的稳定哈希在区间内确定，不使用进程随机数。

### 9.2 首轮请求

首轮没有可读取的旧前缀：

```text
普通输入             = 1% 到 5%
缓存读取             = 0
缓存创建             = 95% 到 99%
```

首轮只使用该任务已经选定的一种创建 TTL。

### 9.3 后续请求

后续请求先根据最长旧前缀计算自然增长率：

```text
growth_rate
= 新增上下文估算 token
  / 当前可缓存上下文估算 token
```

普通输入比例继续保持 `1%` 到 `5%`。缓存创建比例以自然增长率为基础，但限制在能够形成真实 Agent 任务分布的区间：

```text
缓存创建             = 8% 到 20%
缓存读取             = 78% 到 90%
普通输入             = 1% 到 5%
```

计算方式为：

```text
cache_total = 1 - input_share

create_min = max(8%, cache_total - 90%)
create_max = min(20%, cache_total - 78%)

create_share = clamp(
  cache_total * growth_rate,
  create_min,
  create_max
)

read_share = cache_total - create_share
```

这样每个正常增长的后续请求都会携带一定缓存创建，不再出现大量纯读取记录导致缓存读取数字异常突出的情况。

### 9.4 完整任务观察目标

对于至少 4 轮的正常 Agent 任务，累计观察目标大致为：

```text
缓存读取             = 78% 到 90%
缓存创建             = 8% 到 20%
普通输入             = 1% 到 5%
```

一轮或两轮的短任务会因为首轮必须创建而具有更高的累计创建比例，这属于预期行为，不能为了追求累计比例而在首轮虚构缓存读取。

## 10. 整数费用守恒求解

本次不改造 Kiro-Go 现有输入 token 的来源和估算优先级。`T` 指高缓存转换之前，当前 Kiro-Go
响应链路最终准备写入 `input_tokens` 的值：

1. 优先使用 Kiro 上游回传并由现有逻辑换算的上下文用量。
2. 上游没有可用值时，沿用当前请求 token 估算结果。

因此，守恒目标是“改造前后的 Kiro-Go 基础费用一致”，不是在本项目中另外声明 Kiro 上游提供了此前不存在的精确 token 计数。

设该原始输入 token 为 `T`，新 usage 为：

```text
I  = input_tokens
R  = cache_read_input_tokens
C5 = ephemeral_5m_input_tokens
C1 = ephemeral_1h_input_tokens
```

必须满足：

```text
20 * T
= 20 * I
+ 2  * R
+ 25 * C5
+ 40 * C1
```

同时：

```text
C5 == 0 或 C1 == 0
cache_creation_input_tokens = C5 + C1
output_tokens = 原始 output_tokens
```

求解过程：

1. 生命周期估算层给出输入、读取和创建的目标比例。
2. 根据该任务的 TTL 类型选择创建单价 `25` 或 `40`。
3. 计算靠近目标比例的整数初值。
4. 在输入和创建候选附近进行有界整数搜索。
5. 由费用方程反解缓存读取 token。
6. 校验所有 token 非负、TTL 单一、缓存率在 `95%` 到 `99%`、比例在当前轮次允许范围。
7. 使用独立计算路径重新计算左右费用，要求整数完全相等。

5 分钟创建的整数解还必须满足相应整除条件。求解器不得使用浮点误差容忍作为最终守恒依据。

出现以下任一情况时，整条请求返回原始 usage：

- 原始输入 token 小于等于 0。
- 目标区间内不存在精确整数解。
- 乘法或加法可能溢出。
- 任一结果为负数。
- 缓存率或生命周期比例越界。
- 创建汇总不等于 TTL 明细之和。
- 输出 token 发生变化。

禁止通过修改输出 token 补偿输入侧舍入差额。

降级请求不提交高缓存任务状态。下一次满足条件的请求仍从最近一次成功提交的状态继续；如果此前从未成功提交，则仍按首轮创建处理。

## 11. 同步与流式响应合同

### 11.1 统一最终对象

新增一个不依赖 HTTP 写出的纯分配函数：

```text
raw input/output
+ lifecycle snapshot
+ request fingerprint
-> final ClaudeUsage
```

同步和流式路径都只能消费该函数的结果，不得各自重新计算比例。

### 11.2 同步 JSON

成功转换时返回完整字段：

```json
{
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cache_read_input_tokens": 0,
    "cache_creation_input_tokens": 0,
    "cache_creation": {
      "ephemeral_5m_input_tokens": 0,
      "ephemeral_1h_input_tokens": 0
    }
  }
}
```

示例中的数值只表示字段结构，实际值由求解器产生。即使某个缓存字段为 0，也要保证整体结构能够被 Sub2API 标准解析器稳定识别。

### 11.3 流式 SSE

- `message_start.message.usage` 只保留开始阶段可用的估算值，不作为最终计费依据。
- 不在 `message_start` 提前推进任务状态。
- Kiro 流结束并得到最终原始输入和输出 token 后，调用与同步相同的纯分配函数。
- 最终 `message_delta.usage` 返回完整标准字段。
- `message_delta.usage.output_tokens` 必须是最终真实输出值。
- 只有最终 usage 成功写出后，才提交任务状态。

Sub2API 已有流式解析器会从最终 `message_delta.usage` 更新输入、输出、读取和创建字段。本次清退 V2 时必须保留这一原生解析能力。

### 11.4 客户端断开

客户端断开后，Kiro-Go 沿用现有策略继续排空上游，以取得完整 usage 和维护账号池统计。

如果最终 usage 已经取得：

- 可以提交任务状态。
- 不要求已经断开的客户端收到最终事件。

如果上游失败或没有取得最终 usage：

- 不提交任务状态。
- 不生成高缓存结果。

## 12. 并发、换源与性能

- 全局状态表只在读取快照、命中幂等结果、提交成功和清理过期项时短暂加锁。
- 不得在 Kiro 上游网络调用或整个 SSE 生命周期中持有全局锁。
- 同一任务的并发请求只能读取已经成功提交的旧前缀。
- 如果首个请求尚未成功创建缓存，其他并发请求同样表现为创建，这是符合真实缓存尚不可读的行为。
- 内部换源重试复用同一个准备结果，不因选中不同 Kiro 账号重新计算。
- 状态按任务和请求指纹有界保存，定时清理过期任务和幂等结果。
- 算法不创建额外线程池，不在请求高峰时进行无界搜索。

因此，本改造不会因为请求多而产生全局串行堵塞。主要计算均为哈希、token 估算和有界整数求解。

## 13. 降级与可观测性

任何异常均优先保证响应可用和计费可解释：

```text
高缓存转换失败 -> 原始 input/output usage
```

日志只记录：

- 算法版本。
- 任务哈希前缀。
- 请求哈希前缀。
- 首轮、读取增长轮或过期重建类型。
- 5 分钟或 1 小时 TTL 类型。
- 原始输入和最终四类输入 token。
- 守恒校验结果。
- 降级原因枚举。

不得记录：

- API Key 原文。
- 系统提示、用户消息、工具参数或图片内容。
- Kiro 凭据、Sub2API 密钥或其他认证信息。

高缓存字段必须是标准 usage 的一部分。不得要求 Sub2API 依赖自定义响应头才能计费。

## 14. Sub2API 定向清退范围

Sub2API 仓库不能直接整体回滚一串历史提交，因为这些提交之间包含原作者上游合并、Docker 更新器、中文发布规则和其他正常修复。

实施时以 `684a3f6b^` 作为 Equivalent Cache 引入前的参考基线，对专属代码块进行定向恢复，并保留后续正常上游变化。

### 14.1 删除专属模块

删除以下 Equivalent Cache 专属实现及测试：

```text
backend/internal/service/equivalent_cache_billing*.go
backend/internal/service/equivalent_cache_v2_*.go
backend/internal/repository/equivalent_cache_v2_state*.go
```

删除的能力包括：

- V1 固定比例分配。
- V2 生命周期和整数分配器。
- Redis 会话状态。
- 账号 Extra 启用资格。
- 动态价格快照。
- Sub2API 响应二次改写。
- 原始费用锁定和双 usage 计费。
- V2 影子模式与专属审计运行时。
- `usage_allocation_version` 和 `usage_allocation_kind` 的运行时写入。

### 14.2 清理共享接入点

从以下共享文件中只删除 Equivalent Cache 专属字段、分支和调用，保留文件中的所有原生和上游逻辑：

```text
backend/internal/service/gateway_service.go
backend/internal/service/gateway_forward.go
backend/internal/service/gateway_anthropic_passthrough.go
backend/internal/service/gateway_upstream_response.go
backend/internal/service/gateway_usage_billing.go
backend/internal/handler/gateway_handler.go
backend/internal/service/failover_loop.go
```

还需清理：

- `ForwardResult` 中只服务于 V2 的原始 usage、分配结果和价格快照字段。
- handler、DTO、repository 和 Ent 生成文件中的 V2 审计读写。
- V2 专属调度、日志、告警和测试夹具。

清退时必须保留：

- `parseSSEUsagePassthrough` 对最终 `message_delta.usage` 的标准解析。
- `parseClaudeUsageFromResponseBody` 对同步 Anthropic usage 的标准解析。
- `cache_creation_input_tokens` 的标准汇总解析。
- `cache_creation.ephemeral_5m_input_tokens` 和 `ephemeral_1h_input_tokens` 明细解析。
- 原生 `ForceCacheBilling` 功能，除非确认它完全属于 V1/V2；不得因名称相似误删其他业务功能。
- 原生 `cache_ttl_override` 功能本身，只关闭 Kiro 目标账号的配置。
- 分组倍率、用户专属倍率、高峰倍率和普通费用计算。

### 14.3 数据库历史兼容

保留已经发布和可能已经执行的迁移：

```text
backend/migrations/174_usage_log_equivalent_cache_v2_audit.sql
```

原因：

- 生产数据库已经可能存在这些列。
- 删除迁移会导致新旧数据库初始化历史分叉。
- 新增 `DROP COLUMN` 会扩大风险并破坏历史兼容。

运行时代码不再读写这些列。已有列允许长期为空闲兼容列，后续只有在单独的数据治理项目中才能讨论删除。

Ent schema 和生成代码中的 V2 字段可以移除，因为 PostgreSQL 允许存在应用 ORM 未声明的额外列。迁移文件保持不可变历史。

### 14.4 配置清理

对所有 Kiro-Go 类型账号：

- 删除 `equivalent_cache_allocation_v2` Extra 配置。
- 删除旧 V1 开关。
- 关闭账号级 `cache_ttl_override`。
- 不再按账号或分组设置高缓存资格。
- 模型价格恢复为官方缓存比例。

配置清理前必须备份账号 Extra、分组价格和渠道价格。该步骤是部署配置变更，不需要恢复 V2 运行时代码。

### 14.5 文档清理

从 Sub2API 删除或重写以下废弃内容：

```text
docs/EQUIVALENT_CACHE_BILLING_CN.md
docs/superpowers/specs/2026-07-12-kiro-go-cost-locked-equivalent-cache-v2-design.md
docs/superpowers/specs/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix-design.md
docs/superpowers/specs/2026-07-14-equivalent-cache-v2-account-pricing-and-kiro-internal-network-design.md
docs/superpowers/plans/2026-07-12-kiro-go-cost-locked-equivalent-cache-v2.md
docs/superpowers/plans/2026-07-13-equivalent-cache-v2-streaming-final-usage-fix.md
```

同时清理：

- 根 `README.md` 中声称 Sub2API 自身提供 Equivalent Cache 的内容。
- `skills/sub2api-admin` 或其他运维说明中的 V2 开关、账号资格、Redis 状态和专属审计操作。
- `CURRENT_STATE.md` 中已失效的 V2 当前运行描述，只保留必要历史结论。

Kiro-Go 的 `README.md` 和 `README_CN.md` 增加中文 Fork 差异说明，明确高缓存 usage 由 Kiro-Go 返回。

## 15. 8321 网络拓扑

目标拓扑固定为：

```text
Sub2API 容器
  -> http://kiro-go-pr131:8321
  -> Kiro-Go 容器内监听 8321

宿主机本地诊断
  -> http://127.0.0.1:8321
```

Kiro-Go Compose 端口：

```text
127.0.0.1:8321:8321
```

需要同步修改：

- Kiro-Go 默认配置端口。
- 已持久化生产配置中的端口。
- `Dockerfile EXPOSE`。
- `docker-compose.yml`。
- README 示例和 curl 地址。
- Sub2API Kiro 账号 Base URL。
- 健康检查与部署脚本。

Kiro-Go 同时加入 `sub2api_sub2api-network`，并使用固定别名 `kiro-go-pr131`。不得再通过生产公网 IP 绕回宿主机端口。

公网不得直接访问 `8321`。

## 16. 测试设计

### 16.1 Kiro-Go 单元测试

- 没有 `cache_control` 的 Anthropic 请求仍自动建立轮廓。
- OpenAI 请求完全不进入高缓存转换。
- 任务键按 API Key 隔离，不包含 Kiro 上游账号 ID。
- 同一任务换源后继续匹配旧前缀。
- 工具、系统提示或首条用户消息变化后建立新任务。
- 相同完整请求指纹复用同一分配结果。
- 首轮只有创建，没有读取。
- 后续轮同时存在读取和单一 TTL 创建。
- 长期任务 TTL 分布稳定接近 `20% / 80%`。
- 一个任务永远不同时出现 5 分钟和 1 小时创建。
- 缓存总比例始终位于 `95%` 到 `99%`。
- 正常后续轮读取位于 `78%` 到 `90%`，创建位于 `8%` 到 `20%`。
- 5 分钟和 1 小时两类求解都满足整数费用完全相等。
- 输出 token 始终不变。
- 极小输入、无解、溢出和非法输入回退原始 usage。
- 5 分钟活动窗口、1 小时有效期和 70 分钟清理正确。
- 并发测试通过 `go test -race`。

### 16.2 Kiro-Go 响应合同测试

- 同步响应返回六类标准 usage 字段。
- 流式最终 `message_delta.usage` 返回相同字段。
- 同一原始 usage 和生命周期快照下，同步与流式结果完全一致。
- `cache_creation_input_tokens` 等于两个 TTL 明细之和。
- message 正文、thinking、工具调用、停止原因和请求 ID 不受影响。
- 内部换源重试不改变 usage。
- 客户端断开后能排空上游并安全提交最终状态。

### 16.3 Sub2API 清退回归测试

- 后端不存在 Equivalent Cache V1/V2 专属模块引用。
- 账号 Extra 不再影响 Anthropic usage。
- 同步标准 usage 能正确解析四类输入 token。
- 流式最终 `message_delta.usage` 能覆盖最终输入、输出和缓存 token。
- 5 分钟和 1 小时创建按各自价格计费。
- Kiro 账号关闭 `cache_ttl_override` 后分类不被改写。
- 相同 usage 在不同分组中只因有效倍率而产生不同费用。
- 普通 Anthropic、OpenAI、Gemini、图片、视频和按次计费回归通过。
- 数据库带历史审计列和不带旧运行数据时都能启动。
- 迁移唯一性和完整迁移测试通过。

### 16.4 跨服务真实链路测试

至少覆盖：

```text
客户端 -> Sub2API -> Kiro-Go
客户端 -> Sub2API -> Kiro-Go -> 下游 Sub2API
客户端 -> Sub2API -> Kiro-Go -> 下游 NewAPI
```

每条链路至少验证：

- 同步请求。
- 流式请求。
- 首轮创建。
- 后续读取加创建。
- 5 分钟任务。
- 1 小时任务。
- Kiro 上游账号换源。
- 两个不同分组倍率。

## 17. 实施顺序

本设计通过书面复核后，实施计划应按以下顺序拆解：

1. 在 Kiro-Go 为任务识别、比例、费用守恒和响应合同编写失败测试。
2. 重构缓存轮廓和状态键，去掉显式 `cache_control` 依赖。
3. 实现生命周期比例与单 TTL 分配。
4. 实现整数费用守恒纯函数。
5. 统一同步和流式最终 usage。
6. 修改 Kiro-Go 端口与 Docker 网络文档。
7. 完成 Kiro-Go 单元、竞态和响应合同测试。
8. 在 Sub2API 编写清退后的原生解析与计费回归测试。
9. 定向删除 V1/V2 模块和共享接入点。
10. 保留历史迁移，移除运行时审计字段使用。
11. 清理 Sub2API 文档、README 和运维说明。
12. 运行两仓完整测试和构建。
13. 在毕业机备份并部署 Kiro-Go，执行真实链路验收。
14. 在毕业机清理 Sub2API 配置并部署清退版本。
15. 记录镜像摘要、提交、真实 usage 和费用复算结果。
16. 生产变更前再次备份并取得用户明确授权。
17. 生产只定向重建 Kiro-Go 和 Sub2API，不重建无关服务。

## 18. 发布与版本约束

- 所有新增设计文档、实施计划、README、提交说明和发布说明使用中文。
- Kiro-Go 使用自身仓库版本体系发布，不借用 Sub2API 版本号。
- Sub2API 如需重新构建清退后的资产，继续使用用户已确认的 `0.1.152`，不得创建 `0.1.153`。
- 覆盖已有 `0.1.152` 资产前，必须归档旧标签、Release 资产校验和镜像摘要。
- GitHub Release、二进制资产、`checksums.txt`、GHCR 镜像和源代码提交必须指向同一提交。
- 生产不得使用 `latest` 或其他浮动标签。

## 19. 部署、验收与回滚

### 19.1 毕业机

部署前备份：

- Kiro-Go 配置和 Compose。
- Sub2API Compose、环境变量、账号 Extra 和相关分组价格。
- 当前精确镜像摘要。
- 数据库迁移状态和必要数据库备份。

验收至少观察一组完整的多轮 Agent 任务，确认：

```text
首轮                     = 创建为主、读取为 0
后续轮                   = 读取旧前缀并创建新增上下文
缓存总比例               = 95% 到 99%
后续读取比例             = 78% 到 90%
后续创建比例             = 8% 到 20%
单任务 TTL               = 只出现 5m 或只出现 1h
创建 TTL 长期分布        = 约 2:8
输入侧官方相对费用差额   = 0
输出 token 差额          = 0
同步和流式字段差异       = 0
分组倍率计算差异         = 仅符合倍率预期
```

### 19.2 生产

生产变更必须再次获得用户明确授权。变更时：

- 只定向重建 Kiro-Go 和 Sub2API。
- 不重启 PostgreSQL、Redis、Caddy、CPA、QQ/NapCat 或其他无关服务。
- 记录变更前后所有无关容器启动时间。
- 先验证 Docker 内网 `kiro-go-pr131:8321`。
- 再验证宿主机 `127.0.0.1:8321`。
- 最后执行真实同步和流式模型请求。

### 19.3 回滚

Kiro-Go 回滚：

- 恢复上一精确镜像。
- 恢复配置和 Compose 中的旧端口。
- 恢复 Sub2API 账号 Base URL。
- 只重建 Kiro-Go。

Sub2API 回滚：

- 恢复上一精确 Fork 镜像。
- 只重建 Sub2API。
- 不删除历史审计列。

配置回滚：

- 恢复账号 Extra、分组价格和 `cache_ttl_override` 备份。

代码、网络和配置必须能够独立回滚，避免把多个故障面绑定成一次全量回退。

## 20. 完成标准

只有同时满足以下条件，本项目才算完成：

- Kiro-Go 对所有 Anthropic `/v1/messages` 默认生成高缓存 usage。
- Kiro-Go 不再依赖客户端 `cache_control`。
- 状态按 API Key 和任务绑定，不按 Kiro 上游账号或 Sub2API 分组绑定。
- 首轮创建、后续读取加创建符合任务生命周期。
- 每个任务只使用一种创建 TTL，长期分布约为 `2:8`。
- 缓存总比例稳定在 `95%` 到 `99%`。
- 官方相对价格整数费用差额为 0。
- 输出 token 始终不变。
- 同步和流式最终 usage 完全一致。
- 下游面板能收到并展示标准缓存字段。
- Sub2API 不再包含 Equivalent Cache V1/V2 运行时。
- Sub2API 原生 usage 解析、价格和分组倍率继续正常工作。
- Kiro 账号不再启用 `cache_ttl_override`。
- 已执行数据库列被安全保留但不再读写。
- Kiro-Go 容器内外和 Docker 内网统一使用 `8321`。
- 两仓测试、构建、毕业机真实链路和生产真实链路全部通过。
- README、设计、实施、发布和运维产出均使用中文。

## 21. 取代关系

本设计取代 Sub2API 仓库中以下旧方向：

- Equivalent Cache V1。
- Equivalent Cache V2。
- V2 流式最终 usage 修复设计。
- V2 账号资格、动态价格和 Kiro 内网修复设计。

旧文档只用于 Git 历史追溯，不再作为实施依据。后续唯一有效方向是：

```text
Kiro-Go 原生生成标准高缓存 usage
+ Sub2API 原生解析、正常定价和分组倍率
```
