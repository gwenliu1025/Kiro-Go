# Kiro-Go 缓存读取调优部署与真实流量验收

日期：2026-07-14

二号机（UAT/当前生产）：`154.36.172.65`

毕业机：`157.254.187.242`

## 结论

- Kiro-Go 调优提交
  `494dd903a6c6d6107ce65b3b380b87050b8abfc4`
  已部署到二号机和毕业机。
- 两台机器运行同一精确镜像，镜像 ID 和 OCI revision 一致。
- 二号机固定 92 条真实请求窗口中，85 条同时读取并创建缓存，6 条只创建，
  1 条因既有大 payload 截断保护回退原始 usage。
- 85 条读写记录的 `cache_read/cache_creation` 倍数均位于 `2–5`，
  平均为 `3.1406`，没有越界。
- 读取请求覆盖率为 `92.391%`；读取和创建不是互斥状态，因此创建请求覆盖率
  同时为 `98.913%`。
- 两台机器的 `kiro-go-pr131` 均为 `running/healthy`，全部本机、Docker
  内网和生产公网健康检查为 `200`。
- 本轮只重建两台机器的 `kiro-go-pr131`，没有 pull、Prepare、Activate
  或重建 Sub2API，也没有重建其他容器。

## 最小调优内容

算法版本：`native-high-cache-v2`

- 首轮和 TTL 重建阶段约 `80%` 进入读写分配，约 `20%` 只创建缓存。
- 正常续轮全部进入读写分配。
- 读写倍数按输入规模、复用率、增长率和稳定抖动计算：

```text
ratio =
  2.40
  + 0.95 * size_factor
  + 0.25 * reuse_ratio
  - 0.15 * growth_ratio
  + 0.35 * stable_jitter
```

- 最终倍数约束在 `2–5`。
- 没有增加缓存命名空间、数据库、Redis 或新的服务端状态。
- 原始输入越大，模型倾向于分配更多缓存读取；不同请求继续保留稳定差异，
  不使用统一固定比例。

## 源码与镜像

```text
提交：
494dd903a6c6d6107ce65b3b380b87050b8abfc4

源码归档：
kiro-go-cache-read-tuning-494dd90-20260714.tar.gz

源码归档 SHA256：
7093d3e1141e7a1374ee499744ae74fe76144fbedc718186d41f17ff6d3a9597

镜像：
local/kiro-go:cache-read-tuning-494dd90-20260714

镜像 ID：
sha256:8946dbcf172ee40616de9278474afd5c0aa565127dcacebe0391af6c07d7a2b8

镜像归档 SHA256：
964ad525853ef965ac959a7750346f99b24a0504b1b79ae4923f1305f171c18f

org.opencontainers.image.revision：
494dd903a6c6d6107ce65b3b380b87050b8abfc4
```

OCI 中文描述为：

```text
Kiro-Go 缓存读取覆盖率与读写倍数调优
```

## 发布门禁

本地验证：

```text
go test ./... -count=1：通过
go vet ./...：通过
go build ./...：通过
gofmt -d：无差异
git diff --check：通过
定向分配测试连续 20 轮：通过
```

毕业机隔离 Linux 容器验证：

```text
go test ./...：通过
go test -race ./...：通过
go vet ./...：通过
go build ./...：通过
```

独立复审结果：

```text
Critical：0
Important：0
```

## 二号机部署状态

```text
容器：kiro-go-pr131
镜像：local/kiro-go:cache-read-tuning-494dd90-20260714
镜像 ID：sha256:8946dbcf172ee40616de9278474afd5c0aa565127dcacebe0391af6c07d7a2b8
启动时间：2026-07-14T06:44:15.10194506Z
状态：running/healthy
```

健康检查：

```text
Kiro-Go 宿主机回环：200
Sub2API 容器访问 Kiro-Go：200
Sub2API 宿主机回环：200
Caddy 本机入口：200
生产公网：https://xiaoqian.art/health -> 200
Sub2API updater：200
```

Sub2API 保持：

```text
镜像：ghcr.io/gwenliu1025/sub2api:0.1.152
启动时间：2026-07-14T03:48:21.970064416Z
状态：running/healthy
```

二号机切换 staging：

```text
/home/ubuntu/staging/kiro-cache-read-tuning-prod-20260714-064412z
```

Compose 回滚备份：

```text
/home/ubuntu/staging/kiro-cache-read-tuning-prod-20260714-064412z/docker-compose.before-494dd90.yml
```

## 真实 UAT

