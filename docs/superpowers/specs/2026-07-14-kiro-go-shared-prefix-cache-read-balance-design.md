# Kiro-Go 公共前缀复用与缓存读写平衡设计

日期：2026-07-14

状态：已确认

## 1. 问题

生产 UAT 的真实 usage 记录显示，大量请求只有缓存创建，缓存读取明显偏少。

根因不是整数费用守恒求解失败，而是缓存生命周期的作用域过细：

- 当前任务键包含首条有效用户消息。
- 不同用户问题即使使用完全相同的模型、工具定义和系统提示，也会进入不同任务。
- 请求分析只在消息结束后生成前缀断点，没有为工具定义和系统提示组成的稳定公共前缀单独建立断点。
- 首轮和过期重建阶段固定返回 `cache_read_input_tokens=0`。

因此，高度重复的 Agent 工具和系统提示会在多个独立问题中反复被判为新缓存创建，无法作为公共前缀被后续请求读取。

## 2. 目标

- 同一调用方、模型、工具定义和系统提示组成一个缓存命名空间。
- 首条用户消息不再参与缓存命名空间键。
- 不同用户问题可以读取完全相同的工具和系统提示公共前缀。
- 同一对话仍可继续匹配更长的消息级精确前缀。
- 真正冷启动的首次请求允许只有缓存创建。
- 温缓存请求的缓存读取 token 必须高于缓存创建 token。
- 温缓存请求严格满足：

```text
2.0 <= cache_read_input_tokens / cache_creation_input_tokens <= 5.0
```

- 代表性请求分布的读取/创建平均倍数约为 `3.0`。
- 原始输入越大，在其他特征相同的情况下读取倍数越高。
- 不同请求的倍数必须分散，不能形成固定比例模板。
- 允许大部分请求同时创建少量新增缓存；10 条请求中有 6 到 10 条包含缓存创建均可接受。
- 继续保持缓存总比例、费用守恒、单 TTL、输出 token 和同步/流式合同。

## 3. 非目标

- 不伪造跨调用方或跨 API Key 的缓存命中。
- 不对不同模型复用同一缓存命名空间。
- 不进行模糊文本匹配。
- 不把用户消息、系统提示、工具参数或 API Key 原文持久化到内存状态或日志。
- 不修改 OpenAI Chat、OpenAI Responses、Gemini 或其他协议。
- 不修改 Sub2API 计费、倍率或数据库结构。
- 不重建 Sub2API 或其他无关生产服务。

## 4. 缓存命名空间

缓存命名空间键改为：

```text
caller_scope
+ normalized_model
+ canonical_tools
+ canonical_system
```

其中：

- `caller_scope` 使用认证中间件提供的 Kiro-Go API Key ID。
- `normalized_model` 使用规范化后的实际模型。
- `canonical_tools` 使用稳定键顺序的工具名称、说明和输入 schema。
- `canonical_system` 使用规范化后的系统提示内容。
- 首条用户消息不参与命名空间键。

最终只保存 SHA-256：

```text
namespace_key = SHA-256(上述内容)
```

不同 API Key、模型、工具定义或系统提示仍然相互隔离。

## 5. 前缀断点

请求分析按以下顺序增量哈希：

```text
model
-> tools
-> system
-> messages
```

新增一个公共前缀断点：

```text
prelude_prefix = model + tools + system
```

之后继续为每条消息结束位置生成精确前缀断点。

同一命名空间中的不同问题：

- 如果工具和系统提示完全相同，可以命中 `prelude_prefix`。
- 如果历史消息也完全相同，可以继续命中更长的消息级前缀。
- 如果工具或系统提示变化，会进入新的命名空间。
- 用户消息只能通过精确前缀哈希匹配，不进行跨问题的模糊复用。

状态仍然只保存哈希、累计 token、最终 usage 和时间戳。

## 6. 冷缓存与温缓存

### 6.1 冷缓存

以下情况视为冷缓存：

- 命名空间首次成功请求。
- TTL 已过期。
- 没有任何有效公共或消息级前缀。

冷缓存保持：

```text
cache_read_input_tokens = 0
cache_creation_input_tokens > 0
```

这是唯一允许创建高于读取的正常阶段。

### 6.2 温缓存

命中公共前缀或更长消息前缀后进入温缓存：

```text
cache_read_input_tokens > cache_creation_input_tokens > 0
```

温缓存读取/创建倍数由请求特征决定，不直接等于前缀估算 token 的机械相除结果。

## 7. 读取/创建倍数模型

沿用现有特征：

```text
reuse_ratio
growth_ratio
age_ratio
round_factor
size_factor
tool_ratio
stable_jitter
```

温缓存目标倍数：

```text
read_create_ratio_raw
= 2.25
+ 1.00 * size_factor
+ 0.45 * reuse_ratio
- 0.25 * growth_ratio
- 0.10 * age_ratio
+ 0.10 * round_factor
+ 0.55 * stable_jitter

read_create_ratio
= clamp(read_create_ratio_raw, 2.0, 5.0)
```

