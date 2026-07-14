# Kiro-Go 原生高缓存与 Sub2API 清退版毕业机验收

日期：2026-07-14

验收主机：`157.254.187.242`

生产机 `154.36.172.65` 未访问、未备份、未修改。

## 结论

- Kiro-Go 已以提交 `701033bea1b662f101ab309b297df09b0457d5d2`
  构建并只重建 `kiro-go-pr131`。
- Kiro-Go 容器监听、Docker 内网和宿主机回环端口已统一为 `8321`。
- Sub2API 已以提交
  `06a25c999c3d0cde157d30cde694fba2960e6d10`
  构建为应用版本 `0.1.152`，并只重建 `sub2api`。
- account `1910` 已原子切换到 `http://kiro-go-pr131:8321`，旧
  Equivalent Cache Extra 已删除，`cache_ttl_override_enabled=false`。
- account `1910` 仍保持 `schedulable=false`，因为 Kiro-Go 上游账户池为
  `0`，不得在无可用上游时恢复调度。
- 健康、端口、Docker DNS、数据库迁移、价格文件、定向回归和容器启动时间
  验收均通过。
- 使用毕业机已有 Kiro API Key 发起的同步和流式请求均到达 Kiro-Go，并在
  上游账户选择阶段返回 `503 No available accounts`。真实模型 usage、TTL
  分布和跨倍率扣费因此仍被账户池为空阻塞。

## 备份与 staging

完整回滚集：

```text
/home/ubuntu/backups/kiro-native-high-cache-20260714-000824
/home/ubuntu/backups/kiro-native-high-cache-latest
```

结果：

```text
大小：1.3G
文件：23
校验项：21
checksum_failures=0
敏感文件权限：0600
```

源码 staging：

```text
/home/ubuntu/staging/kiro-native-high-cache-20260714
```

归档：

```text
kiro-go-701033b.tar.gz
SHA256=21a70b4928a06d8f6a7ded10a7f8d35feb70268af0abda714a995f866c5cb361

sub2api-1c0f22a8.tar.gz
SHA256=4f4406e0edf0d193938fda8012be891e631ca8934c168142ac7d95eea153fe27

最终 Sub2API 候选源码：
/home/ubuntu/staging/kiro-native-high-cache-20260714/sub2api-build-06a25c99-20260714-023000z/source.tar.gz
SHA256=8ac72157e7649abef3676f52b4bf0d561cea6a743dbc4c8bf8df960d918b74c9
```

## Kiro-Go 部署

候选镜像：

```text
local/kiro-go:native-high-cache-701033b-20260714
sha256:594031079e4b01998ad24a89b1d4645e670f9eec67fac7eeb4fa3ec5bb64f0e2
org.opencontainers.image.revision=701033bea1b662f101ab309b297df09b0457d5d2
org.opencontainers.image.created=2026-07-14T00:54:28Z
```

精确回滚镜像：

```text
local/kiro-go:rollback-1babd73-20260714
sha256:80cf711d465ff952cebed47082e8de06e289d83cdf3a112359dbe9a3d296dfac
```

就地配置回滚：

```text
/home/ubuntu/kiro-go-pr131/docker-compose.yml.before-native-high-cache-20260714-0058
/home/ubuntu/kiro-go-pr131/data/config.json.before-native-high-cache-20260714-0058
```

最终状态：

```text
started=2026-07-14T00:55:20.924090987Z
status=running
health=healthy
127.0.0.1:8321 -> 8321/tcp
127.0.0.1:3128 -> 3128/tcp
port=8321
accounts=0
apiKeys=1
```

网络验证：

```text
宿主机 http://127.0.0.1:8321/health -> 200
Sub2API 容器 http://kiro-go-pr131:8321/health -> 200
Docker DNS kiro-go-pr131 -> 172.18.0.13
公网 TCP 157.254.187.242:8321 -> blocked
```

`ss` 只显示 `127.0.0.1:8321`，Docker 端口绑定没有 `0.0.0.0` 或公网地址。

## Sub2API 部署

候选镜像：

