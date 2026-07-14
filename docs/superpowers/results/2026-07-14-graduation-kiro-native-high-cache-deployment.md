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

## 精确发布清单

### Kiro-Go

```text
代码提交=701033bea1b662f101ab309b297df09b0457d5d2
毕业机候选镜像=local/kiro-go:native-high-cache-701033b-20260714
毕业机候选镜像 ID=sha256:594031079e4b01998ad24a89b1d4645e670f9eec67fac7eeb4fa3ec5bb64f0e2
OCI revision=701033bea1b662f101ab309b297df09b0457d5d2
OCI created=2026-07-14T00:54:28Z
源码归档=kiro-go-701033b.tar.gz
源码归档 SHA256=21a70b4928a06d8f6a7ded10a7f8d35feb70268af0abda714a995f866c5cb361
```

当前 Kiro-Go 资产只是毕业机本地候选，还没有形成供生产机按精确标签拉取的
正式远端资产。生产发布时必须沿用 Kiro-Go 自身版本体系，并保证最终镜像的
OCI revision 仍指向上述提交。

### Sub2API

```text
代码提交=06a25c999c3d0cde157d30cde694fba2960e6d10
应用版本=0.1.152
毕业机候选镜像=ghcr.io/gwenliu1025/sub2api:0.1.152
毕业机候选镜像 ID=sha256:015e79c653d710174b5e6bc0d8618a982fc28c8af0698d196d22ab4869148720
OCI revision=06a25c999c3d0cde157d30cde694fba2960e6d10
OCI created=2026-07-14T02:32:04Z
镜像创建时间=2026-07-14T02:32:55.751765136Z
源码归档=/home/ubuntu/staging/kiro-native-high-cache-20260714/sub2api-build-06a25c99-20260714-023000z/source.tar.gz
源码归档 SHA256=8ac72157e7649abef3676f52b4bf0d561cea6a743dbc4c8bf8df960d918b74c9
```

当前远端 `v0.1.152`、GitHub Release 和 GHCR 同标签仍属于提交
`ca7eaa4da82c492f9852fd3c9b480b1932ccc5c2`，GHCR 摘要为
`sha256:8f7f1cb6874da8a1aa28095d3b66b14e80aa89cab34b011605f4d001787e0a0c`。
生产切换前必须重新归档并发布，使 Git 标签、Release 二进制、
`checksums.txt`、GHCR 镜像和源码提交全部一致指向 `06a25c99`。重新发布前
不得在毕业机对 `0.1.152` 执行 pull、Prepare 或 Activate。

### 定向变更范围

本轮毕业机实际变更范围为：

- 只重建 Compose 服务 `kiro-go-pr131` 和 `sub2api`；
- Kiro-Go 恢复为容器内外统一 `8321`，宿主机只绑定
  `127.0.0.1:8321`；
- account `1910` 只修改 `credentials.base_url` 和清退相关 Extra，并写入
  `account_changed` outbox 事件，未覆盖 API Key 或模型映射；
- 不重建 PostgreSQL、Redis、Caddy、CPA、QQ/NapCat、TransitHub 或其他
  无关服务。

生产机应使用同一变更边界，但任何操作仍需单独明确授权。

## 可执行回滚命令

以下命令仅作为毕业机精确回滚手册保存，本轮没有执行。执行前必须再次确认
目标主机为 `157.254.187.242`，并先保存当时的容器启动时间。所有命令都以
`root` 身份在毕业机执行。

### Kiro-Go 镜像、Compose 和端口回滚

旧 Compose 的真实解析合同为：

```text
service=kiro-go-pr131
image=kiro-go-pr131-kiro-go-pr131
build.context=/home/ubuntu/kiro-go-pr131/src
build.dockerfile=Dockerfile
host port=8321
container port=8080
```

旧 Compose 没有显式 `image` 字段，因此不能凭空填写镜像。先把已保存的旧
镜像标记为 Compose 解析出的默认镜像名，再恢复两个 `0600` 备份文件，并用
`--no-build --pull never` 定向重建：

