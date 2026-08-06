package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/store/storetest"
)

// The local-agent routes are the console's only way to manage the servers the
// desktop's built-in agent additionally reports to (AGENT-007 phase 3). They
// exist only in desktop mode, because only the desktop has an embedded agent to
// reconfigure — and the console learns that from server-info rather than by
// probing.

// fakeLocalAgent is a minimal in-memory implementation of the seam.
type fakeLocalAgent struct {
	added []api.LocalAgentServerSpec
}

func (f *fakeLocalAgent) List(context.Context) ([]api.LocalAgentServer, error) {
	out := make([]api.LocalAgentServer, 0, len(f.added))
	for _, s := range f.added {
		entry := api.LocalAgentServer{Name: s.Name, URL: s.URL,
			Status: api.LocalAgentServerStatus{State: "connecting"}}
		if s.Permissions != nil {
			entry.Permissions = *s.Permissions
		}
		out = append(out, entry)
	}
	return out, nil
}

func (f *fakeLocalAgent) Add(_ context.Context, spec api.LocalAgentServerSpec) (api.LocalAgentServer, error) {
	f.added = append(f.added, spec)
	out := api.LocalAgentServer{Name: spec.Name, URL: spec.URL}
	if spec.Permissions != nil {
		out.Permissions = *spec.Permissions
	}
	return out, nil
}

func (f *fakeLocalAgent) Remove(context.Context, string) error { return api.ErrLocalAgentNotFound }

func (f *fakeLocalAgent) SetPermissions(context.Context, string, *[]string) error {
	return api.ErrLocalAgentNotFound
}

// startForLocalAgent brings up a server with or without the desktop seam and
// returns an authenticated client plus its base URL.
func startForLocalAgent(t *testing.T, local api.LocalAgentAPI) (*http.Client, string) {
	t.Helper()
	ctx := context.Background()
	cfg := Config{
		Addr:      "127.0.0.1:0",
		DBPath:    filepath.Join(storetest.Dir(t), "localagent.db"),
		AdminUser: "admin",
		AdminPass: "test-password",
		MaxAgents: 5,
	}
	if local != nil {
		cfg.Desktop = &DesktopConfig{LocalAgent: local}
	}
	srv, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	base := srv.BaseURL()
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"test-password"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	return client, base
}

func TestLocalAgentRoutesAreDesktopOnly(t *testing.T) {
	t.Run("absent on a self-hosted server", func(t *testing.T) {
		client, base := startForLocalAgent(t, nil)

		resp, err := client.Get(base + "/api/v1/local-agent/servers")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 — a server with no embedded agent has no such feature", resp.StatusCode)
		}

		if info := serverInfo(t, client, base); info["local_agent"] != nil {
			t.Fatalf("server-info advertised local_agent = %v on a self-hosted server", info["local_agent"])
		}
	})

	t.Run("present in desktop mode", func(t *testing.T) {
		fake := &fakeLocalAgent{}
		client, base := startForLocalAgent(t, fake)

		if info := serverInfo(t, client, base); info["local_agent"] != true {
			t.Fatalf("server-info local_agent = %v, want true so the console knows to show the panel", info["local_agent"])
		}

		resp, err := client.Post(base+"/api/v1/local-agent/servers", "application/json",
			strings.NewReader(`{"url":"https://work.example","enroll_token":"tok","permissions":["probe.icmp"]}`))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST status = %d: %s", resp.StatusCode, body)
		}
		if strings.Contains(string(body), "tok") {
			t.Fatalf("the enrollment token was echoed back: %s", body)
		}
		if len(fake.added) != 1 {
			t.Fatalf("the seam saw %d adds", len(fake.added))
		}
		if got := fake.added[0]; got.URL != "https://work.example" || got.EnrollToken != "tok" {
			t.Fatalf("spec reached the seam as %+v", got)
		}
		if fake.added[0].Name == "" {
			t.Fatal("no name was derived from the URL host")
		}

		resp, err = client.Get(base + "/api/v1/local-agent/servers")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status = %d: %s", resp.StatusCode, body)
		}
		if !strings.Contains(string(body), `"servers"`) {
			t.Fatalf("list response is not the documented envelope: %s", body)
		}
	})
}

func serverInfo(t *testing.T, client *http.Client, base string) map[string]any {
	t.Helper()
	resp, err := client.Get(base + "/api/v1/server-info")
	if err != nil {
		t.Fatalf("server-info: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode server-info: %v", err)
	}
	return out
}
