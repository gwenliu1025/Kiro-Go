# Kiro-Go 改造前基线

日期：2026-07-14

## 代码基线

- 设计基线提交：`2ab979f`
- 实施分支：`feat/kiro-native-high-cache`
- 实施计划提交：`accf5d4`

## 已确认的既有测试契约冲突

改造前的 `go test ./...` 稳定失败于：

- `TestClaudeToolResultMixedTextAndImage`
- `TestOpenAIToolResultImageCarriedWhenFollowedByUser`

根因不是本次高缓存改造。提交 `72da572` 已要求孤立或历史工具结果扁平化，以避免 Kiro 上游拒绝历史结构化工具结果；提交 `2ad0c56` 新增的两个图片用例仍断言旧的结构化 `ToolResults` 形态。

定向修正测试后还暴露出一个真实基线缺陷：Claude 孤立工具结果同时携带文本和图片时，图片占位文本会抢先成为 `finalContent`，导致工具结果正文丢失。基线修复将扁平化后的工具正文追加到图片提示后，不恢复上游不接受的结构化历史工具结果。

基线修正包含测试契约更新和上述最小正文保留修复，继续验证：

- 图片保留在正确的当前消息或历史消息；
- 工具结果文本没有丢失；
- 历史工具结果已经扁平化；
- 工具图片不会泄漏到后续用户消息。

## 验证命令

```powershell
go test ./proxy -run '^(TestClaudeToolResultMixedTextAndImage|TestOpenAIToolResultImageCarriedWhenFollowedByUser)$' -count=1 -v
go test ./...
go build ./...
go vet ./...
```

## Race 环境限制

当前 Windows 环境默认 `CGO_ENABLED=0`，且未安装 `gcc`，因此本机不能执行有效的 `go test -race ./...`。最终竞态验证必须在 Linux CI 或毕业机执行，不能用普通测试替代。
