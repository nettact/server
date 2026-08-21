package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nettact/protocol"
	"github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/permission"
	"github.com/nettact/server-core/store/storetest"
)

// TestHTTPEnrollSchema7AndSchema8ResponseShape drives the assembled server's
// real HTTP enrollment endpoint and checks the response is encoded in the
// requesting schema's shape: the schema 8 response carries the
// enrollment_epoch key, the schema 7 response drops it (a pre-boundary peer
// never learned a credential generation and must not be handed one). This is
// the one place the encoding difference is observable at the server assembly
// layer — the in-process EnrollAgent path returns the canonical struct and
// never reaches the response encoder.
func TestHTTPEnrollSchema7AndSchema8ResponseShape(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "enroll-shape.db")
	srv := startDesktopTestServer(t, dbPath, time.Minute)
	ctx := context.Background()

	post := func(schema int) (int, string) {
		t.Helper()
		token, err := srv.MintEnrollmentToken(ctx, "shape")
		if err != nil {
			t.Fatalf("MintEnrollmentToken: %v", err)
		}
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("keygen: %v", err)
		}
		const nonce = "shape-nonce"
		req := enroll.EnrollRequest{
			SchemaVersion:   schema,
			EnrollmentToken: token,
			PublicKey:       pub,
			Nonce:           nonce,
			Signature:       ed25519.Sign(priv, []byte(nonce)),
			Hostname:        "shape-host",
			Platform:        "linux",
			AgentVersion:    "test",
			Permissions:     permission.PermissionReport{},
		}
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal enroll request: %v", err)
		}
		resp, err := http.Post(srv.BaseURL()+"/api/v1/enroll", "application/json", bytes.NewReader(body)) //nolint:gosec // loopback test server
		if err != nil {
			t.Fatalf("POST /api/v1/enroll: %v", err)
		}
		defer resp.Body.Close()
		buf := new(strings.Builder)
		if _, err := io.Copy(buf, resp.Body); err != nil {
			t.Fatalf("read response body: %v", err)
		}
		return resp.StatusCode, buf.String()
	}

	// Compare the decoded key SETS, not substrings: a missing key and an
	// unexpected extra key are both failures, and only the key set states that.
	keysOf := func(body string) map[string]bool {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("decode enroll response %q: %v", body, err)
		}
		keys := make(map[string]bool, len(m))
		for k := range m {
			keys[k] = true
		}
		return keys
	}
	want := func(keys map[string]bool, exact []string, body string) {
		t.Helper()
		if len(keys) != len(exact) {
			t.Errorf("response key set = %v, want exactly %v; body %s", keys, exact, body)
			return
		}
		for _, k := range exact {
			if !keys[k] {
				t.Errorf("response is missing the %s key: %s", k, body)
			}
		}
	}

	preBoundary := []string{"agent_id", "site_id", "agent_token", "server_time", "config_version"}

	code, body7 := post(7)
	if code != http.StatusOK {
		t.Fatalf("schema 7 enroll status = %d, want 200; body %s", code, body7)
	}
	keys7 := keysOf(body7)
	if keys7["enrollment_epoch"] {
		t.Errorf("schema 7 response carries the enrollment_epoch key: %s", body7)
	}
	want(keys7, preBoundary, body7)

	code, body8 := post(protocol.SchemaVersion)
	if code != http.StatusOK {
		t.Fatalf("schema 8 enroll status = %d, want 200; body %s", code, body8)
	}
	keys8 := keysOf(body8)
	if !keys8["enrollment_epoch"] {
		t.Errorf("schema 8 response missing the enrollment_epoch key: %s", body8)
	}
	want(keys8, append(append([]string{}, preBoundary...), "enrollment_epoch"), body8)
}