该公式满足：

- 原始输入规模越大，读取倍数越高。
- 前缀复用越多，读取倍数越高。
- 新增上下文越多，创建占比适度提高。
- 越接近 TTL 过期，创建占比适度提高。
- 稳定微扰打散相近请求，但不改变绝对边界。
- 相同请求指纹的幂等重试得到相同结果。

`size_factor` 继续使用原始输入 token 的对数压缩值，避免超长请求支配结果。

## 8. 目标比例反解

普通输入比例继续位于 `1%` 到 `5%`：

```text
cache_total = 1 - input_share
```

对于温缓存：

```text
create_share
= cache_total / (1 + read_create_ratio)

read_share
= cache_total - create_share
```

因此：

```text
read_share / create_share = read_create_ratio
```

当倍数位于 `2` 到 `5` 时，温缓存的大致范围为：

```text
cache_creation_share = 约 16% 到 33%
cache_read_share     = 约 63% 到 83%
```

最终整数求解后必须重新校验实际 token 倍数，而不能只校验浮点目标。

## 9. 保留的硬约束

所有成功转换继续满足：

```text
95% <= cache_rate <= 99%

cache_creation_input_tokens
= ephemeral_5m_input_tokens
+ ephemeral_1h_input_tokens
```

每个命名空间只使用一种创建 TTL：

```text
约 20% -> 5m
约 80% -> 1h
```

费用守恒继续使用：

```text
20 * raw_input
= 20 * input
+ 2  * cache_read
+ 25 * cache_create_5m
+ 40 * cache_create_1h
```

同时：

- 输出 token 不变。
- 同步 JSON 和流式最终 `message_delta.usage` 使用同一结果。
- 求解候选不超过 `64`。
- 无合法整数解时回退原始 usage。

## 10. 状态兼容

算法版本从：

```text
native-high-cache-v1
```

提升为：

```text
native-high-cache-v2
```

服务重启后旧内存状态自然清空。新版本不会把旧任务键状态误认为新的公共前缀状态。

## 11. 测试

必须先增加失败测试，再修改生产代码。

### 11.1 请求分析

- 相同 API Key、模型、工具和系统提示，仅首条用户消息不同，命名空间键相同。
- API Key、模型、工具或系统提示任一变化，命名空间键不同。
- 请求分析生成独立公共前缀断点。
- 后续消息变化只改变请求指纹和消息级前缀，不改变命名空间键。

### 11.2 状态与命中

- 第一个请求为冷缓存。
- 第二个不同问题但公共前缀相同的请求命中温缓存。
- 不同系统提示或工具定义不能命中。
- 同一对话继续增长时优先选择最长消息级前缀。
- TTL 过期后重新进入冷缓存。
- 相同请求重试继续复用原 usage。

### 11.3 读写平衡

- 每个温缓存整数结果严格满足读取/创建倍数 `2.0` 到 `5.0`。
- 代表性特征样本的平均倍数位于 `2.8` 到 `3.2`。
- 相同其他特征下，大输入的读取倍数高于小输入。
- 代表性样本产生足够多的比例桶，不堆积成固定值。
- 冷缓存仍然只有创建。
- 缓存总比例、单 TTL 和整数费用守恒全部通过。

### 11.4 合同与回归

- 同步和流式最终 usage 一致。
- OpenAI 和其他协议不受影响。
- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `go build ./...`

## 12. 生产 UAT 验收

生产机 `154.36.172.65` 是本轮 UAT 环境。部署时：

- 先备份当前 Kiro-Go Compose。
- 构建精确提交和精确镜像标签。
- 只重建 `kiro-go-pr131`。
- 不重建 Sub2API、PostgreSQL、Redis、Caddy、CPA、QQ/NapCat、
  TransitHub 或其他服务。
- 比较切换前后无关容器启动时间。

真实流量至少观察 30 条切换后成功 usage，并按缓存命名空间区分冷启动和温缓存。

验收门槛：

```text
温缓存记录 cache_read > cache_creation：100%
温缓存读取/创建倍数位于 2 到 5：100%
温缓存平均读取/创建倍数：2.8 到 3.2
同类请求中原始输入越大，读取倍数总体越高
缓存总比例位于 95% 到 99%：100%
双 TTL 混用：0
旧 Sub2API 分配字段写入：0
```

整体读取命中率单独报告。每个命名空间的第一个冷启动请求不作为模型失败；
公共前缀预热后仍持续出现大量只创建记录则验收失败。

生产 UAT 通过后，将同一精确镜像同步到毕业机，并只重建毕业机
`kiro-go-pr131`。

## 13. 回滚

回滚时：

- 恢复切换前 Compose 备份引用的上一精确镜像。
- 只重建 `kiro-go-pr131`。
- 不修改 Sub2API 数据库、账户、价格或路由。
- 回滚后重新检查 Kiro-Go 回环、Sub2API 内网访问和无关容器启动时间。
