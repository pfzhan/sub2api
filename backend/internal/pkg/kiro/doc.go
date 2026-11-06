// Package kiro 实现 Kiro（Amazon Q Developer / CodeWhisperer）上游协议：
// 凭据导入与刷新，Builder ID / IAM Identity Center 设备码登录与 Google /
// GitHub 登录，额度查询，Anthropic Messages 请求到 conversationState 的转换，
// AWS event stream 解码，以及流事件到 Anthropic SSE 的转换。
//
// 本包只依赖 Doer 发送 HTTP 请求，不涉及账号、调度和计费，服务层负责把它
// 接入网关。
//
// 协议细节参考 AIClient2API 项目，参考的版本与对应关系记录在 UPSTREAM.md。
package kiro