```bash
set -euo pipefail

test "$(docker image inspect \
  local/kiro-go:rollback-1babd73-20260714 \
  --format '{{.Id}}')" = \
  "sha256:80cf711d465ff952cebed47082e8de06e289d83cdf3a112359dbe9a3d296dfac"

docker image tag \
  local/kiro-go:rollback-1babd73-20260714 \
  kiro-go-pr131-kiro-go-pr131:latest

install -m 0600 \
  /home/ubuntu/kiro-go-pr131/docker-compose.yml.before-native-high-cache-20260714-0058 \
  /home/ubuntu/kiro-go-pr131/docker-compose.yml
install -m 0600 \
  /home/ubuntu/kiro-go-pr131/data/config.json.before-native-high-cache-20260714-0058 \
  /home/ubuntu/kiro-go-pr131/data/config.json

docker compose \
  --project-directory /home/ubuntu/kiro-go-pr131 \
  -f /home/ubuntu/kiro-go-pr131/docker-compose.yml \
  up -d --pull never --no-build --no-deps --force-recreate kiro-go-pr131
```

该旧合同把宿主机 `8321` 映射到容器 `8080`，且旧 Compose 没有限定
`127.0.0.1`。回滚后必须复查防火墙和公网暴露，不得把旧端口合同误认为当前
安全拓扑。

### account 1910 Base URL 回滚

Kiro-Go 回滚到容器端口 `8080` 后，必须以预期当前值为条件，把 account
`1910` 的 Base URL 一并恢复；该事务不读取或改写 API Key、模型映射和其他
凭据：

```bash
docker exec -i sub2api-postgres sh -lc \
  'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
BEGIN;
DO $rollback$
DECLARE
  affected integer;
BEGIN
  UPDATE accounts
  SET credentials = jsonb_set(
        COALESCE(credentials, '{}'::jsonb),
        '{base_url}',
        to_jsonb('http://kiro-go-pr131:8080'::text),
        true
      ),
      updated_at = NOW()
  WHERE id = 1910
    AND deleted_at IS NULL
    AND status = 'active'
    AND schedulable = FALSE
    AND credentials->>'base_url' = 'http://kiro-go-pr131:8321';

  GET DIAGNOSTICS affected = ROW_COUNT;
  IF affected <> 1 THEN
    RAISE EXCEPTION 'account 1910 Base URL 预期值不匹配，已回滚事务';
  END IF;

  INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
  VALUES ('account_changed', 1910, NULL, NULL);
END
$rollback$;
COMMIT;
SQL
```

### Sub2API 镜像回滚

回到最后一个正式 `0.1.152` 资产时，使用提交 `ca7eaa4` 的精确本地标签。
重新标记同版本 Compose 标签后，必须使用 `--pull never`，避免从远端获得与
预期不同的资产：

```bash
set -euo pipefail

test "$(docker image inspect \
  local/sub2api:rollback-0.1.152-ca7eaa4-20260714 \
  --format '{{.Id}}')" = \
  "sha256:8f7f1cb6874da8a1aa28095d3b66b14e80aa89cab34b011605f4d001787e0a0c"

docker image tag \
  local/sub2api:rollback-0.1.152-ca7eaa4-20260714 \
  ghcr.io/gwenliu1025/sub2api:0.1.152

docker compose \
  --project-directory /home/ubuntu/sub2api \
  -f /home/ubuntu/sub2api/docker-compose.yml \
  --env-file /home/ubuntu/sub2api/.env \
  up -d --pull never --no-deps --force-recreate sub2api
```

如需回到本轮之前的上一毕业机候选，唯一替换项为：

```text
回滚标签=local/sub2api:rollback-0.1.152-1c0f22a-20260714-022741z
预期镜像 ID=sha256:af52620f4dc4b293436d79e687520791198915f16e8b8826dcdb5a990f1cca9b
```

不得同时使用两个回滚来源，也不得在回滚过程中执行 pull、Prepare 或
Activate。

### Equivalent Cache 旧 Extra 回滚

