package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestFetchAntigravityQuotaSummary(t *testing.T) {
	resetAntigravityQuotaRefreshState()
	t.Cleanup(resetAntigravityQuotaRefreshState)
	t.Cleanup(func() { cliproxyauth.SetAntigravityQuotaHint("quota-auth", cliproxyauth.AntigravityQuotaHint{}) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityQuotaSummaryPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.ProtoMajor != 1 {
			t.Errorf("proto = %s, want HTTP/1.1", r.Proto)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "antigravity/hub/") {
			t.Errorf("User-Agent = %q", got)
		}
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read body: %v", errRead)
		}
		if strings.TrimSpace(string(body)) != `{"project":"aicode-consumers"}` {
			t.Errorf("body = %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","resetTime":"2026-10-27T12:00:00Z","remainingFraction":0.25}]}]}`))
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{
		ID:         "quota-auth",
		Provider:   "antigravity",
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"project_id": "project-a"},
	}
	executor := &AntigravityExecutor{}
	if errFetch := executor.fetchAntigravityQuotaSummary(context.Background(), auth, "access-token"); errFetch != nil {
		t.Fatalf("fetchAntigravityQuotaSummary() error = %v", errFetch)
	}

	hint, ok := cliproxyauth.GetAntigravityQuotaHint(auth.ID)
	if !ok || !hint.Known {
		t.Fatalf("quota hint = %#v, ok=%v", hint, ok)
	}
	if len(hint.Groups) != 1 || len(hint.Groups[0].Buckets) != 1 {
		t.Fatalf("quota groups = %#v", hint.Groups)
	}
	bucket := hint.Groups[0].Buckets[0]
	if bucket.BucketID != "gemini-5h" || bucket.RemainingFraction != 0.25 {
		t.Fatalf("bucket = %#v", bucket)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-10-27T12:00:00Z")
	if !bucket.ResetTime.Equal(wantReset) {
		t.Fatalf("reset time = %v, want %v", bucket.ResetTime, wantReset)
	}
}
