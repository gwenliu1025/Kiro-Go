# IAM Identity Center 跨区登录实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 IAM Identity Center 跨区登录、Profile ARN 解析、凭据 JSON 兼容和前端错误订阅显示，使 OIDC/token 始终使用认证区域，而 Kiro 数据面根据 Profile ARN 使用独立区域。

**Architecture:** 现有 `Account.Region` 保留为 OIDC/token 认证区域，新增 `ProfileRegionHint` 作为尚未获得 ARN 时的 Profile 探测提示。IAM 登录会话分别保存 `AuthRegion` 与 `ProfileRegionHint`；登录成功后立即持久化 Enterprise 身份并解析 Profile ARN。JSON 导入导出复用相同字段语义，前端把空订阅显示为“待检测”而不是 Free。

**Tech Stack:** Go 1.21、标准库 `net/http`/`httptest`、原生 JavaScript、现有静态管理页与 Go 测试框架。

---

## 文件职责

- `config/config.go`：账号持久化字段，新增 `ProfileRegionHint`。
- `auth/iam_sso.go`：IAM OIDC 会话、认证区域、Profile 区域提示、超时和取消。
- `auth/iam_sso_test.go`：认证区域与 Profile 区域分离的单元测试。
- `proxy/handler.go`：IAM 登录 API、账号落库、登录后订阅刷新、JSON 导入导出。
- `proxy/iam_sso_handler_test.go`：IAM 请求兼容、账号身份字段和登录后状态测试。
- `proxy/kiro_api.go`：Profile ARN 校验、候选区域顺序和数据面路由。
- `proxy/kiro_region_test.go`、`proxy/kiro_api_test.go`：跨区 Profile 回归测试。
- `proxy/import_credentials_test.go`：导入 ARN、Provider 和认证区域测试。
- `proxy/export_accounts_test.go`：Enterprise/Power/Profile ARN 导出测试。
- `proxy/admin_frontend_contract_test.go`：无前端测试框架情况下的管理页字段契约测试。
- `web/app.js`：IAM 登录弹窗、请求字段、刷新错误和 Unknown 订阅状态。
- `web/styles.css`：Unknown 订阅徽标和高级设置的轻量样式。
- `web/locales/zh.json`、`web/locales/en.json`：新增字段和状态文案。

---

### Task 1：拆分 IAM 认证区域与 Profile 区域

**Files:**
- Modify: `config/config.go`
- Modify: `auth/iam_sso.go`
- Create: `auth/iam_sso_test.go`

- [ ] **Step 1：写入失败测试，证明 Profile 区域不能改变 OIDC endpoint**

在 `auth/iam_sso_test.go` 增加测试服务器，替换包内 `iamOIDCBaseURL`，检查传入区域始终为 `us-east-1`，并返回临时 client 凭据：

```go
func TestStartIamSsoLoginSeparatesAuthAndProfileRegions(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/client/register" {
            t.Fatalf("unexpected path %s", r.URL.Path)
        }
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(`{"clientId":"client-1","clientSecret":"secret-1"}`))
    }))
    defer srv.Close()

    previous := iamOIDCBaseURL
    iamOIDCBaseURL = func(region string) string {
        if region != "us-east-1" {
            t.Fatalf("OIDC region = %q, want us-east-1", region)
        }
        return srv.URL
    }
    defer func() { iamOIDCBaseURL = previous }()

    sessionID, authorizeURL, _, err := StartIamSsoLogin(
        "https://d-example.awsapps.com/start",
        "us-east-1",
        "eu-central-1",
    )
    if err != nil {
        t.Fatalf("StartIamSsoLogin: %v", err)
    }
    defer CancelIamSsoLogin(sessionID)

    sessionsMu.RLock()
    session := sessions[sessionID]
    sessionsMu.RUnlock()
    if session == nil {
        t.Fatal("IAM SSO session not found")
    }
    if session.AuthRegion != "us-east-1" || session.ProfileRegionHint != "eu-central-1" {
        t.Fatalf("unexpected session regions: %+v", session)
    }
    if !strings.Contains(authorizeURL, "client_id=client-1") {
        t.Fatalf("authorize URL missing client id: %s", authorizeURL)
    }
}
```

