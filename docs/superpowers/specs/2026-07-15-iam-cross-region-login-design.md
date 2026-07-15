# IAM Identity Center 跨区登录与凭据兼容设计

## 背景

KiroGo 当前把一个 `Region` 字段同时用于 IAM Identity Center OIDC 认证、token 刷新、Kiro Profile 查找和数据面请求。对于“OIDC client 在 `us-east-1` 注册，但 Kiro Profile 位于 `eu-central-1` 等其他区域”的账号，这种建模会产生两个直接故障：

1. 用户填写 Profile 所在区域后，OIDC client 注册和授权请求被错误发送到该区域，登录可能没有响应或直接失败。
2. 用户使用 `us-east-1` 完成登录后，账号缺少明确的 Profile 区域和 Profile ARN，订阅刷新可能失败并被前端错误显示为 Free。

现有凭据导入导出还会丢失 `profileArn`、错误推断 `Provider=BuilderId`，并把 Power 导出为 `Pro_Plus`，进一步放大上述问题。

## 目标

- 将 IAM Identity Center 的认证区域与 Kiro Profile 数据区域彻底分离。
- 保持 OIDC client 注册、授权码交换和 refresh token 刷新始终使用同一个认证区域。
- 允许用户填写卖家提供的 Profile 区域提示，例如 `eu-central-1`。
- 登录成功后立即跨区解析并持久化 Profile ARN。
- Kiro 数据面请求始终根据 Profile ARN 自动选择区域。
- 修复凭据 JSON 导入导出中的 Profile ARN、Provider 和订阅类型兼容问题。
- 区分“订阅未知/读取失败”和真实 Free，显示可操作的错误。

## 非目标

- 本阶段不修改 Microsoft 365 / Entra ID 的“企业 SSO”登录流程。
- 本阶段不实现“图图大王”提供的第八种 API 登录方式。
- 不保存或自动填写卖家提供的用户名和密码。
- 不通过遍历所有 AWS Region 注册大量 OIDC client。
- 不改变 Kiro 请求根据 Profile ARN 路由数据面的既有机制。

## 术语与字段语义

### `authRegion`

IAM Identity Center OIDC 认证区域。用于：

- `RegisterClient`
- authorization code 授权
- authorization code 换取 token
- refresh token 刷新

前端默认值为 `us-east-1`，放在“高级设置”中。账号持久化时继续写入现有 `Account.Region`，保持现有 token 刷新逻辑兼容。

### callback

OAuth callback 保持为本地回环地址，例如：

```text
http://127.0.0.1/oauth/callback
```

callback 不包含 AWS Region，也不允许用户选择“回调区域”。它只承载授权响应中的 `code`、`state` 或错误参数。

### `profileRegionHint`

卖家提供的 Kiro Profile/订阅区域提示，例如 `eu-central-1`。它只参与 Profile ARN 解析，不参与 OIDC 注册、换 token 或 refresh token 刷新。

在 `config.Account` 中新增可选字段：

```go
ProfileRegionHint string `json:"profileRegionHint,omitempty"`
```

### `profileArn`

CodeWhisperer/Kiro Profile 的权威标识。成功解析后，Profile ARN 中的 Region 是后续 Kiro/Q 数据面路由的唯一权威来源。

优先级如下：

```text
已保存的 profileArn
> 实时 token 响应返回的 profileArn
> 用户导入的合法 profileArn
> 跨区 ListAvailableProfiles 解析结果
> profileRegionHint
> authRegion
```

其中 `profileRegionHint` 和 `authRegion` 只用于尚未获得 ARN 时生成探测候选区域，不能覆盖已确认的 ARN。

## 前端设计

IAM Identity Center 登录弹窗调整为：

```text
Start URL
https://d-xxxx.awsapps.com/start

Profile Region
eu-central-1

高级设置
认证 Region
us-east-1

[返回] [开始登录]
```

### 字段规则

- `Start URL` 必填，接受 AWS Access Portal URL 或 IAM Identity Center Issuer URL。
- `Profile Region` 必填，默认不预设欧洲区域，要求用户填写卖家提供的值。
- `认证 Region` 默认 `us-east-1`，允许高级用户修改。
- 不增加“回调 Region”字段。
- 不解析或保存包含用户名和密码的整行卖家账号数据。

### 登录状态

登录完成后，弹窗或账号列表依次显示：