只有在 Sub2API 已回滚到仍需要旧 Equivalent Cache Extra 的旧运行时时，才
恢复 account `1910` 的旧 Extra。以下命令从完整备份中只提取 `extra`，不会
提取、输出或落盘 `credentials.api_key`：

```bash
set -euo pipefail

{
  cat <<'SQL'
BEGIN;
CREATE TEMP TABLE rollback_account_extra (payload jsonb);
\copy rollback_account_extra(payload) FROM STDIN
SQL
  jq -c '{extra:(.extra // {})}' \
    /home/ubuntu/backups/kiro-native-high-cache-20260714-000824/database/account-1910.json
  cat <<'SQL'
\.
DO $rollback$
DECLARE
  rollback_extra jsonb;
  affected integer;
BEGIN
  SELECT payload->'extra'
  INTO STRICT rollback_extra
  FROM rollback_account_extra;

  UPDATE accounts
  SET extra = COALESCE(rollback_extra, '{}'::jsonb),
      updated_at = NOW()
  WHERE id = 1910
    AND deleted_at IS NULL
    AND status = 'active'
    AND schedulable = FALSE
    AND COALESCE(extra, '{}'::jsonb) =
      '{"cache_ttl_override_enabled": false}'::jsonb;

  GET DIAGNOSTICS affected = ROW_COUNT;
  IF affected <> 1 THEN
    RAISE EXCEPTION 'account 1910 Extra 回滚目标不唯一，已回滚事务';
  END IF;

  INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
  VALUES ('account_changed', 1910, NULL, NULL);
END
$rollback$;
COMMIT;
SQL
} | docker exec -i sub2api-postgres sh -lc \
  'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
```

### 回滚后核验

```bash
docker inspect kiro-go-pr131 --format \
  '{{.Config.Image}}|{{.State.Status}}|{{.State.Health.Status}}|{{.State.StartedAt}}'
docker inspect sub2api --format \
  '{{.Config.Image}}|{{.Image}}|{{.State.Status}}|{{.State.Health.Status}}|{{.State.StartedAt}}'

curl -fsS http://127.0.0.1:8321/health
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1/health

docker exec sub2api getent hosts kiro-go-pr131
docker exec sub2api sh -lc \
  'wget -q -O - http://kiro-go-pr131:8080/health'
```

还必须把回滚后的全部容器启动时间与回滚前记录比较，确认只有实际执行回滚的
目标服务发生变化。

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

## 毕业机账户来源盘点

2026-07-14 对毕业机自身的 Kiro-Go 配置和本轮备份执行了只读盘点。只输出
文件路径、权限和数组数量，没有输出账户、Token、API Key 或完整邮箱：

```text
/home/ubuntu/kiro-go-pr131/data/config.json
mode=0600 accounts=0 enabled=0 apiKeys=1

/home/ubuntu/kiro-go-pr131/data/config.json.before-native-high-cache-20260714-0058
mode=0600 accounts=0 enabled=0 apiKeys=1

/home/ubuntu/backups/kiro-native-high-cache-20260714-000824/kiro/config.json
mode=0600 accounts=0 enabled=0 apiKeys=1
```

因此毕业机当前配置、部署前配置和完整回滚备份中都不存在可复用的 Kiro 上游
账户。真实链路阻塞不是遗漏了某个毕业机本地备份，而是确实缺少外部提供且
明确授权的账户来源。

## 授权后账户导入路径

Kiro-Go 已核验的首选入口为：

```text
POST http://127.0.0.1:8321/admin/api/auth/credentials
```

该接口要求 `refreshToken`，并在持久化前先执行一次真实 Token 刷新。刷新
失败时返回错误且不写入账户；刷新成功后以 `0600` 保存 `config.json` 并立即
执行 `pool.Reload()`，不需要重启 Kiro-Go。

不得直接编辑 `config.json`，也不得使用只包含一个临时 `accessToken` 的
数据绕过刷新校验。获得用户明确授权的凭据文件后，文件必须是如下字段形状的
JSON 对象：