再增加 callback 交换测试，断言 `ProfileRegionHint=eu-central-1` 时 token exchange 仍调用认证区对应 endpoint。

- [ ] **Step 2：运行测试并确认因新 API/字段不存在而失败**

Run:

```powershell
go test ./auth -run 'TestStartIamSsoLoginSeparatesAuthAndProfileRegions|TestCompleteIamSsoLoginUsesAuthRegion' -count=1
```

Expected: FAIL，提示 `StartIamSsoLogin` 参数数量、`AuthRegion`、`ProfileRegionHint`、`CancelIamSsoLogin` 或测试钩子不存在。

- [ ] **Step 3：实现最小认证会话模型**

在 `config.Account` 增加：

```go
ProfileRegionHint string `json:"profileRegionHint,omitempty"` // Profile 区域提示，不参与 token 刷新
```

在 `auth/iam_sso.go` 中：

```go
var iamOIDCBaseURL = func(region string) string {
    return fmt.Sprintf("https://oidc.%s.amazonaws.com", region)
}

type IamSsoSession struct {
    ClientID         string
    ClientSecret     string
    CodeVerifier     string
    State            string
    AuthRegion       string
    ProfileRegionHint string
    StartURL         string
    RedirectURI      string
    ExpiresAt        time.Time
    timer            *time.Timer
}
```

将签名调整为：

```go
func StartIamSsoLogin(startURL, authRegion, profileRegionHint string) (sessionID, authorizeURL string, expiresIn int, err error)
```

所有 RegisterClient、authorize 和 token exchange 都通过 `iamOIDCBaseURL(session.AuthRegion)`。增加：

```go
func CancelIamSsoLogin(sessionID string)
```

会话创建后使用 `time.AfterFunc` 在十分钟到期时删除会话并清除 client secret；成功完成或取消时停止 timer。

- [ ] **Step 4：运行认证测试并确认通过**

Run:

```powershell
gofmt -w auth/iam_sso.go auth/iam_sso_test.go config/config.go
go test ./auth -count=1
go test ./config -count=1
```

Expected: PASS。

- [ ] **Step 5：提交认证会话变更**

```powershell
git add auth/iam_sso.go auth/iam_sso_test.go config/config.go
git commit -m "修复：拆分 IAM 认证区与 Profile 区域"
```

---

### Task 2：修复 IAM 登录 API 与 Enterprise 账号落库

**Files:**
- Modify: `proxy/handler.go`
- Create: `proxy/iam_sso_handler_test.go`
- Modify: `web/app.js`（仅在本任务添加取消 API 调用，不调整最终 UI）

- [ ] **Step 1：写入失败测试，锁定请求兼容与账号字段**

测试覆盖：

```go
func TestNormalizeIamSsoStartRequestUsesLegacyRegionAsAuthRegion(t *testing.T) {
    req := iamSsoStartRequest{Region: "us-east-1", ProfileRegion: "eu-central-1"}
    req.normalize()
    if req.AuthRegion != "us-east-1" || req.ProfileRegion != "eu-central-1" {
        t.Fatalf("unexpected normalized request: %+v", req)
    }
}

func TestBuildIamSsoAccountPersistsEnterpriseRoutingFields(t *testing.T) {
    account := buildIamSsoAccount(auth.IamSsoResult{
        AccessToken:       "access",
        RefreshToken:      "refresh",
        ClientID:          "client",
        ClientSecret:      "secret",
        AuthRegion:        "us-east-1",
        ProfileRegionHint: "eu-central-1",
        StartURL:          "https://d-example.awsapps.com/start",
        ExpiresIn:         3600,
    })
    if account.AuthMethod != "idc" || account.Provider != "Enterprise" {
        t.Fatalf("unexpected identity fields: %+v", account)
    }
    if account.Region != "us-east-1" || account.ProfileRegionHint != "eu-central-1" {
        t.Fatalf("unexpected routing fields: %+v", account)
    }
    if account.StartUrl != "https://d-example.awsapps.com/start" {
        t.Fatalf("start URL not persisted: %+v", account)
    }
}
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```powershell
go test ./proxy -run 'TestNormalizeIamSsoStartRequest|TestBuildIamSsoAccount' -count=1
```

Expected: FAIL，相关类型和 helper 尚不存在。

- [ ] **Step 3：实现 IAM API 请求和账号构造**

在 `proxy/handler.go` 增加内部请求类型：

```go
type iamSsoStartRequest struct {
    StartURL      string `json:"startUrl"`
    AuthRegion    string `json:"authRegion"`
    ProfileRegion string `json:"profileRegion"`
    Region        string `json:"region"`
}

