package auth

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAntigravityQuotaAllowsModel(t *testing.T) {
	auth := &Auth{ID: "quota-auth", Provider: "antigravity"}
	now := time.Now()
	SetAntigravityQuotaHint(auth.ID, AntigravityQuotaHint{
		Known:     true,
		UpdatedAt: now,
		Groups: []AntigravityQuotaGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []AntigravityQuotaBucket{
					{BucketID: "gemini-weekly", RemainingFraction: 0, ResetTime: now.Add(time.Hour)},
					{BucketID: "gemini-5h", RemainingFraction: 0.5, ResetTime: now.Add(time.Minute)},
				},
			},
			{
				DisplayName: "Claude and GPT models",
				Buckets: []AntigravityQuotaBucket{
					{BucketID: "3p-weekly", RemainingFraction: 0.9, ResetTime: now.Add(time.Hour)},
					{BucketID: "3p-5h", RemainingFraction: 0.9, ResetTime: now.Add(time.Minute)},
				},
			},
		},
	})

	if AntigravityQuotaAllowsModel(auth, "gemini-3.1-pro", now) {
		t.Fatal("exhausted Gemini weekly bucket should block the Gemini family")
	}
	if !AntigravityQuotaAllowsModel(auth, "claude-sonnet-4-6", now) {
		t.Fatal("Claude bucket has remaining quota")
	}
	if !AntigravityQuotaAllowsModel(auth, "gpt-oss-120b", now) {
		t.Fatal("GPT bucket has remaining quota")
	}
}

func TestSchedulerSkipsAntigravityQuotaExhaustedModel(t *testing.T) {
	const model = "gemini-3.1-pro"
	authIDs := []string{"ag-quota-blocked", "ag-quota-ready"}
	for _, authID := range authIDs {
		registry.GetGlobalRegistry().RegisterClient(authID, "antigravity", []*registry.ModelInfo{{ID: model}})
	}
	t.Cleanup(func() {
		for _, authID := range authIDs {
			registry.GetGlobalRegistry().UnregisterClient(authID)
		}
	})

	now := time.Now()
	SetAntigravityQuotaHint("ag-quota-blocked", AntigravityQuotaHint{
		Known:     true,
		UpdatedAt: now,
		Groups: []AntigravityQuotaGroup{{
			DisplayName: "Gemini Models",
			Buckets: []AntigravityQuotaBucket{
				{BucketID: "gemini-5h", RemainingFraction: 0, ResetTime: now.Add(time.Hour)},
			},
		}},
	})
	SetAntigravityQuotaHint("ag-quota-ready", AntigravityQuotaHint{
		Known:     true,
		UpdatedAt: now,
		Groups: []AntigravityQuotaGroup{{
			DisplayName: "Gemini Models",
			Buckets: []AntigravityQuotaBucket{
				{BucketID: "gemini-5h", RemainingFraction: 0.5, ResetTime: now.Add(time.Hour)},
			},
		}},
	})

	scheduler := newSchedulerForTest(
		&RoundRobinSelector{},
		&Auth{ID: "ag-quota-blocked", Provider: "antigravity"},
		&Auth{ID: "ag-quota-ready", Provider: "antigravity"},
	)
	for i := 0; i < 3; i++ {
		picked, errPick := scheduler.pickSingle(context.Background(), "antigravity", model, cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickSingle() error = %v", errPick)
		}
		if picked == nil || picked.ID != "ag-quota-ready" {
			t.Fatalf("pickSingle() = %#v, want ag-quota-ready", picked)
		}
	}
}

func TestAntigravityQuotaHintExpiryAndResetAreOptimistic(t *testing.T) {
	auth := &Auth{ID: "quota-expired", Provider: "antigravity"}
	now := time.Now()
	SetAntigravityQuotaHint(auth.ID, AntigravityQuotaHint{
		Known:     true,
		UpdatedAt: now.Add(-AntigravityQuotaHintRefreshInterval - time.Second),
		Groups: []AntigravityQuotaGroup{{
			DisplayName: "Gemini Models",
			Buckets: []AntigravityQuotaBucket{
				{BucketID: "gemini-weekly", RemainingFraction: 0, ResetTime: now.Add(time.Hour)},
			},
		}},
	})
	if !AntigravityQuotaAllowsModel(auth, "gemini-3.1-pro", now) {
		t.Fatal("stale quota hints must not block scheduling")
	}

	SetAntigravityQuotaHint(auth.ID, AntigravityQuotaHint{
		Known:     true,
		UpdatedAt: now,
		Groups: []AntigravityQuotaGroup{{
			DisplayName: "Gemini Models",
			Buckets: []AntigravityQuotaBucket{
				{BucketID: "gemini-weekly", RemainingFraction: 0, ResetTime: now.Add(-time.Minute)},
			},
		}},
	})
	if !AntigravityQuotaAllowsModel(auth, "gemini-3.1-pro", now) {
		t.Fatal("expired reset time should allow the model again")
	}
}