```text
ghcr.io/gwenliu1025/sub2api:0.1.152
本机镜像 ID=sha256:015e79c653d710174b5e6bc0d8618a982fc28c8af0698d196d22ab4869148720
version=0.1.152
revision=06a25c999c3d0cde157d30cde694fba2960e6d10
created=2026-07-14T02:32:04Z
image_created=2026-07-14T02:32:55.751765136Z
```

该镜像是毕业机本地候选。远端 GHCR 同标签仍是此前正式发布资产：

```text
sha256:8f7f1cb6874da8a1aa28095d3b66b14e80aa89cab34b011605f4d001787e0a0c
```

因此在正式重新发布前，不得在毕业机执行会重新拉取远端
`ghcr.io/gwenliu1025/sub2api:0.1.152` 的 Prepare、Activate 或手工 pull。

精确回滚镜像：

```text
local/sub2api:rollback-0.1.152-ca7eaa4-20260714
sha256:8f7f1cb6874da8a1aa28095d3b66b14e80aa89cab34b011605f4d001787e0a0c

local/sub2api:rollback-0.1.152-1c0f22a-20260714-022741z
sha256:af52620f4dc4b293436d79e687520791198915f16e8b8826dcdb5a990f1cca9b
```

当前部署前镜像继续保留：

```text
ghcr.io/gwenliu1025/sub2api:0.1.149
sha256:7dca5936fc15e8bf26bd043193e7fadfa4e91c39d179debf313e3755ff7f2240
```

最终状态：

```text
started=2026-07-14T02:34:10.443306229Z
status=running
health=healthy
image_id=sha256:015e79c653d710174b5e6bc0d8618a982fc28c8af0698d196d22ab4869148720
```

健康验证：

```text
http://127.0.0.1:8080/health -> 200
http://127.0.0.1:8080/healthz -> 200
http://127.0.0.1/health -> 200
updater socket /v1/health -> 200
updater socket /v1/status -> 200
```

更新代理的状态缓存仍显示旧 `0.1.149`。这是手工候选 Compose 切换的已知
差异，不代表实际容器镜像；本次没有伪造代理状态，也没有调用会拉取远端旧
`0.1.152` 的 Prepare/Activate。

## account 1910

事务提交时间：

```text
2026-07-14 09:12:05.868603+08
```

同一事务完成：

- 校验旧 Base URL 为 `http://kiro-go-pr131:8080`；
- 校验账户为 `active` 且 `schedulable=false`；
- 仅局部更新 `credentials.base_url`，不覆盖 API Key 或模型映射；
- 删除五个 `equivalent_cache_billing_*` 顶层 Extra；
- 写入 `cache_ttl_override_enabled=false`；
- 插入 `scheduler_outbox` 的 `account_changed` 事件。

最终安全摘要：

```text
id=1910
status=active
schedulable=false
base_url=http://kiro-go-pr131:8321
has_api_key=true
extra={"cache_ttl_override_enabled": false}
outbox_id=372598
```

## 定价文件回退验收

毕业机自动下载的上游价格文件包含不符合标准倍率的旧 Claude 3 缓存价格。
候选版本按设计拒绝该文件，并回退到镜像内置价格文件。

首次切换在看到 ERROR 日志后于
`2026-07-14T01:14:46Z` 定向回滚到 `0.1.149`。对镜像、staging、运行数据
和配置路径完成核对后，确认 ERROR 表示远端数据被安全拒绝，不是 fallback
失败。随后再次部署并设置以下硬验收条件：

- 日志必须出现 `Using fallback file`；
- 日志必须出现 `Service initialized with 196 models`；
- 不得出现最终 `Pricing service initialization failed`；
- 运行价格文件 SHA256 必须等于镜像内置 fallback 文件。

最终结果：

```text
fallback_sha256=9b7f21e48fedeb601d98fd77fe3bd36fb41d4f960f20fc5e55c353d5ad01e28b
models=196
checked_claude_cache_models=23
bad_ratio_count=0
fatal_startup_errors=0
```

关键价格：

