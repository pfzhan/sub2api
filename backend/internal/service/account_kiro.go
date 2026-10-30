package service

import "errors"

// IsKiro 报告账号是否为 Kiro 账号。
func (a *Account) IsKiro() bool {
	return a != nil && a.Platform == PlatformKiro
}

// validatePlatformAccountType 校验只支持单一账号类型的平台。
func validatePlatformAccountType(platform, accountType string) error {
	switch platform {
	case PlatformTypeSafe:
		if accountType != AccountTypeAPIKey {
			return errors.New("typesafe accounts only support apikey credentials")
		}
	case PlatformKiro:
		if accountType != AccountTypeOAuth {
			return errors.New("kiro accounts only support oauth credentials")
		}
	}
	return nil
}