1. `正在完成 IAM 登录`
2. `正在查找 Kiro Profile`
3. `已找到 Profile：eu-central-1`，随后刷新订阅
4. 成功时显示 AWS 实时返回的 Power、Pro 或 Free
5. 失败时显示 `Profile 检测失败` 或 `订阅读取失败`，不得回退显示 Free

`autoRefreshNewAccount` 必须检查 HTTP 状态和响应体。非 2xx 响应需要显示后端错误，并保留“未知”状态供用户重试。

## API 设计

### 开始登录

`POST /auth/iam-sso/start`

新请求格式：

```json
{
  "startUrl": "https://d-xxxx.awsapps.com/start",
  "authRegion": "us-east-1",
  "profileRegion": "eu-central-1"
}
```

兼容旧客户端：

- 如果没有 `authRegion`，读取旧字段 `region`。
- 如果两者都为空，使用 `us-east-1`。
- 旧字段 `region` 不再代表 Profile 区域。

后端会话保存：

```go
type IamSsoSession struct {
    // 现有字段省略
    AuthRegion       string
    ProfileRegionHint string
    StartURL         string
}
```

### 完成登录

`POST /auth/iam-sso/complete` 继续接收 `sessionId` 和 callback URL。授权码必须在会话的 `AuthRegion` 对应 OIDC endpoint 交换，不能使用 `ProfileRegionHint`。

登录成功后创建账号时必须写入：

```text
AuthMethod=idc
Provider=Enterprise
Region=<authRegion>
StartUrl=<startUrl>
ProfileRegionHint=<profileRegion>
```

如果 token 响应已经返回合法 `profileArn`，直接保存并跳过跨区探测。

## Profile ARN 解析

### 候选区域顺序

对于缺少 Profile ARN 的 IAM IdC 账号，候选区域按以下顺序去重：

```text
authRegion
profileRegionHint
KIRO_PROFILE_REGIONS 配置值
内置安全回退区域
```

示例：

```text
authRegion=us-east-1
profileRegionHint=eu-central-1

候选顺序：us-east-1 -> eu-central-1
```

所有 `AuthMethod=idc` 且缺少 ARN 的账号都允许使用明确的 `profileRegionHint`，不再限定为 `idc + us-east-1` 才能跨区探测。

### 登录后立即解析

账号凭据持久化并重新加载账号池后，后端立即调用 Profile ARN 解析，而不是等待前端第二次刷新才触发。

- 找到 ARN：保存 ARN，再调用实时订阅查询。
- 未找到 ARN：保留有效账号和 token，返回结构化 warning，允许稍后重试。
- 查询错误：不得将账号订阅写成 Free。

登录成功和 Profile 解析成功是两个独立状态。Profile 查询暂时失败不能回滚已经成功的 IAM 登录。

### 数据面路由

拿到 ARN 后继续复用现有逻辑：

```text
arn:aws:codewhisperer:eu-central-1:...:profile/...
-> q.eu-central-1.amazonaws.com
```

refresh token 仍然使用 `Account.Region=us-east-1`，不得被 Profile 区域覆盖。

## JSON 导入设计

前端解析 Kiro Account Manager 格式时新增：

```text
profileArn = credentials.profileArn || account.profileArn
profileRegionHint = credentials.profileRegionHint || account.profileRegionHint
```

导入请求增加：

```json
{
  "profileArn": "arn:aws:codewhisperer:...",
  "profileRegionHint": "eu-central-1"
}
```

后端规则：

1. refresh token 必须继续使用导入凭据自带的认证 Region。
2. refresh 响应返回合法 ARN 时优先使用实时 ARN。
3. refresh 响应不含 ARN 时保留导入的合法 ARN。
4. ARN 非法时拒绝该 ARN，但不回显敏感凭据。
5. 缺失 Provider 的 `idc` 凭据默认标记为 `Enterprise`，不能默认标记为 `BuilderId`。
6. 只有明确的 Builder ID 登录或可信导出字段才能设置 `Provider=BuilderId`。

JSON 内的订阅标题、额度和使用量不作为实时权益证明，只用于导入后的临时展示或完全忽略。最终订阅必须通过 AWS 实时接口确认。

## JSON 导出设计

兼容导出必须包含：

- credentials 内的 `profileArn`
- 顶层 `profileArn`，兼容读取顶层 ARN 的工具
- `profileRegionHint`
- `Provider`
- `StartUrl`
- 正确的 `AuthMethod`

身份映射规则：

```text
Provider=Enterprise 或 AuthMethod=idc -> idp=Enterprise
Provider=BuilderId -> idp=BuilderId
AuthMethod=social -> 使用实际 Provider
```

