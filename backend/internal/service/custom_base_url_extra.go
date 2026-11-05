package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// validateCustomBaseURLExtra 校验 Anthropic OAuth/SetupToken 账号 extra 里的自定义中继地址。
// 开关打开且地址非空时，地址必须通过网关转发时的同一套校验，否则拒绝保存；
// 开关关闭或地址为空时中继不生效，不做校验。
func validateCustomBaseURLExtra(cfg *config.Config, platform, accountType string, extra map[string]any) error {
	if extra == nil || platform != PlatformAnthropic || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) {
		return nil
	}
	if enabled, _ := extra["custom_base_url_enabled"].(bool); !enabled {
		return nil
	}
	raw, _ := extra["custom_base_url"].(string)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if _, err := validateGatewayUpstreamBaseURL(cfg, raw); err != nil {
		return infraerrors.BadRequest("INVALID_CUSTOM_BASE_URL", "custom_base_url is invalid: "+err.Error())
	}
	return nil
}