func (req *iamSsoStartRequest) normalize() {
    if strings.TrimSpace(req.AuthRegion) == "" {
        req.AuthRegion = strings.TrimSpace(req.Region)
    }
    if req.AuthRegion == "" {
        req.AuthRegion = "us-east-1"
    }
    req.ProfileRegion = strings.TrimSpace(req.ProfileRegion)
}
```

`apiStartIamSso` 调用新的 `auth.StartIamSsoLogin(req.StartURL, req.AuthRegion, req.ProfileRegion)`。

`CompleteIamSsoLogin` 返回包含认证区、Profile 区提示和 Start URL 的 `auth.IamSsoResult`，`buildIamSsoAccount` 将账号保存为：

```go
AuthMethod:         "idc",
Provider:           "Enterprise",
Region:             result.AuthRegion,
StartUrl:           result.StartURL,
ProfileRegionHint:  result.ProfileRegionHint,
```

新增 `/auth/iam-sso/cancel`，前端关闭 IAM 登录弹窗时释放会话。

- [ ] **Step 4：登录成功后立即刷新 Profile 与订阅**

账号先正常持久化并 `h.pool.Reload()`，再调用：

```go
info, refreshErr := RefreshAccountInfo(&account)
if refreshErr == nil {
    _ = config.UpdateAccountInfo(account.ID, *info)
}
```

返回规则：

- IAM token 成功时始终 `success=true`。
- Profile/订阅失败时附带 `warning`，不回滚账号。
- 只有实时订阅成功才返回 `subscriptionType`。

- [ ] **Step 5：运行处理器与相关回归测试**

Run:

```powershell
gofmt -w proxy/handler.go proxy/iam_sso_handler_test.go
go test ./proxy -run 'TestNormalizeIamSsoStartRequest|TestBuildIamSsoAccount|TestApiCompleteIamSso' -count=1
```

Expected: PASS。

- [ ] **Step 6：提交 IAM API 变更**

```powershell
git add proxy/handler.go proxy/iam_sso_handler_test.go web/app.js
git commit -m "修复：保存 IAM 企业身份并立即刷新 Profile"
```

---

### Task 3：让 Profile 探测使用明确的区域提示

**Files:**
- Modify: `proxy/kiro_api.go`
- Modify: `proxy/kiro_region_test.go`
- Modify: `proxy/kiro_api_test.go`

- [ ] **Step 1：写入候选区域失败测试**

```go
func TestKiroProfileRegionCandidatesUsesIDCProfileHint(t *testing.T) {
    got := kiroProfileRegionCandidates(&config.Account{
        AuthMethod:       "idc",
        Region:           "us-east-1",
        ProfileRegionHint: "eu-central-1",
    })
    assertOrder(t, got, []string{"us-east-1", "eu-central-1"})
}

func TestKiroProfileRegionCandidatesKeepsNonDefaultAuthRegionAndHint(t *testing.T) {
    got := kiroProfileRegionCandidates(&config.Account{
        AuthMethod:       "idc",
        Region:           "eu-north-1",
        ProfileRegionHint: "us-east-1",
    })
    assertOrder(t, got, []string{"eu-north-1", "us-east-1"})
}
```

再扩展 Profile API 测试：第一个 Region 返回空 profiles，第二个 Region 返回 EU ARN，断言 ARN 被保存且数据面 URL 使用 EU。

- [ ] **Step 2：运行测试并确认旧单区逻辑失败**

Run:

```powershell
go test ./proxy -run 'TestKiroProfileRegionCandidatesUsesIDCProfileHint|TestKiroProfileRegionCandidatesKeepsNonDefaultAuthRegionAndHint|TestResolveProfileArn' -count=1
```

Expected: FAIL，候选列表缺少 `ProfileRegionHint`。

- [ ] **Step 3：实现候选区域和 ARN 校验**

`kiroProfileRegionCandidates` 顺序调整为：

```go
add(account.Region)
add(account.ProfileRegionHint)
```

只要 IdC 账号存在 `ProfileRegionHint`，就允许使用该提示；环境变量和内置回退仍按既有条件追加，避免无提示账号产生无界请求。

新增严格 ARN helper：

```go
func normalizeCodeWhispererProfileArn(raw string) (string, string, bool)
```

它必须验证 ARN 分区、`codewhisperer` service、非空 Region 和 `profile/` 资源，并返回规范化 ARN、Region 与是否合法。

- [ ] **Step 4：运行 Profile 和数据面测试**

Run:

```powershell
gofmt -w proxy/kiro_api.go proxy/kiro_region_test.go proxy/kiro_api_test.go
go test ./proxy -run 'TestKiroProfileRegion|TestResolveProfileArn|TestRegionalize' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交 Profile 路由变更**