| 模型 | 输入 | 输出 | 缓存读取 | 5 分钟创建 | 1 小时创建 |
| --- | ---: | ---: | ---: | ---: | ---: |
| `claude-haiku-4-5` | `1e-6` | `5e-6` | `1e-7` | `1.25e-6` | `2e-6` |
| `claude-opus-4-6` | `5e-6` | `2.5e-5` | `5e-7` | `6.25e-6` | `1e-5` |
| `claude-opus-4-8` | `5e-6` | `2.5e-5` | `5e-7` | `6.25e-6` | `1e-5` |

## 数据库与隔离回归

migration 174 的八个历史列全部存在：

```text
raw_input_tokens
raw_output_tokens
raw_cache_read_tokens
raw_cache_creation_tokens
raw_cache_creation_5m_tokens
raw_cache_creation_1h_tokens
usage_allocation_version
usage_allocation_kind
```

毕业机隔离命令：

```bash
go test ./internal/service \
  -run '(Pricing|EquivalentCacheCleanup|ClaudeFlat渠道|ClaudeInterval渠道)' \
  -count=1
```

结果：

```text
ok github.com/Wei-Shaw/sub2api/internal/service
```

此前 staging 还已通过：

```text
go test ./...
go test -tags=unit ./...
go build ./cmd/server
go build -ldflags='-s -w -X main.Version=0.1.152' -trimpath -o bin/server ./cmd/server
```

Sub2API 最终 race 工作副本：

```text
/home/ubuntu/staging/kiro-native-high-cache-20260714/sub2api-race-fix-20260714-015824z
```

其 13 个变更文件与提交 `06a25c99` 的最终构建 staging 逐文件一致。Linux
毕业机验证结果：

```text
定向 service race：exit=0，DATA RACE=0，FAIL=0
普通全量 race：exit=0，DATA RACE=0，FAIL=0
unit 标签全量 race：exit=0，DATA RACE=0，FAIL=0
```

5 个已退出的 race 诊断容器已删除，日志和状态摘要保留在最终 Sub2API 构建
staging；运行中的服务未受影响。

Kiro-Go staging 已通过：

```text
go test -race ./...
go build ./...
```

## 容器启动时间

以完整备份中的
`metadata/container-start-times.txt` 和最终容器 inspect 对比：

```text
changed_count=2
changed=kiro-go-pr131,sub2api
added=none
missing=none
unexpected_changed=none
```

PostgreSQL、Redis、Caddy、CPA、QQ/NapCat、TransitHub 和其他无关服务均未
被本次部署重建。

## 真实链路阻塞边界

未带密钥的同步和流式请求均返回 `401`，证明 Kiro-Go 认证生效。

使用毕业机已有、未输出且未落盘的 Kiro API Key 后：

```text
同步请求 -> 503 No available accounts
流式请求 -> 503 No available accounts
```

同时：

```text
Kiro-Go /v1/models -> 200
Kiro-Go accounts=0
Kiro-Go apiKeys=1
```

因此当前不能验收：

- 首轮真实缓存创建 usage；
- 后续轮真实缓存读取与新增创建 usage；
- 真实 5m/1h TTL 分布；
- Kiro 上游账户换源；
- 两个真实分组倍率的最终扣费；
- 下游 Sub2API/NewAPI 的真实 usage 展示。

解除阻塞需要向毕业机 Kiro-Go 导入一个明确授权的可用上游账户。不得从生产机
或备用机复制，也不得生成临时凭据仅用于探测。

## 生产门禁

毕业机可执行范围已完成。生产机变更前仍必须：

1. 获得用户对生产机备份、Kiro-Go、account `1910` 和 Sub2API 定向切换的
   明确授权；
2. 重新归档并发布与提交
   `06a25c999c3d0cde157d30cde694fba2960e6d10`
   一致的 `0.1.152` Release、二进制、checksums 和 GHCR 镜像；
3. 在生产变更前再次完整备份；
4. 只重建 Kiro-Go 和 Sub2API；
5. 比较全部无关容器启动时间；
6. 在有可用 Kiro 上游账户后执行真实同步、流式、TTL、倍率和下游展示验收。