```text
refreshToken     必填
clientId         IAM Identity Center / Builder ID 账户按来源提供
clientSecret     IAM Identity Center / Builder ID 账户按来源提供
authMethod       idc 或 social；省略时由接口按字段推断
provider         可选
region           可选，默认 us-east-1
```

以下脚本只接受权限不宽于 `0600`、大小不超过 `64 KiB` 的普通文件。管理密码
从运行中容器环境或现有 `config.json` 读取到内存；请求体和密码不会进入命令行
参数，输出只包含 HTTP 状态、成功标志、账户 ID、池账户数和可用账户数。
**该脚本等待用户提供并明确授权凭据文件后才能执行，本轮没有执行。**

```bash
set -euo pipefail
: "${KIRO_CREDENTIAL_FILE:?必须指定已授权的 Kiro 凭据 JSON 文件}"

python3 - "$KIRO_CREDENTIAL_FILE" <<'PY'
import json
import os
import stat
import subprocess
import sys
import urllib.error
import urllib.request

credential_path = os.path.realpath(sys.argv[1])
file_stat = os.stat(credential_path)
if not stat.S_ISREG(file_stat.st_mode):
    raise SystemExit("凭据路径不是普通文件")
if stat.S_IMODE(file_stat.st_mode) & 0o077:
    raise SystemExit("凭据文件权限必须不宽于 0600")
if file_stat.st_size > 64 * 1024:
    raise SystemExit("凭据文件超过 64 KiB")

with open(credential_path, encoding="utf-8") as credential_file:
    payload = json.load(credential_file)
if not isinstance(payload, dict) or not payload.get("refreshToken"):
    raise SystemExit("凭据 JSON 缺少 refreshToken")

container = json.loads(
    subprocess.check_output(
        ["docker", "inspect", "kiro-go-pr131"],
        text=True,
    )
)[0]
admin_password = ""
for item in container.get("Config", {}).get("Env") or []:
    if item.startswith("ADMIN_PASSWORD="):
        admin_password = item.split("=", 1)[1]
        break
if not admin_password:
    with open(
        "/home/ubuntu/kiro-go-pr131/data/config.json",
        encoding="utf-8",
    ) as config_file:
        admin_password = json.load(config_file).get("password", "")
if not admin_password:
    raise SystemExit("未找到 Kiro-Go 管理密码")

base_url = "http://127.0.0.1:8321"
headers = {
    "Content-Type": "application/json",
    "X-Admin-Password": admin_password,
}
request = urllib.request.Request(
    base_url + "/admin/api/auth/credentials",
    data=json.dumps(payload, separators=(",", ":")).encode("utf-8"),
    headers=headers,
    method="POST",
)
try:
    with urllib.request.urlopen(request, timeout=45) as response:
        import_status = response.status
        result = json.load(response)
except urllib.error.HTTPError as error:
    print(f"import_http_status={error.code}|success=false")
    raise SystemExit(1)

account = result.get("account") or {}
if not result.get("success") or not account.get("id"):
    print(f"import_http_status={import_status}|success=false")
    raise SystemExit(1)

status_request = urllib.request.Request(
    base_url + "/admin/api/status",
    headers={"X-Admin-Password": admin_password},
)
with urllib.request.urlopen(status_request, timeout=15) as response:
    status = json.load(response)

print(
    "import_http_status=%d|success=true|account_id=%s|accounts=%s|available=%s"
    % (
        import_status,
        account["id"],
        status.get("accounts"),
        status.get("available"),
    )
)
PY
```

导入成功但 `available=0` 时，仍不能继续真实链路验收，需要先处理账户禁用、
冷却或额度状态。只有 `accounts>=1` 且 `available>=1` 后，才执行账户
`/test`、Sub2API 同步与流式请求、TTL、换源、倍率和下游展示验收。管理接口的
`/accounts/{id}/test` 会产生真实上游模型流量，只能在账户来源和真实探测均已
明确授权后调用，且不能替代完整的 Sub2API 跨服务验收。

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