订阅类型必须保持原始含义：

```text
KIRO POWER / POWER -> POWER
PRO_PLUS -> Pro_Plus
PRO -> Pro
FREE -> Free
未知 -> Unknown
```

不得再把 Power 导出为 `Pro_Plus`。

## 订阅状态设计

前端新增明确状态：

```text
UNKNOWN：尚未查询或查询失败
FREE：AWS 实时明确返回 Free
POWER/PRO/PRO_PLUS：AWS 实时返回对应权益
```

空字符串、缺失字段和 Profile 解析错误统一显示为“待检测”或“读取失败”，不能显示为 Free。

后端订阅刷新响应需要区分：

- token 刷新失败
- Profile ARN 未找到
- Profile Region 不匹配
- AWS 订阅接口失败
- AWS 明确返回 Free

## 输入校验与安全

- `Start URL` 必须使用 HTTPS，并限制为受支持的 AWS Access Portal/Issuer 域名。
- `authRegion` 和 `profileRegionHint` 必须符合 AWS Region 格式，拒绝路径、端口和 URL 片段。
- 导入 ARN 必须解析为 `codewhisperer` service，并包含合法 Region 和 profile 资源。
- callback 继续校验 PKCE 和 `state`。
- callback URL 只读取 `code`、`state` 和错误字段，不发起对该 URL 的网络请求。
- 登录会话需要定时销毁，并提供取消接口，及时清除内存中的 client secret 和 PKCE 状态。
- 日志和错误响应不得包含 access token、refresh token、client secret、密码或完整 callback URL。

## 兼容与迁移

- 现有 `Account.Region` 继续表示 OIDC/token Region，不执行数据迁移。
- 新增 `ProfileRegionHint` 为可选字段，旧配置可直接加载。
- 已有 Profile ARN 的账号不受候选区域改动影响。
- 旧 `/auth/iam-sso/start` 请求中的 `region` 继续可用，但只映射为 `authRegion`。
- 已被错误保存为 `Provider=BuilderId` 的 IdC 账号，可在成功解析企业 Profile 后自动修正为 `Enterprise`；自动修正必须有测试覆盖。

## 测试设计

### IAM 登录

- `authRegion=us-east-1` 时，RegisterClient、authorize 和 token exchange 全部使用 `us-east-1`。
- `profileRegionHint=eu-central-1` 不得改变 OIDC endpoint。
- callback state 不匹配时拒绝完成登录。
- 登录会话超时和取消后清除敏感状态。

### Profile 解析

- IAM IdC 账号按照 `us-east-1 -> eu-central-1` 顺序探测。
- 第一区域无 Profile、第二区域有 Profile 时保存 EU ARN。
- ARN 保存后数据请求切换到 `q.eu-central-1.amazonaws.com`。
- token refresh 仍然请求 `oidc.us-east-1.amazonaws.com/token`。
- Profile 解析失败不会把订阅写成 Free。

### JSON 兼容

- 导入时保留 credentials 层 ARN。
- credentials 层缺失时读取顶层 ARN。
- 实时 refresh ARN 优先于导入 ARN。
- IdC + Provider 缺失时标记为 Enterprise。
- 导出包含两层 ARN、Profile Region、Start URL 和 Provider。
- Power 导出为 Power，不再导出成 `Pro_Plus`。

### 前端

- IAM 弹窗显示 Profile Region 和折叠的认证 Region。
- 不显示“回调 Region”。
- 非 2xx 自动刷新显示错误。
- 空订阅显示“待检测”，AWS 明确返回 Free 才显示 Free。

## 验收标准

使用以下示例输入：

```text
Start URL=https://d-example.awsapps.com/start
authRegion=us-east-1
profileRegionHint=eu-central-1
```

系统必须满足：

1. OIDC client 注册、授权码交换和后续 refresh token 均使用 `us-east-1`。
2. 登录成功后依次查询 `us-east-1` 和 `eu-central-1` 的可用 Profile。
3. 找到 EU Profile 后持久化 EU Profile ARN。
4. 使用量、订阅和模型请求转向 `q.eu-central-1.amazonaws.com`。
5. 前端显示 AWS 实时订阅，不因暂时查询失败显示 Free。
6. 导出再导入后仍保留 Enterprise、认证区域、Profile 区域和 Profile ARN。
7. Microsoft 365 SSO 和其他七种既有登录方式行为不变。
8. 第八种 API 登录保持独立，后续通过单独设计和实施计划接入。
