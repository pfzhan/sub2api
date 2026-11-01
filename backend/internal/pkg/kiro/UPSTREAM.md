# Kiro 协议参考来源

本包的协议细节参考 [AIClient2API](https://github.com/justlovemaki/AIClient2API) 的 Kiro 实现。两者许可证不同（AIClient2API 为 GPL-3.0），本包按协议行为独立实现，没有复制源码。

## 参考版本

| 项目 | 提交 | 日期 |
| --- | --- | --- |
| AIClient2API | `d91f950fd67df6a8be6d42f6f0164802b4813ee9` | 2026-10-26 |

同步上游修复时，先查看该提交之后以下文件的变更：

```bash
git log --oneline d91f950fd67df6a8be6d42f6f0164802b4813ee9..HEAD -- \
  src/providers/claude/claude-kiro.js \
  src/providers/claude/kiro-effort.js \
  src/providers/claude/kiro-profile.js \
  src/auth/kiro-oauth.js \
  src/services/usage-service.js
```

同步后更新上表中的提交和日期。

## 对应关系

| 本包 | AIClient2API |
| --- | --- |
| `auth.go` `Refresh` | `claude-kiro.js` `_doTokenRefresh` |
| `auth.go` `DiscoverProfileARN`、`ideHeaders` | `claude-kiro.js` `_discoverProfileArn`、`generateMachineIdFromConfig` |
| `auth.go` `RegisterClient`、`StartDeviceAuthorization`、`PollDeviceToken` | `kiro-oauth.js` `handleKiroBuilderIDDeviceCode`、`pollKiroBuilderIDToken` |
| `social.go` | `kiro-oauth.js` `handleKiroSocialAuth`、`createKiroHttpCallbackServer` |
| `usage.go` | `claude-kiro.js` `getUsageLimits`；`usage-service.js` `formatKiroUsage` |
| `credentials.go` `ParseImport` | `kiro-oauth.js` `importAwsCredentials`、`batchImportKiroRefreshTokens` |
| `request.go` `GenerateURL` | `kiro-profile.js` `shouldRouteBuilderToCodeWhisperer` |
| `request.go` `NewGenerateRequest` | `claude-kiro.js` `initialize`、`streamApiReal` 中的请求头 |
| `request.go` `BuildRequest` | `claude-kiro.js` `buildCodewhispererRequest` |
| `request.go` `additionalModelRequestFields` | `kiro-effort.js` `buildKiroAdditionalModelRequestFields` |
| `request.go` `ToolNames`、`buildToolEntries` | `claude-kiro.js` `shortenKiroToolName`、`buildKiroToolNameMaps`、`buildKiroPlaceholderTool` |
| `models.go` | `claude-kiro.js` `MODEL_CONTEXT_TOKENS`、`resolveKiroModel`；`provider-models.js` |
| `eventstream.go` | `claude-kiro.js` `parseAwsEventStreamBuffer`、`_readEventTypeHeader` |
| `stream.go` `Converter` | `claude-kiro.js` `_generateContentStreamRaw`、`generateContentStream`、`findRealTag`、`findRealThinkingEndTag` |
| `errors.go` `ClassifyForbidden` | `claude-kiro.js` `_isRefreshableForbidden` |
| `errors.go` `QuotaResetAt` | `claude-kiro.js` `_getNextMonthFirstDay` |

## 与参考实现的差异

- 不注入 AIClient2API 追加的身份提示词。
- 不解析正文里的 `[Called ...]` 文本工具调用，也不把字面量 `\n` 转成换行。这两项处理的是早期按文本解析响应时的遗留格式，按帧解码后不会出现。
- 同一条消息的多个文本块用换行连接；`tool_result` 的 `is_error` 映射为 `status: "error"`。
- event stream 解码校验两个 CRC，帧头不合法时逐字节重新对齐。
- 没有 profile 的 idc 凭据（不限于标记为 Builder ID 的）都走 `codewhisperer.*` 端点。
- 网络重试、限流冷却、账号切换由 sub2api 的通用网关流程处理，本包不做重试。
- social 登录不启动本地回调服务，由管理员把浏览器跳转到的 `kiro://` 回调地址粘贴回来；回调地址里的 state 必须与发起登录时一致。
- IAM Identity Center 登录与 Builder ID 共用设备码流程，SSO 区域与接口区域分开保存。
- 额度汇总只计入未过期的条目，不按"前后三个月"的时间窗过滤。
- Kiro 响应只有本次消耗的 credit（`meteringEvent`），没有缓存 token 数。AIClient2API 把缓存读写固定为 0；本包在 `prompt_cache.go` 中按 Anthropic prompt caching 的断点、回看块数和有效期规则在本地推算缓存读写，再按请求体字节比例从输入 token 中拆出。