```powershell
git add proxy/kiro_api.go proxy/kiro_region_test.go proxy/kiro_api_test.go
git commit -m "修复：按提示区域解析 IAM Profile"
```

---

### Task 4：修复凭据 JSON 导入

**Files:**
- Modify: `proxy/handler.go`
- Modify: `proxy/import_credentials_test.go`
- Modify: `web/app.js`

- [ ] **Step 1：写入导入失败测试**

增加三项测试：

```go
func TestApiImportCredentialsKeepsImportedProfileArnWhenRefreshOmitsIt(t *testing.T)
func TestApiImportCredentialsPrefersRefreshedProfileArn(t *testing.T)
func TestApiImportCredentialsDefaultsMissingIDCProviderToEnterprise(t *testing.T)
```

首个测试请求体包含：

```json
{
  "refreshToken": "rt-good",
  "clientId": "client",
  "clientSecret": "secret",
  "authMethod": "idc",
  "region": "us-east-1",
  "profileRegionHint": "eu-central-1",
  "profileArn": "arn:aws:codewhisperer:eu-central-1:123456789012:profile/example"
}
```

假 OIDC refresh 响应不返回 `profileArn`，测试断言导入 ARN、Enterprise Provider 和 Profile 区域提示被保存。

- [ ] **Step 2：运行测试并确认失败**

Run:

```powershell
go test ./proxy -run 'TestApiImportCredentials(KeepsImportedProfileArn|PrefersRefreshedProfileArn|DefaultsMissingIDCProvider)' -count=1
```

Expected: FAIL，当前请求结构不接收 ARN，Provider 仍为空。

- [ ] **Step 3：实现后端导入选择规则**

请求结构新增：

```go
ProfileArn       string `json:"profileArn"`
ProfileRegionHint string `json:"profileRegionHint"`
```

选择规则：

```go
profileArn := normalizeImportedProfileArn(req.ProfileArn)
if normalizedRefreshedArn != "" {
    profileArn = normalizedRefreshedArn
}
if req.AuthMethod == "idc" && strings.TrimSpace(req.Provider) == "" {
    req.Provider = "Enterprise"
}
```

如果 ARN 合法且 `ProfileRegionHint` 为空，从 ARN 提取 Region 填入提示字段。

- [ ] **Step 4：实现前端 JSON 映射**

`importCredentials()` 映射增加：

```javascript
profileArn: c.profileArn || a.profileArn || '',
profileRegionHint: c.profileRegionHint || a.profileRegionHint || ''
```

payload 同步发送这两个字段。旧 IdC JSON 缺失 Provider 时前端不再强制设置 `BuilderId`，交由后端可信规则处理。

- [ ] **Step 5：运行导入测试和语法检查**

Run:

```powershell
gofmt -w proxy/handler.go proxy/import_credentials_test.go
go test ./proxy -run 'TestApiImportCredentials' -count=1
node --check web/app.js
```

Expected: PASS。

- [ ] **Step 6：提交导入修复**

```powershell
git add proxy/handler.go proxy/import_credentials_test.go web/app.js
git commit -m "修复：导入时保留 Profile ARN 和企业身份"
```

---

### Task 5：修复凭据 JSON 导出