固定验收窗口：

```text
起始游标：2867677
样本：游标后的前 92 条 account 1910 使用记录
样本最小 ID：2867727
样本最大 ID：2868604
```

请求覆盖：

```text
读取请求：85 / 92 = 92.391%
创建请求：91 / 92 = 98.913%
同时读取和创建：85
只创建：6
无缓存：1 / 92 = 1.087%
```

读写 token 倍数：

```text
记录数：85
最小：2.6013
平均：3.1406
中位数：3.1133
最大：3.6185
2–5 越界：0
0.01 倍数分桶：53
```

Kiro 原生高缓存记录：

```text
记录数：90
缓存率最小：96.044%
缓存率平均：97.468%
缓存率最大：98.617%
95%–99% 越界：0
```

连续 10 条请求的读取数：

```text
最少：6
平均：9.265
最多：10
```

这里记录的是自然流量的真实滑动窗口。整体读取覆盖率超过九成，但稳定哈希分配
不会强制每一个连续 10 条窗口都恰好至少读取 9 条。

TTL 和字段约束：

```text
5 分钟创建：15
1 小时创建：76
混用双 TTL：0
cache_creation 汇总不一致：0
cache_ttl_override 改写：0
旧 raw_* 写入：0
旧 usage_allocation_* 写入：0
```

## 特殊样本

### 大 payload 截断保护

记录 `2868046` 的原始输入为 `377557`，进入既有 `900 KiB` payload
截断保护路径。`TestClaudeTruncatedPayloadFallsBackWithoutCommit` 明确要求
此类请求回退原始 usage，因此它不是新分配器失败。

### Sub2API 既有强制缓存计费

记录 `2868373` 在数据库中显示为 100% 缓存率。Sub2API 日志确认其既有
`force_cache_billing` 将 `355 input_tokens` 改写为
`cache_read_input_tokens`。这不是 Kiro-Go 输出，本轮按约束不修改 Sub2API。

## 日志分类

二号机切换后日志关键词命中 13 条，均为：

```text
WARN [KiroAPI] Endpoint CodeWhisperer/Kiro IDE error: HTTP 500
```

分类结果：

```text
Kiro-Go 本地 ERROR：0
panic：0
fatal：0
```

这些记录来自上游瞬时 `HTTP 500`，不构成缓存分配或容器健康失败。

## 毕业机部署状态

```text
容器：kiro-go-pr131
镜像：local/kiro-go:cache-read-tuning-494dd90-20260714
镜像 ID：sha256:8946dbcf172ee40616de9278474afd5c0aa565127dcacebe0391af6c07d7a2b8
启动时间：2026-07-14T07:06:12.427792324Z
状态：running/healthy
```

健康检查：

```text
Kiro-Go 宿主机回环：200
Sub2API 容器访问 Kiro-Go：200
Sub2API 宿主机回环：200
Caddy 本机入口：200
Sub2API updater：200
Kiro-Go 本地 ERROR/panic/fatal：0/0/0
```

Sub2API 保持：

```text
镜像：ghcr.io/gwenliu1025/sub2api:0.1.152
启动时间：2026-07-14T02:34:10.443306229Z
状态：running/healthy
```

毕业机切换 staging：

```text
/home/ubuntu/staging/kiro-cache-read-tuning-grad-20260714-070611z
```

Compose 回滚备份：

```text
/home/ubuntu/staging/kiro-cache-read-tuning-grad-20260714-070611z/docker-compose.before-494dd90.yml
```

## 无关服务证明

- 二号机切换前后，除 `kiro-go-pr131` 外 18 个容器启动时间差异为 `0`。
- 毕业机切换前后，除 `kiro-go-pr131` 外 16 个容器启动时间差异为 `0`。
- 两台机器的 Sub2API 镜像、状态和启动时间均未变化。
- 本轮未执行 Sub2API pull、Prepare、Activate 或容器重建。

## 回滚

回滚时恢复对应机器的 Compose 备份，并仅执行：

```bash
docker compose --project-directory /home/ubuntu/kiro-go-pr131 \
  -f /home/ubuntu/kiro-go-pr131/docker-compose.yml \
  up -d --pull never --no-build --no-deps --force-recreate kiro-go-pr131
```

上一精确镜像为：

```text
local/kiro-go:native-high-cache-1b94c33-20260714
```

不要重建 Sub2API、PostgreSQL、Redis、Caddy、CPA、QQ/NapCat、
TransitHub 或其他无关服务。
