package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 预编译正则表达式（避免每次调用重新编译）
var (
	// 匹配 User-Agent 版本号: xxx/x.y.z
	userAgentVersionRegex = regexp.MustCompile(`/(\d+)\.(\d+)\.(\d+)`)

	// fingerprintUserAgentPattern 校验可写入账号级持久身份的 User-Agent 形态：
	// <product>/<major>.<minor>.<patch> 之后必须紧跟空白或字符串结束。
	// 版本号带 -local / -dev / +build 等后缀的本地构建一律不接受。
	fingerprintUserAgentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/\d+\.\d+\.\d+(\s|$)`)

	// claudeCLIUAVersionPrefixRegex 匹配 claude-cli UA 开头的 "claude-cli/x.y.z" 版本号段，
	// 供版本下限抬升时就地替换版本号使用；UA 其余部分（括号内的真实客户端形态等）原样保留。
	claudeCLIUAVersionPrefixRegex = regexp.MustCompile(`(?i)^(claude-cli)/\d+\.\d+\.\d+`)
)

const (
	// claudeCLIUserAgentProduct 是官方 Claude Code CLI 的产品名（小写）。
	claudeCLIUserAgentProduct = "claude-cli"
	// maxFingerprintUserAgentLength 限制写入缓存的 User-Agent 长度。
	maxFingerprintUserAgentLength = 256
	// maxClaudeCLIMajorVersionSkew 是 claude-cli 主版本号相对 sub2api 自身伪装
	// 版本（claude.CLIVersion()）允许的最大超前量。给足两个大版本的升级
	// 窗口，同时挡掉 999 这类哨兵版本号。
	maxClaudeCLIMajorVersionSkew = 2
)

// isAcceptableFingerprintUserAgent 判断 User-Agent 是否可作为账号级持久身份写入缓存。
//
// 指纹是账号级、“只升不降”、活跃账号懒续期后近乎永不过期的持久状态，且系统内
// 没有重置入口。一旦写入畸形或哨兵版本（如 claude-cli/999.0.0-local），该账号
// 此后所有上游请求都会在 HTTP 头与请求体 cc_version 两处声称这个不存在的版本，
// 被上游判定为非正版客户端并持续返回不带限流重置头的 429；无重置头又会落到 5 秒
// 兜底冷却，账号池收缩后对外表现为 503 风暴。
//
// 校验必须放在创建与升级两条路径的共同入口：只在 isNewerVersion 处加校验是不够的，
// createFingerprintFromHeaders 首次创建时同样会原样保存畸形 UA，删键恢复后账号可被
// 同一客户端立即再次毒化。
func isAcceptableFingerprintUserAgent(ua string) bool {
	ua = strings.TrimSpace(ua)
	if ua == "" || len(ua) > maxFingerprintUserAgentLength {
		return false
	}
	if !fingerprintUserAgentPattern.MatchString(ua) {
		return false
	}
	// 非 claude-cli 产品不做版本区间约束：形态合法即可，避免误伤其他合法客户端。
	if extractProduct(ua) != claudeCLIUserAgentProduct {
		return true
	}
	major, _, _, ok := parseUserAgentVersion(ua)
	if !ok {
		return false
	}
	// 超前检查以运行期生效版本为基准（EffectiveCLIVersion）：同步跨大版本后，
	// 若仍与静态基线比较会误拒客户端上报的新版本。
	currentMajor, _, _, currentOK := parseUserAgentVersion(claudeCLIUserAgentProduct + "/" + claude.EffectiveCLIVersion())
	if !currentOK {
		return true
	}
	return major <= currentMajor+maxClaudeCLIMajorVersionSkew
}

// floorClaudeCLIUserAgentVersion 把 claude-cli UA 中的版本号抬升到运行期生效版本
// （claude.EffectiveCLIVersion()）下限：低于下限时就地升到下限并返回 changed=true，否则原样返回。
//
// 为什么需要这条：账号级指纹"只升不降"、活跃账号懒续期后近乎永不过期，且系统内没有重置
// 入口，defaultFingerprint 只在首次创建指纹时使用。存量账号缓存里的 claude-cli 版本停留在
// 历史值（如 claude-cli/2.1.220），客户端送来更旧的版本时 isNewerVersion 不会触发升级——
// 仅升 CLICurrentVersion 常量对所有已有账号完全无效，上游按指纹 UA 做客户端版本闸门
// （如 Fable 5.1 要求 >= 2.1.251）时旧指纹永远过不去。
//
// 约束：
//   - 只对 claude-cli 产品生效，其它产品一律不动，避免误伤别的合法客户端；
//   - 只升不降：版本等于或高于 CLICurrentVersion（含客户端上报的更新版本）时不做任何改动；
//   - 只替换 claude-cli/ 后的版本号段，UA 其余部分（如 "(external, claude-desktop-3p,
//     agent-sdk/0.3.100)"）原样保留，不重建整个字符串、不退化为 defaultFingerprint.UserAgent；
//   - X-Stainless-* 字段不在此处理，维持调用方的既有 merge 语义。
func floorClaudeCLIUserAgentVersion(ua string) (string, bool) {
	if extractProduct(ua) != claudeCLIUserAgentProduct {
		return ua, false
	}
	// 用 EffectiveCLIVersion()（面板/同步的运行期值，经 IsSupportedCLIVersion
	// 校验、恒 >= 内置基线）而非静态基线 CLICurrentVersion：
	// 运行期同步到更高版本后，存量低版本指纹才能被抬到新版本；"只升不降"语义不变。
	floor := claude.EffectiveCLIVersion()
	floorUA := claudeCLIUserAgentProduct + "/" + floor
	// isNewerVersion(floor, ua) 为 true 当且仅当下限版本严格高于 ua：
	// ua 等于或高于下限、产品名不一致、或版本无法解析时都不做改动。
	if !isNewerVersion(floorUA, ua) {
		return ua, false
	}
	floored := claudeCLIUAVersionPrefixRegex.ReplaceAllString(ua, "${1}/"+floor)
	if floored == ua {
		return ua, false
	}
	return floored, true
}

// defaultFingerprint 返回默认指纹值（当客户端未提供时使用）。
// UserAgent 不能在包 init 时固化：必须在每次使用时经 EffectiveCLIVersion 现取，
// 否则运行期同步到新版本后，新建账号会写入过期版本作为持久身份。
func defaultFingerprint() Fingerprint {
	return Fingerprint{
		UserAgent:               claude.DefaultUserAgent(),
		StainlessLang:           "js",
		StainlessPackageVersion: "0.94.0",
		StainlessOS:             "Linux",
		StainlessArch:           "arm64",
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: "v24.3.0",
	}
}

// Fingerprint represents account fingerprint data
type Fingerprint struct {
	ClientID                string
	UserAgent               string
	StainlessLang           string
	StainlessPackageVersion string
	StainlessOS             string
	StainlessArch           string
	StainlessRuntime        string
	StainlessRuntimeVersion string
	UpdatedAt               int64 `json:",omitempty"` // Unix timestamp，用于判断是否需要续期TTL
}

// IdentityCache defines cache operations for identity service
type IdentityCache interface {
	GetFingerprint(ctx context.Context, accountID int64) (*Fingerprint, error)
	SetFingerprint(ctx context.Context, accountID int64, fp *Fingerprint) error
	// GetMaskedSessionID 获取固定的会话ID（用于会话ID伪装功能）
	// 返回的 sessionID 是一个 UUID 格式的字符串
	// 如果不存在或已过期（默认 2 小时无请求），返回空字符串
	GetMaskedSessionID(ctx context.Context, accountID int64) (string, error)
	// SetMaskedSessionID 写入伪装 session ID 并设置 2 小时 TTL。
	// 只在首次生成或过期重建时调用，不要每请求 SET。
	SetMaskedSessionID(ctx context.Context, accountID int64, sessionID string) error
	// TouchMaskedSessionID 在剩余 TTL 低于刷新阈值时把 TTL 续到 2 小时。
	// TTL 仍充足时不写 Redis。
	TouchMaskedSessionID(ctx context.Context, accountID int64) error
}

// IdentityService 管理OAuth账号的请求身份指纹
type IdentityService struct {
	cache IdentityCache
}

// NewIdentityService 创建新的IdentityService
func NewIdentityService(cache IdentityCache) *IdentityService {
	return &IdentityService{cache: cache}
}

// GetOrCreateFingerprint 获取或创建账号的指纹
// 如果缓存存在，检测user-agent版本，新版本则更新
// 如果缓存不存在，生成随机ClientID并从请求头创建指纹，然后缓存
func (s *IdentityService) GetOrCreateFingerprint(ctx context.Context, accountID int64, headers http.Header) (*Fingerprint, error) {
	// 入口统一校验：创建与升级两条路径共用，任一路径漏掉都会让畸形 UA 被持久化。
	clientUA := strings.TrimSpace(headers.Get("User-Agent"))
	uaAcceptable := isAcceptableFingerprintUserAgent(clientUA)

	// 尝试从缓存获取指纹
	cached, err := s.cache.GetFingerprint(ctx, accountID)
	if err == nil && cached != nil {
		needWrite := false

		// 只在真正阻止了一次写入时记录，便于定位污染源，同时避免被毒化客户端的
		// 高频重试刷屏（无重置头的 429 会落到 5 秒兜底冷却，重试相当密集）。
		if !uaAcceptable && clientUA != "" && isNewerVersion(clientUA, cached.UserAgent) {
			logger.LegacyPrintf("service.identity",
				"Rejected fingerprint user-agent for account %d: %q (malformed or implausible version)",
				accountID, clientUA)
		}

		if !isAcceptableFingerprintUserAgent(cached.UserAgent) {
			// 自愈：缓存中已是畸形/哨兵 UA（本次加固之前写入的）。指纹在活跃账号上
			// 懒续期后近乎永不过期，且系统内没有重置入口——不在读取时纠正，存量被
			// 毒化的账号就只能靠手工删 Redis 键恢复。
			poisoned := cached.UserAgent
			if uaAcceptable {
				mergeHeadersIntoFingerprint(cached, headers)
			} else {
				cached.UserAgent = defaultFingerprint().UserAgent
			}
			needWrite = true
			logger.LegacyPrintf("service.identity",
				"Replaced malformed cached fingerprint for account %d: %q -> %q",
				accountID, poisoned, cached.UserAgent)
		} else {
			// 客户端送来更新版本时的常规升级：merge 语义 — 仅更新请求中实际携带的字段，
			// 保留缓存值，避免缺失的头被硬编码默认值覆盖（如新 CLI 版本 + 旧 SDK 默认值的不一致）
			if uaAcceptable && isNewerVersion(clientUA, cached.UserAgent) {
				mergeHeadersIntoFingerprint(cached, headers)
				needWrite = true
				logger.LegacyPrintf("service.identity", "Updated fingerprint for account %d: %s (merge update)", accountID, clientUA)
			}

		}

		// 版本下限抬升（floor）：heal 与 healthy 两条缓存命中路径共同的后置条件，与
		// 上面的客户端常规升级相互独立、二者取更新者。客户端送来更旧版本时
		// isNewerVersion 不触发，此处仍能把低于 CLICurrentVersion 的存量指纹就地抬到
		// 下限并持久化，否则升 CLICurrentVersion 对所有已有账号无效，新模型的客户端
		// 版本闸门（如 Fable 5.1 要求 >= 2.1.251）永远过不去。自愈路径同样必须经过：
		// 畸形缓存 + 合法但过旧的客户端 UA 首次读取就要落到下限，不能先落库一个
		// 过旧版本、等下一次读取才纠正。
		if flooredUA, changed := floorClaudeCLIUserAgentVersion(cached.UserAgent); changed {
			cached.UserAgent = flooredUA
			needWrite = true
			logger.LegacyPrintf("service.identity",
				"Floored cached fingerprint claude-cli version for account %d: %s", accountID, flooredUA)
		}

		if !needWrite && time.Since(time.Unix(cached.UpdatedAt, 0)) > 24*time.Hour {
			// 距上次写入超过24小时，续期TTL
			needWrite = true
		}

		if needWrite {
			cached.UpdatedAt = time.Now().Unix()
			if err := s.cache.SetFingerprint(ctx, accountID, cached); err != nil {
				logger.LegacyPrintf("service.identity", "Warning: failed to refresh fingerprint for account %d: %v", accountID, err)
			}
		}
		return cached, nil
	}

	// 缓存不存在或解析失败，创建新指纹。首次创建同样是持久化写入，
	// 畸形 UA 在这里落库后就成了账号的长期身份，必须同样拒绝。
	if !uaAcceptable && clientUA != "" {
		logger.LegacyPrintf("service.identity",
			"Rejected fingerprint user-agent for account %d: %q (malformed or implausible version)",
			accountID, clientUA)
	}
	fp := s.createFingerprintFromHeaders(headers)

	// 生成随机ClientID
	fp.ClientID = generateClientID()
	fp.UpdatedAt = time.Now().Unix()

	// 读缓存失败（连接、超时等）时缓存里可能仍有指纹，写回会用新 ClientID 和
	// 默认值把它覆盖。本次请求使用临时指纹，不落库。
	if err != nil && !fingerprintCacheMissing(err) {
		logger.LegacyPrintf("service.identity",
			"Warning: failed to read fingerprint for account %d, using a temporary one without caching: %v", accountID, err)
		return fp, nil
	}

	// 保存到缓存（7天TTL，每24小时自动续期）
	if err := s.cache.SetFingerprint(ctx, accountID, fp); err != nil {
		logger.LegacyPrintf("service.identity", "Warning: failed to cache fingerprint for account %d: %v", accountID, err)
	}

	logger.LegacyPrintf("service.identity", "Created new fingerprint for account %d with client_id: %s", accountID, fp.ClientID)
	return fp, nil
}

// fingerprintCacheMissing 报告读取错误是否表示缓存里没有可用指纹：键不存在
// 或内容无法解析。只有这两种情况可以新建指纹并写回。
func fingerprintCacheMissing(err error) bool {
	if errors.Is(err, redis.Nil) {
		return true
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

// createFingerprintFromHeaders 从请求头创建指纹
func (s *IdentityService) createFingerprintFromHeaders(headers http.Header) *Fingerprint {
	fp := &Fingerprint{}

	// 获取User-Agent：只接受形态合法且版本合理的值，否则回退默认指纹。
	// 首次创建同样是持久化写入，必须与升级路径共用同一套校验。
	if ua := strings.TrimSpace(headers.Get("User-Agent")); isAcceptableFingerprintUserAgent(ua) {
		// 首次创建与缓存命中路径共用同一个版本下限：合法但过旧的 claude-cli UA
		// 落库时同样不能低于运行期生效版本（EffectiveCLIVersion），否则新账号一开始就带着过旧的持久身份。
		fp.UserAgent, _ = floorClaudeCLIUserAgentVersion(ua)
	} else {
		fp.UserAgent = defaultFingerprint().UserAgent
	}

	applyStainlessToFingerprint(fp, headers, true)
	return fp
}

// stainlessLooksLikeClaudeCode 判断请求头是否是 Claude Code 的 js/node Stainless SDK。
// Python/Java 等 SDK 的 X-Stainless-* 与 claude-cli User-Agent 混在一起是第三方信号。
func stainlessLooksLikeClaudeCode(headers http.Header) bool {
	lang := strings.TrimSpace(headers.Get("X-Stainless-Lang"))
	runtime := strings.TrimSpace(headers.Get("X-Stainless-Runtime"))
	return strings.EqualFold(lang, "js") && strings.EqualFold(runtime, "node")
}

// applyStainlessToFingerprint 写入 Claude Code 形态的 Stainless 字段。
// OS/Arch 是机器身份，可从任意客户端保留；lang/package/runtime 只接受 js/node，
// 否则用 defaultFingerprint，避免首个 Python SDK 请求把账号指纹毒化成非 CLI。
func applyStainlessToFingerprint(fp *Fingerprint, headers http.Header, fillDefaults bool) {
	if fp == nil {
		return
	}
	df := defaultFingerprint()
	if stainlessLooksLikeClaudeCode(headers) {
		applyStainlessHeader(fp, headers, fillDefaults, df)
	} else if fillDefaults {
		fp.StainlessLang = df.StainlessLang
		fp.StainlessPackageVersion = df.StainlessPackageVersion
		fp.StainlessRuntime = df.StainlessRuntime
		fp.StainlessRuntimeVersion = df.StainlessRuntimeVersion
	}
	if osName := strings.TrimSpace(headers.Get("X-Stainless-OS")); osName != "" {
		fp.StainlessOS = osName
	} else if fillDefaults && fp.StainlessOS == "" {
		fp.StainlessOS = df.StainlessOS
	}
	if arch := strings.TrimSpace(headers.Get("X-Stainless-Arch")); arch != "" {
		fp.StainlessArch = arch
	} else if fillDefaults && fp.StainlessArch == "" {
		fp.StainlessArch = df.StainlessArch
	}
}

// mergeHeadersIntoFingerprint 将请求头中实际存在的字段合并到现有指纹中（用于版本升级场景）
// 关键语义：请求中有的字段 → 用新值覆盖；缺失的头 → 保留缓存中的已有值。
// 非 js/node 的 Stainless lang/runtime/package 不合并，避免 Python SDK 毒化 CLI 指纹。
func mergeHeadersIntoFingerprint(fp *Fingerprint, headers http.Header) {
	// User-Agent：版本升级的触发条件，一定存在
	if ua := headers.Get("User-Agent"); ua != "" {
		fp.UserAgent = ua
	}
	applyStainlessToFingerprint(fp, headers, false)
}

func applyStainlessHeader(fp *Fingerprint, headers http.Header, fillDefaults bool, df Fingerprint) {
	set := func(dst *string, key, fallback string) {
		if v := strings.TrimSpace(headers.Get(key)); v != "" {
			*dst = v
			return
		}
		if fillDefaults {
			*dst = fallback
		}
	}
	set(&fp.StainlessLang, "X-Stainless-Lang", df.StainlessLang)
	set(&fp.StainlessPackageVersion, "X-Stainless-Package-Version", df.StainlessPackageVersion)
	set(&fp.StainlessRuntime, "X-Stainless-Runtime", df.StainlessRuntime)
	set(&fp.StainlessRuntimeVersion, "X-Stainless-Runtime-Version", df.StainlessRuntimeVersion)
}

// getHeaderOrDefault 获取header值，如果不存在则返回默认值
func getHeaderOrDefault(headers http.Header, key, defaultValue string) string {
	if v := headers.Get(key); v != "" {
		return v
	}
	return defaultValue
}

// ApplyFingerprint 将指纹应用到请求头（覆盖原有的x-stainless-*头）
// 使用 setHeaderRaw 保持原始大小写（如 X-Stainless-OS 而非 X-Stainless-Os）
func (s *IdentityService) ApplyFingerprint(req *http.Request, fp *Fingerprint) {
	if fp == nil {
		return
	}

	// 设置user-agent
	if fp.UserAgent != "" {
		setHeaderRaw(req.Header, "User-Agent", fp.UserAgent)
	}

	// 设置x-stainless-*头（保持与 claude.DefaultHeaders() 一致的大小写）
	if fp.StainlessLang != "" {
		setHeaderRaw(req.Header, "X-Stainless-Lang", fp.StainlessLang)
	}
	if fp.StainlessPackageVersion != "" {
		setHeaderRaw(req.Header, "X-Stainless-Package-Version", fp.StainlessPackageVersion)
	}
	if fp.StainlessOS != "" {
		setHeaderRaw(req.Header, "X-Stainless-OS", fp.StainlessOS)
	}
	if fp.StainlessArch != "" {
		setHeaderRaw(req.Header, "X-Stainless-Arch", fp.StainlessArch)
	}
	if fp.StainlessRuntime != "" {
		setHeaderRaw(req.Header, "X-Stainless-Runtime", fp.StainlessRuntime)
	}
	if fp.StainlessRuntimeVersion != "" {
		setHeaderRaw(req.Header, "X-Stainless-Runtime-Version", fp.StainlessRuntimeVersion)
	}
}

// RewriteUserID 重写body中的metadata.user_id
// 支持旧拼接格式和新 JSON 格式的 user_id 解析，
// 根据 fingerprintUA 版本选择输出格式。
//
// 重要：此函数使用 json.RawMessage 保留其他字段的原始字节，
// 避免重新序列化导致 thinking 块等内容被修改。
func (s *IdentityService) RewriteUserID(body []byte, accountID int64, accountUUID, cachedClientID, fingerprintUA string) ([]byte, error) {
	if len(body) == 0 || accountUUID == "" || cachedClientID == "" {
		return body, nil
	}

	metadata := gjson.GetBytes(body, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return body, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return body, nil
	}

	userIDResult := metadata.Get("user_id")
	if !userIDResult.Exists() || userIDResult.Type != gjson.String {
		return body, nil
	}
	userID := userIDResult.String()
	if userID == "" {
		return body, nil
	}

	// 解析 user_id（兼容旧拼接格式和新 JSON 格式）
	parsed := ParseMetadataUserID(userID)
	if parsed == nil {
		return body, nil
	}

	sessionTail := parsed.SessionID // 原始session UUID

	// 生成新的session hash: SHA256(accountID::sessionTail) -> UUID格式
	seed := fmt.Sprintf("%d::%s", accountID, sessionTail)
	newSessionHash := generateUUIDFromSeed(seed)

	// 根据客户端版本选择输出格式
	version := ExtractCLIVersion(fingerprintUA)
	newUserID := FormatMetadataUserID(cachedClientID, accountUUID, newSessionHash, version)
	if newUserID == userID {
		return body, nil
	}

	newBody, err := sjson.SetBytes(body, "metadata.user_id", newUserID)
	if err != nil {
		return body, nil
	}
	return newBody, nil
}

// RewriteUserIDWithMasking 重写body中的metadata.user_id，支持会话ID伪装
// 如果账号启用了会话ID伪装（session_id_masking_enabled，OAuth 账号默认开启），
// 则在完成常规重写后，将 session 部分替换为固定的伪装ID（有效期内保持不变）。
// apiKeyID > 0 时伪装 ID 按 API Key 区分，同一账号下不同 API Key 不共用会话。
//
// 重要：此函数使用 json.RawMessage 保留其他字段的原始字节，
// 避免重新序列化导致 thinking 块等内容被修改。
func (s *IdentityService) RewriteUserIDWithMasking(ctx context.Context, body []byte, account *Account, apiKeyID int64, accountUUID, cachedClientID, fingerprintUA string) ([]byte, error) {
	// 先执行常规的 RewriteUserID 逻辑
	newBody, err := s.RewriteUserID(body, account.ID, accountUUID, cachedClientID, fingerprintUA)
	if err != nil {
		return newBody, err
	}

	// 检查是否启用会话ID伪装
	if !account.IsSessionIDMaskingEnabled() {
		return newBody, nil
	}

	metadata := gjson.GetBytes(newBody, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return newBody, nil
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return newBody, nil
	}

	userIDResult := metadata.Get("user_id")
	if !userIDResult.Exists() || userIDResult.Type != gjson.String {
		return newBody, nil
	}
	userID := userIDResult.String()
	if userID == "" {
		return newBody, nil
	}

	// 解析已重写的 user_id
	uidParsed := ParseMetadataUserID(userID)
	if uidParsed == nil {
		return newBody, nil
	}

	// 获取或生成固定的伪装 session ID
	maskedSessionID, err := s.cache.GetMaskedSessionID(ctx, account.ID)
	if err != nil {
		logger.LegacyPrintf("service.identity", "Warning: failed to get masked session ID for account %d: %v", account.ID, err)
		return newBody, nil
	}

	if maskedSessionID == "" {
		// 首次或已过期，生成新的伪装 session ID
		maskedSessionID = generateRandomUUID()
		logger.LegacyPrintf("service.identity", "Generated new masked session ID for account %d: %s", account.ID, maskedSessionID)
		if err := s.cache.SetMaskedSessionID(ctx, account.ID, maskedSessionID); err != nil {
			logger.LegacyPrintf("service.identity", "Warning: failed to set masked session ID for account %d: %v", account.ID, err)
		}
	} else if err := s.cache.TouchMaskedSessionID(ctx, account.ID); err != nil {
		logger.LegacyPrintf("service.identity", "Warning: failed to touch masked session ID for account %d: %v", account.ID, err)
	}

	maskedSessionID = maskedSessionIDForAPIKey(maskedSessionID, apiKeyID)

	// 用 FormatMetadataUserID 重建（保持与 RewriteUserID 相同的格式）
	version := ExtractCLIVersion(fingerprintUA)
	newUserID := FormatMetadataUserID(uidParsed.DeviceID, uidParsed.AccountUUID, maskedSessionID, version)

	slog.Debug("session_id_masking_applied",
		"account_id", account.ID,
		"before", userID,
		"after", newUserID,
	)

	if newUserID == userID {
		return newBody, nil
	}

	maskedBody, setErr := sjson.SetBytes(newBody, "metadata.user_id", newUserID)
	if setErr != nil {
		return newBody, nil
	}
	return maskedBody, nil
}

// maskedSessionIDForAPIKey 从账号级伪装 session ID 派生出该 API Key 的伪装 ID。
// 同一账号、同一 API Key 在账号级 ID 有效期内得到相同结果，账号级 ID 轮换时随之
// 轮换；apiKeyID <= 0（取不到 API Key）时沿用账号级 ID。
func maskedSessionIDForAPIKey(accountSessionID string, apiKeyID int64) string {
	if apiKeyID <= 0 || accountSessionID == "" {
		return accountSessionID
	}
	sum := sha256.Sum256([]byte(accountSessionID + ":" + strconv.FormatInt(apiKeyID, 10)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateRandomUUID 生成随机 UUID v4 格式字符串
func generateRandomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback: 使用时间戳生成
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		b = h[:16]
	}

	// 设置 UUID v4 版本和变体位
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// generateClientID 生成64位十六进制客户端ID（32字节随机数）
func generateClientID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// 极罕见的情况，使用时间戳+固定值作为fallback
		logger.LegacyPrintf("service.identity", "Warning: crypto/rand.Read failed: %v, using fallback", err)
		// 使用SHA256(当前纳秒时间)作为fallback
		h := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(h[:])
	}
	return hex.EncodeToString(b)
}

// generateUUIDFromSeed 从种子生成确定性UUID v4格式字符串
func generateUUIDFromSeed(seed string) string {
	hash := sha256.Sum256([]byte(seed))
	bytes := hash[:16]

	// 设置UUID v4版本和变体位
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// parseUserAgentVersion 解析user-agent版本号
// 例如：claude-cli/2.1.2 -> (2, 1, 2)
func parseUserAgentVersion(ua string) (major, minor, patch int, ok bool) {
	// 匹配 xxx/x.y.z 格式
	matches := userAgentVersionRegex.FindStringSubmatch(ua)
	if len(matches) != 4 {
		return 0, 0, 0, false
	}
	major, _ = strconv.Atoi(matches[1])
	minor, _ = strconv.Atoi(matches[2])
	patch, _ = strconv.Atoi(matches[3])
	return major, minor, patch, true
}

// extractProduct 提取 User-Agent 中 "/" 前的产品名
// 例如：claude-cli/2.1.22 (external, cli) -> "claude-cli"
func extractProduct(ua string) string {
	if idx := strings.Index(ua, "/"); idx > 0 {
		return strings.ToLower(ua[:idx])
	}
	return ""
}

// isNewerVersion 比较版本号，判断newUA是否比cachedUA更新
// 要求产品名一致（防止浏览器 UA 如 Mozilla/5.0 误判为更新版本）
func isNewerVersion(newUA, cachedUA string) bool {
	// 校验产品名一致性
	newProduct := extractProduct(newUA)
	cachedProduct := extractProduct(cachedUA)
	if newProduct == "" || cachedProduct == "" || newProduct != cachedProduct {
		return false
	}

	newMajor, newMinor, newPatch, newOk := parseUserAgentVersion(newUA)
	cachedMajor, cachedMinor, cachedPatch, cachedOk := parseUserAgentVersion(cachedUA)

	if !newOk || !cachedOk {
		return false
	}

	// 比较版本号
	if newMajor > cachedMajor {
		return true
	}
	if newMajor < cachedMajor {
		return false
	}

	if newMinor > cachedMinor {
		return true
	}
	if newMinor < cachedMinor {
		return false
	}

	return newPatch > cachedPatch
}
