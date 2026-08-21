package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nettact/protocol"
	"github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/permission"
	"github.com/nettact/protocol/telemetry"
	"github.com/nettact/protocol/wire"
	"github.com/nettact/server-core/store/storetest"
)

// TestWebSocketSessionServesBothSchemas drives the assembled server over a real
// WebSocket, with a real codec, for both wire schemas.
//
// The in-process tests dial through wire.Pipe, which hands the Frame struct
// straight across and serializes nothing. That makes them silent about the
// entire transport half: subprotocol negotiation, the codec chosen from it,
// and the routing that picks a schema's adapter for a socket rather than for a
// pipe. A regression in any of those would leave every in-process assertion
// green, so "both schemas are served" cannot honestly rest on them alone.
//
// What this asserts, per schema: the upgrade is accepted with the negotiated
// subprotocol, a Hello encoded by the real codec is admitted, the config push
// arrives decoded, and the schema's floor behaviour holds on the wire — the
// pre-boundary session is sent no sequence floor, the current one is.
func TestWebSocketSessionServesBothSchemas(t *testing.T) {
	for _, tc := range []struct {
		name      string
		schema    int
		caps      []string
		wantFloor bool
	}{
		{
			name:      "pre-boundary schema is served without a floor",
			schema:    7,
			caps:      nil,
			wantFloor: false,
		},
		{
			name:      "current schema is served the floor barrier",
			schema:    protocol.SchemaVersion,
			caps:      []string{wire.CapSequenceFloorV1},
			wantFloor: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(storetest.Dir(t), "ws-schema.db")
			srv := startDesktopTestServer(t, dbPath, time.Minute)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			agentToken, agentID, siteID := enrollOverHTTP(t, srv, tc.schema)

			// The subprotocol is what the codec is derived from, so choosing it
			// here is what makes this a transport test rather than a second
			// in-process one.
			const subprotocol = wire.SubprotocolJSON
			contentType := wire.SubprotocolContentType(subprotocol)

			wsURL := strings.Replace(srv.BaseURL(), "http://", "ws://", 1) + "/api/v1/agent/ws"
			c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
				HTTPHeader:   http.Header{"Authorization": {"Bearer " + agentToken}},
				Subprotocols: []string{subprotocol},
			})
			if err != nil {
				t.Fatalf("dial %s: %v", wsURL, err)
			}
			defer c.Close(websocket.StatusNormalClosure, "test done")
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if got := c.Subprotocol(); got != subprotocol {
				t.Fatalf("negotiated subprotocol = %q, want %q", got, subprotocol)
			}
			c.SetReadLimit(1 << 20)

			writeFrame := func(f wire.Frame) {
				t.Helper()
				data, err := wire.MarshalFrame(f, contentType)
				if err != nil {
					t.Fatalf("marshal frame: %v", err)
				}
				if err := c.Write(ctx, websocket.MessageText, data); err != nil {
					t.Fatalf("write frame: %v", err)
				}
			}
			readFrame := func() wire.Frame {
				t.Helper()
				_, data, err := c.Read(ctx)
				if err != nil {
					t.Fatalf("read frame: %v", err)
				}
				f, err := wire.UnmarshalFrame(data, contentType)
				if err != nil {
					t.Fatalf("decode frame: %v", err)
				}
				return f
			}

			hello := wire.Hello{SchemaVersion: tc.schema, Capabilities: tc.caps}
			if tc.wantFloor {
				// The current schema's peer states the credential generation it
				// holds; enrollment issued generation 1.
				hello.EnrollmentEpoch = 1
			}
			writeFrame(wire.Frame{Hello: &hello})

			// Every admitted session is pushed its config first, whatever schema
			// it speaks — that is the frame that proves the socket was admitted
			// and decoded, not merely accepted.
			if f := readFrame(); f.DesiredState == nil {
				t.Fatalf("first push = %+v, want DesiredState", f)
			}

			if !tc.wantFloor {
				// A pre-boundary session must be sent no floor: it runs no
				// barrier. Rather than probe for silence with a cancelled read —
				// which closes the socket client-side and would make a server
				// that hung up look identical to one behaving correctly — send
				// telemetry and read the next frame. The floor, if the server
				// wrongly pushed one, is pushed immediately after the config and
				// would arrive ahead of this Ack. So one read proves both halves:
				// no floor, and the legacy peer is still genuinely being served.
				now := time.Now().UTC()
				writeFrame(wire.Frame{Packet: &telemetry.Packet{
					SchemaVersion: tc.schema,
					AgentID:       agentID,
					SiteID:        siteID,
					Sequence:      1,
					SentAt:        now,
					Metrics: []telemetry.Metric{
						{TS: now, Kind: telemetry.ICMPRTTms, Target: "1.1.1.1", Value: 12.3, Unit: telemetry.UnitMs},
					},
				}})
				switch f := readFrame(); {
				case f.SequenceFloor != nil:
					t.Fatalf("a pre-boundary session was pushed a sequence floor; it negotiated no such frame")
				case f.Ack == nil:
					t.Fatalf("after a packet the pre-boundary session answered %+v, want an Ack", f)
				}
				return
			}

			if f := readFrame(); f.SequenceFloor == nil {
				t.Fatalf("second push = %+v, want SequenceFloor", f)
			}
		})
	}
}

// enrollOverHTTP enrolls one agent through the real HTTP endpoint in the given
// schema and returns its agent token and identity.
func enrollOverHTTP(t *testing.T, srv *Server, schema int) (token, agentID, siteID string) {
	t.Helper()
	ctx := context.Background()

	enrollToken, err := srv.MintEnrollmentToken(ctx, "ws-schema")
	if err != nil {
		t.Fatalf("MintEnrollmentToken: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	const nonce = "ws-schema-nonce"
	req := enroll.EnrollRequest{
		SchemaVersion:   schema,
		EnrollmentToken: enrollToken,
		PublicKey:       pub,
		Nonce:           nonce,
		Signature:       ed25519.Sign(priv, []byte(nonce)),
		Hostname:        "ws-host",
		Platform:        "linux",
		AgentVersion:    "test",
		Permissions:     permission.PermissionReport{},
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal enroll request: %v", err)
	}
	resp, err := http.Post(srv.BaseURL()+"/api/v1/enroll", "application/json", strings.NewReader(string(body))) //nolint:gosec // loopback test server
	if err != nil {
		t.Fatalf("POST /api/v1/enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		AgentToken string `json:"agent_token"`
		AgentID    string `json:"agent_id"`
		SiteID     string `json:"site_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode enroll response: %v", err)
	}
	if out.AgentToken == "" {
		t.Fatal("enroll response carried no agent_token")
	}
	return out.AgentToken, out.AgentID, out.SiteID
}
