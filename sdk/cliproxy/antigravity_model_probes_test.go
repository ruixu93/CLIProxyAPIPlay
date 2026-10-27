package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestAntigravityAccountChangesCancelConnectedProbe(t *testing.T) {
	for _, change := range []string{"disabled", "route", "token", "config-proxy", "native-refresh", "native-prepare", "native-update", "native-remove"} {
		t.Run(change, func(t *testing.T) {
			resetAntigravityCapabilityCache()
			t.Cleanup(resetAntigravityCapabilityCache)
			started, cancelled := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(cancelled)
			}))
			ctx, cancel := context.WithCancel(t.Context())
			svc := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
			t.Cleanup(func() { cancel(); svc.WaitAntigravityProbes(); server.Close() })
			auth, _ := svc.coreManager.Register(ctx, antigravityTestAuth("cancel-"+change, server.URL))
			t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
			svc.registerModelsForAuth(ctx, auth)
			<-started
			updated := auth.Clone()
			switch change {
			case "disabled":
				updated.Disabled = true
			case "route":
				updated.Attributes["base_url"] = "https://new-route.invalid"
			case "token", "native-refresh", "native-prepare", "native-update":
				updated.Metadata["access_token"] = "new-token"
			}
			if change == "config-proxy" {
				next := &config.Config{}
				next.ProxyURL = "direct"
				svc.commitConfigUpdate(next)
			} else {
				switch change {
				case "native-refresh":
					_, _ = svc.coreManager.UpdateRefreshedAuth(ctx, auth, updated)
				case "native-prepare":
					_, _ = svc.coreManager.UpdatePreparedAuth(ctx, auth, updated)
				case "native-update":
					_, _ = svc.coreManager.Update(ctx, updated)
				case "native-remove":
					svc.coreManager.Remove(ctx, auth.ID)
				default:
					svc.prepareCoreAuthForModelRegistration(ctx, updated)
				}
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("obsolete connected probe was not canceled")
			}
			svc.WaitAntigravityProbes()
			antigravityCapabilityMu.RLock()
			failures := len(antigravityAuthFailureCache)
			antigravityCapabilityMu.RUnlock()
			if failures != 0 {
				t.Fatal("account invalidation counted as upstream failure")
			}
			svc.antigravityProbeMu.Lock()
			active := len(svc.antigravityAccountProbes)
			svc.antigravityProbeMu.Unlock()
			if active != 0 {
				t.Fatal("completed account probes retained")
			}
		})
	}
}

func TestAntigravityModelUserAgentPerAuth(t *testing.T) {
	previous := antigravityModelUserAgent
	antigravityModelUserAgent = func() string { return "antigravity/hub/2.26.0 (aidev_client; os_type=linux; arch=amd64)" }
	t.Cleanup(func() { antigravityModelUserAgent = previous })

	macAuth := &coreauth.Auth{Metadata: map[string]any{"ua_platform": "macos"}}
	if got := antigravityModelUserAgentForAuth(macAuth); !strings.Contains(got, "os_type=darwin; arch=arm64") {
		t.Fatalf("macOS model UA = %q", got)
	}

	const explicit = "antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682)"
	explicitAuth := &coreauth.Auth{Metadata: map[string]any{"user_agent": explicit, "ua_platform": "linux"}}
	if got := antigravityModelUserAgentForAuth(explicitAuth); got != explicit {
		t.Fatalf("explicit model UA = %q, want %q", got, explicit)
	}

	if got := antigravityModelUserAgentForAuth(&coreauth.Auth{}); got != antigravityModelUserAgent() {
		t.Fatalf("default model UA = %q, want %q", got, antigravityModelUserAgent())
	}
}

func TestAntigravityModelHTTPClientUsesHTTP11WithoutALPN(t *testing.T) {
	client, closeIdle, err := antigravityModelHTTPClient("")
	if err != nil {
		t.Fatalf("antigravityModelHTTPClient() error = %v", err)
	}
	defer closeIdle()

	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.ForceAttemptHTTP2 {
		t.Fatal("fetchAvailableModels transport must not enable HTTP/2")
	}
	if transport.TLSNextProto == nil {
		t.Fatal("fetchAvailableModels transport must disable implicit h2 upgrade")
	}
	if transport.TLSClientConfig == nil || len(transport.TLSClientConfig.NextProtos) != 0 {
		t.Fatalf("fetchAvailableModels TLS ALPN = %v, want none", transport.TLSClientConfig)
	}
}

func TestAntigravityModelProbeUsesHTTP11(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != antigravityModelsPath {
			t.Errorf("path = %q, want %q", r.URL.Path, antigravityModelsPath)
		}
		if r.ProtoMajor != 1 {
			t.Errorf("proto = %s, want HTTP/1.1", r.Proto)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-3-pro":{}}}`))
	}))
	defer server.Close()

	svc := &Service{}
	hints, status := svc.probeAntigravityModelCapabilityHints(t.Context(), &coreauth.Auth{
		Metadata: map[string]any{"project_id": "project-a"},
	}, []string{server.URL}, "", "access-token", "test-agent")
	if status != antigravityProbeStatusSuccess {
		t.Fatalf("probe status = %v, want success", status)
	}
	if _, ok := hints.ModelIDs["gemini-3-pro"]; !ok {
		t.Fatalf("model hints = %v, want gemini-3-pro", hints.ModelIDs)
	}
}

func TestAntigravityDeletedAccountCannotStartQueuedProbe(t *testing.T) {
	svc := &Service{cfg: &config.Config{}, coreManager: coreauth.NewManager(nil, nil, nil)}
	auth, _ := svc.coreManager.Register(t.Context(), antigravityTestAuth("queued-removed", "http://127.0.0.1:1"))
	key := svc.antigravityCapabilityKey(auth)
	svc.applyCoreAuthRemoval(t.Context(), auth.ID)
	ctx, finish, active := svc.beginAntigravityAccountProbe(t.Context(), auth, key, "flight", "token-queued-removed")
	defer finish()
	if active || ctx.Err() == nil {
		t.Fatal("removed account started a late queued probe")
	}
}
