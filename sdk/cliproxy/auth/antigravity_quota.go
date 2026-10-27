package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	homekv "github.com/router-for-me/CLIProxyAPI/v8/internal/home"
)

// AntigravityQuotaHintRefreshInterval bounds how often the CloudCode quota
// summary is queried for one auth.
const AntigravityQuotaHintRefreshInterval = 10 * time.Minute

// AntigravityQuotaBucket is one bucket from retrieveUserQuotaSummary.
type AntigravityQuotaBucket struct {
	BucketID          string    `json:"bucket_id"`
	Window            string    `json:"window,omitempty"`
	ResetTime         time.Time `json:"reset_time,omitempty"`
	RemainingFraction float64   `json:"remaining_fraction"`
}

// AntigravityQuotaGroup is one model-family group from retrieveUserQuotaSummary.
type AntigravityQuotaGroup struct {
	DisplayName string                   `json:"display_name,omitempty"`
	Description string                   `json:"description,omitempty"`
	Buckets     []AntigravityQuotaBucket `json:"buckets,omitempty"`
}

// AntigravityQuotaHint stores the latest quota summary for one auth.
type AntigravityQuotaHint struct {
	Known     bool                    `json:"known"`
	Groups    []AntigravityQuotaGroup `json:"groups,omitempty"`
	UpdatedAt time.Time               `json:"updated_at"`
}

// IsAntigravityQuotaHintFresh reports whether a known quota hint is still fresh.
func IsAntigravityQuotaHintFresh(hint AntigravityQuotaHint, now time.Time) bool {
	return hint.Known && !hint.UpdatedAt.IsZero() && now.Sub(hint.UpdatedAt) < AntigravityQuotaHintRefreshInterval
}

var antigravityQuotaHintByAuth sync.Map

// SetAntigravityQuotaHint updates the latest quota summary for one auth.
// The local copy is always retained so scheduler reads do not depend on Home KV.
func SetAntigravityQuotaHint(authID string, hint AntigravityQuotaHint) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if hint.UpdatedAt.IsZero() {
		hint.UpdatedAt = time.Now()
	}
	antigravityQuotaHintByAuth.Store(authID, hint)
	if _, homeMode, _ := homekv.CurrentKVClient(); homeMode {
		homekv.KVSetJSONBestEffort(context.Background(), antigravityQuotaHintKey(authID), hint, 30*time.Minute)
	}
}

// GetAntigravityQuotaHint returns the local quota summary for one auth.
func GetAntigravityQuotaHint(authID string) (AntigravityQuotaHint, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return AntigravityQuotaHint{}, false
	}
	value, ok := antigravityQuotaHintByAuth.Load(authID)
	if !ok {
		return AntigravityQuotaHint{}, false
	}
	hint, ok := value.(AntigravityQuotaHint)
	if !ok {
		antigravityQuotaHintByAuth.Delete(authID)
		return AntigravityQuotaHint{}, false
	}
	return hint, true
}

func antigravityQuotaHintKey(authID string) string {
	return "cpa:antigravity:quota-hint:" + strings.TrimSpace(authID)
}

// AntigravityQuotaAllowsModel reports whether the latest quota summary still
// permits this model family. Unknown or stale data is optimistic.
func AntigravityQuotaAllowsModel(auth *Auth, model string, now time.Time) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return true
	}
	hint, ok := GetAntigravityQuotaHint(auth.ID)
	if !ok || !IsAntigravityQuotaHintFresh(hint, now) {
		return true
	}
	relevant := antigravityQuotaRelevantBuckets(hint, model)
	for _, bucket := range relevant {
		if bucket.RemainingFraction > 0 {
			continue
		}
		if bucket.ResetTime.IsZero() || bucket.ResetTime.After(now) {
			return false
		}
	}
	return true
}

func antigravityQuotaRelevantBuckets(hint AntigravityQuotaHint, model string) []AntigravityQuotaBucket {
	model = strings.ToLower(strings.TrimSpace(model))
	thirdParty := strings.Contains(model, "claude") || strings.Contains(model, "gpt")
	buckets := make([]AntigravityQuotaBucket, 0, 4)
	for _, group := range hint.Groups {
		groupName := strings.ToLower(group.DisplayName + " " + group.Description)
		for _, bucket := range group.Buckets {
			bucketID := strings.ToLower(strings.TrimSpace(bucket.BucketID))
			isThirdPartyBucket := strings.HasPrefix(bucketID, "3p-") || strings.Contains(groupName, "claude") || strings.Contains(groupName, "gpt")
			isGeminiBucket := strings.HasPrefix(bucketID, "gemini-") || strings.Contains(groupName, "gemini")
			if (thirdParty && isThirdPartyBucket) || (!thirdParty && isGeminiBucket) {
				buckets = append(buckets, bucket)
			}
		}
	}
	return buckets
}