**Files:**
- Modify: `proxy/handler.go`
- Create: `proxy/export_accounts_test.go`

- [ ] **Step 1：写入失败测试，复现 BuilderId、Pro_Plus 和 ARN 丢失**

测试向临时配置写入：

```go
config.Account{
    ID:                "enterprise-power",
    AuthMethod:        "idc",
    Provider:          "Enterprise",
    Region:            "us-east-1",
    StartUrl:          "https://d-example.awsapps.com/start",
    ProfileRegionHint: "eu-central-1",
    ProfileArn:        "arn:aws:codewhisperer:eu-central-1:123456789012:profile/example",
    SubscriptionType:  "POWER",
    SubscriptionTitle: "KIRO POWER",
}
```

调用 `apiExportAccounts`，断言：

```text
idp=Enterprise
credentials.provider=Enterprise
credentials.profileArn=<EU ARN>
top-level profileArn=<EU ARN>
subscription.type=POWER
startUrl=<原值>
profileRegionHint=eu-central-1
```

- [ ] **Step 2：运行测试并确认失败**

Run:

```powershell
go test ./proxy -run 'TestApiExportAccountsPreservesEnterprisePowerProfile' -count=1
```

Expected: FAIL，现有导出缺少 ARN/Start URL，并将 Power 映射为 `Pro_Plus`。

- [ ] **Step 3：实现兼容导出字段和身份映射**

扩展 `ExportCredentials`：

```go
ProfileArn        string `json:"profileArn,omitempty"`
ProfileRegionHint string `json:"profileRegionHint,omitempty"`
StartURL          string `json:"startUrl,omitempty"`
```

扩展 `ExportAccount`：

```go
ProfileArn        string `json:"profileArn,omitempty"`
ProfileRegionHint string `json:"profileRegionHint,omitempty"`
StartURL          string `json:"startUrl,omitempty"`
```

身份与订阅映射按设计文档执行，先判断 `POWER`，再判断 `PRO_PLUS` 和 `PRO`。Provider 为空但 `AuthMethod=idc` 时导出为 Enterprise，不再导出 BuilderId。

- [ ] **Step 4：运行导出测试**

Run:

```powershell
gofmt -w proxy/handler.go proxy/export_accounts_test.go
go test ./proxy -run 'TestApiExportAccounts' -count=1
```

Expected: PASS。

- [ ] **Step 5：提交导出修复**

```powershell
git add proxy/handler.go proxy/export_accounts_test.go
git commit -m "修复：导出完整 IAM 企业凭据"
```

---

### Task 6：改造 IAM 登录弹窗和订阅状态

**Files:**
- Modify: `web/app.js`
- Modify: `web/styles.css`
- Modify: `web/locales/zh.json`
- Modify: `web/locales/en.json`
- Create: `proxy/admin_frontend_contract_test.go`

- [ ] **Step 1：写入静态前端契约失败测试**

由于项目没有 JavaScript 测试框架，使用 Go 测试读取静态资源，锁定关键字段而不模拟 DOM：

```go
func TestAdminFrontendIamSsoContract(t *testing.T) {
    app := readWebAsset(t, "../web/app.js")
    for _, expected := range []string{
        `id="iamProfileRegion"`,
        `id="iamAuthRegion"`,
        `profileRegion: $('iamProfileRegion').value`,
        `authRegion: $('iamAuthRegion').value`,
        `subscription.unknown`,
    } {
        if !strings.Contains(app, expected) {
            t.Fatalf("app.js missing %q", expected)
        }
    }
    if strings.Contains(app, `id="iamCallbackRegion"`) {
        t.Fatal("callback region field must not be added")
    }
}
```

再断言中英文 locale 都包含 `iam.profileRegion`、`iam.authRegion`、`subscription.unknown` 和刷新失败文案。

- [ ] **Step 2：运行测试并确认失败**

Run:

```powershell
go test ./proxy -run 'TestAdminFrontendIamSsoContract' -count=1
```

Expected: FAIL，字段和 Unknown 文案尚不存在。

- [ ] **Step 3：修改 IAM 登录弹窗**

弹窗显示：

```text
Start URL
Profile Region
高级设置：认证 Region（默认 us-east-1）
```

