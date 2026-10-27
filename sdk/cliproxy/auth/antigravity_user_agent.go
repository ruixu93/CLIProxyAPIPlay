package auth

import "strings"

// AntigravityUserAgentOverride returns the per-account User-Agent override and
// platform shorthand. Attributes take precedence over file metadata.
func AntigravityUserAgentOverride(auth *Auth) (userAgent, platform string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		userAgent = strings.TrimSpace(auth.Attributes["user_agent"])
		platform = strings.TrimSpace(auth.Attributes["ua_platform"])
	}
	if auth.Metadata != nil {
		if userAgent == "" {
			if value, ok := auth.Metadata["user_agent"].(string); ok {
				userAgent = strings.TrimSpace(value)
			}
		}
		if platform == "" {
			if value, ok := auth.Metadata["ua_platform"].(string); ok {
				platform = strings.TrimSpace(value)
			}
		}
	}
	return userAgent, platform
}
