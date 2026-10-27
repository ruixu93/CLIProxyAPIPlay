package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityQuotaSummaryPath           = "/v1internal:retrieveUserQuotaSummary"
	antigravityQuotaProject               = "aicode-consumers"
	antigravityQuotaRequestLimit          = 1 << 20
	antigravityQuotaForceRefreshMinPeriod = 30 * time.Second
)

type antigravityQuotaRefreshState struct {
	mu          sync.Mutex
	lastAttempt time.Time
	inFlight    bool
}

var (
	antigravityQuotaRefreshByID sync.Map // auth.ID -> *antigravityQuotaRefreshState
	antigravityQuotaRefreshTTL  = 5 * time.Second
)

type antigravityQuotaSummaryResponse struct {
	Groups []struct {
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Buckets     []struct {
			BucketID          string  `json:"bucketId"`
			DisplayName       string  `json:"displayName"`
			Window            string  `json:"window"`
			ResetTime         string  `json:"resetTime"`
			Description       string  `json:"description"`
			RemainingFraction float64 `json:"remainingFraction"`
		} `json:"buckets"`
	} `json:"groups"`
}

// maybeRefreshAntigravityQuotaHint schedules a quota-summary query when the
// cached hint is stale. It never blocks the inference request that triggered it.
func (e *AntigravityExecutor) maybeRefreshAntigravityQuotaHint(ctx context.Context, auth *cliproxyauth.Auth, accessToken string, force bool) {
	if e == nil || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	if !force {
		if hint, ok := cliproxyauth.GetAntigravityQuotaHint(authID); ok && cliproxyauth.IsAntigravityQuotaHintFresh(hint, time.Now()) {
			return
		}
	}
	token := strings.TrimSpace(accessToken)
	if token == "" {
		token = metaStringValue(auth.Metadata, "access_token")
	}
	if token == "" {
		return
	}

	state := &antigravityQuotaRefreshState{}
	if existing, loaded := antigravityQuotaRefreshByID.LoadOrStore(authID, state); loaded {
		if cast, ok := existing.(*antigravityQuotaRefreshState); ok && cast != nil {
			state = cast
		}
	}
	state.mu.Lock()
	minInterval := cliproxyauth.AntigravityQuotaHintRefreshInterval
	if force {
		minInterval = antigravityQuotaForceRefreshMinPeriod
	}
	if state.inFlight || (!state.lastAttempt.IsZero() && time.Since(state.lastAttempt) < minInterval) {
		state.mu.Unlock()
		return
	}
	state.inFlight = true
	state.lastAttempt = time.Now()
	state.mu.Unlock()

	authCopy := auth.Clone()
	refreshCtx := context.Background()
	if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			refreshCtx = context.WithValue(refreshCtx, "cliproxy.roundtripper", rt)
		}
	}
	refreshCtx, cancelRefresh := context.WithTimeout(refreshCtx, antigravityQuotaRefreshTTL)
	go func() {
		defer cancelRefresh()
		defer func() {
			state.mu.Lock()
			state.inFlight = false
			state.mu.Unlock()
		}()
		if errFetch := e.fetchAntigravityQuotaSummary(refreshCtx, authCopy, token); errFetch != nil {
			log.Debugf("antigravity executor: quota summary refresh failed: %v", errFetch)
		}
	}()
}

func (e *AntigravityExecutor) fetchAntigravityQuotaSummary(ctx context.Context, auth *cliproxyauth.Auth, accessToken string) error {
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return nil
	}
	endpointURL := strings.TrimSuffix(antigravityLoadCodeAssistBaseURL(auth), "/") + antigravityQuotaSummaryPath
	body, errMarshal := json.Marshal(map[string]string{"project": antigravityQuotaProject})
	if errMarshal != nil {
		return errMarshal
	}
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if errReq != nil {
		return errReq
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
	if host := resolveHost(antigravityLoadCodeAssistBaseURL(auth)); host != "" {
		httpReq.Host = host
	}

	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, errDo := helps.WithAntigravityHTTPClientTrace(httpClient, auth, "quota_query").Do(httpReq)
	if errDo != nil {
		return errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity executor: close quota response body error: %v", errClose)
		}
	}()

	data, errRead := io.ReadAll(io.LimitReader(httpResp.Body, antigravityQuotaRequestLimit+1))
	if errRead != nil {
		return errRead
	}
	if len(data) > antigravityQuotaRequestLimit {
		return fmt.Errorf("quota summary response exceeds %d bytes", antigravityQuotaRequestLimit)
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("quota summary returned status %d: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
	}

	var parsed antigravityQuotaSummaryResponse
	if errUnmarshal := json.Unmarshal(data, &parsed); errUnmarshal != nil {
		return errUnmarshal
	}
	hint := cliproxyauth.AntigravityQuotaHint{
		Known:     true,
		Groups:    make([]cliproxyauth.AntigravityQuotaGroup, 0, len(parsed.Groups)),
		UpdatedAt: time.Now(),
	}
	for _, group := range parsed.Groups {
		mapped := cliproxyauth.AntigravityQuotaGroup{
			DisplayName: group.DisplayName,
			Description: group.Description,
			Buckets:     make([]cliproxyauth.AntigravityQuotaBucket, 0, len(group.Buckets)),
		}
		for _, bucket := range group.Buckets {
			mapped.Buckets = append(mapped.Buckets, cliproxyauth.AntigravityQuotaBucket{
				BucketID:          bucket.BucketID,
				Window:            bucket.Window,
				ResetTime:         parseAntigravityQuotaResetTime(bucket.ResetTime),
				RemainingFraction: bucket.RemainingFraction,
			})
		}
		hint.Groups = append(hint.Groups, mapped)
	}
	cliproxyauth.SetAntigravityQuotaHint(authID, hint)
	return nil
}

func parseAntigravityQuotaResetTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if parsed, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
		return parsed
	}
	return time.Time{}
}

func resetAntigravityQuotaRefreshState() {
	antigravityQuotaRefreshByID = sync.Map{}
}