使用原生 `<details>` 展开高级设置，不添加“回调 Region”。请求体发送 `startUrl`、`profileRegion`、`authRegion`。

- [ ] **Step 4：修复 Unknown 与刷新错误**

调整：

```javascript
function formatSubscriptionLabel(type) {
  const s = (type || '').toUpperCase();
  if (!s || s === 'UNKNOWN') return t('subscription.unknown');
  // 其余既有映射保留
}
```

`getSubBadge` 对空值使用 `badge-muted`。`autoRefreshNewAccount` 必须解析非 2xx 响应并 `toastError`，不能使用空 catch。

IAM complete 返回 `warning` 时显示警告，但保留已登录账号。

- [ ] **Step 5：运行前端契约、JSON 和语法检查**

Run:

```powershell
go test ./proxy -run 'TestAdminFrontendIamSsoContract' -count=1
node --check web/app.js
Get-Content web/locales/zh.json -Raw | ConvertFrom-Json | Out-Null
Get-Content web/locales/en.json -Raw | ConvertFrom-Json | Out-Null
```

Expected: 全部成功。

- [ ] **Step 6：提交前端变更**

```powershell
git add web/app.js web/styles.css web/locales/zh.json web/locales/en.json proxy/admin_frontend_contract_test.go
git commit -m "修复：改进 IAM 登录弹窗和订阅状态"
```

---

### Task 7：完整验证与浏览器验收

**Files:**
- No production files unless verification reveals a scoped regression.

- [ ] **Step 1：运行格式与静态检查**

```powershell
gofmt -w auth/iam_sso.go auth/iam_sso_test.go config/config.go proxy/handler.go proxy/iam_sso_handler_test.go proxy/kiro_api.go proxy/kiro_region_test.go proxy/kiro_api_test.go proxy/import_credentials_test.go proxy/export_accounts_test.go proxy/admin_frontend_contract_test.go
go vet ./...
node --check web/app.js
```

Expected: 退出码 0。

- [ ] **Step 2：运行本次范围测试**

```powershell
go test ./auth ./config ./proxy -run 'Test(StartIamSso|CompleteIamSso|NormalizeIamSso|BuildIamSso|KiroProfileRegion|ResolveProfileArn|ApiImportCredentials|ApiExportAccounts|AdminFrontendIamSso)' -count=1
```

Expected: PASS。

- [ ] **Step 3：运行完整测试并对比既有失败**

```powershell
go test ./... -count=1
```

Expected: 本次新增测试全部通过；如果仍只有基线中的 `TestClaudeToolResultMixedTextAndImage` 与 `TestOpenAIToolResultImageCarriedWhenFollowedByUser` 失败，记录为既有 translator 问题，不在本次认证变更中修改。出现其他失败必须在结束前修复。

- [ ] **Step 4：启动本地管理页**

使用临时配置和未占用端口启动 KiroGo，避免连接生产账号。根据项目 CLI 参数确认命令；若默认端口可用，使用：

```powershell
go run .
```

启动后记录本地管理页 URL，不导入真实 token，不登录用户提供的账号。

- [ ] **Step 5：浏览器验证桌面与移动布局**

使用浏览器自动化检查：

- IAM Identity Center 卡片可进入。
- 弹窗包含 Start URL、Profile Region、折叠的认证 Region。
- 默认认证 Region 为 `us-east-1`。
- 不存在回调 Region。
- 最长 Region 文本不溢出。
- Unknown 徽标和错误 toast 不与按钮或内容重叠。

保存桌面和移动截图作为验证证据。

- [ ] **Step 6：审查最终差异和工作区**

```powershell
git diff HEAD~5 --check
git status --short
git log -6 --oneline
```

确认只包含本设计范围文件，不包含凭据、账号密码、callback URL、token、Cookie 或本地测试配置。

---

## 后续第八种登录的接入边界

“图图大王”的 API 登录在本计划完成后单独设计。它可以复用：

- `config.Account` 的统一账号字段。
- Profile ARN 校验和跨区解析。
- 登录后实时订阅刷新。
- Unknown/Free/Power 前端状态。

它不得复用 IAM 的 PKCE 会话、callback、OIDC client 注册或 refresh endpoint，除非实际代码审查证明协议完全一致。
