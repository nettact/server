package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/protocol"
	protoenroll "github.com/nettact/protocol/enroll"
	"github.com/nettact/protocol/permission"
	"github.com/nettact/protocol/telemetry"
	"github.com/nettact/protocol/wire"
	"github.com/nettact/server-core/store/storetest"
)

// TestInProcessEnrollAndDial proves the desktop in-memory path end to end at the
// server boundary: mint a token, EnrollAgent directly (no HTTP), then
// DialAgent (no WebSocket) and run the full Hello→DesiredState→Packet→Ack flow.
func TestInProcessEnrollAndDial(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "inproc.db")
	srv := startDesktopTestServer(t, dbPath, time.Minute)
	ctx := context.Background()

	token, err := srv.MintEnrollmentToken(ctx, "test agent")
	if err != nil {
		t.Fatalf("MintEnrollmentToken: %v", err)
	}

	// Build a signed enrollment request just as the agent does.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	nonceBytes := make([]byte, 32)
	_, _ = rand.Read(nonceBytes)
	nonce := base64.StdEncoding.EncodeToString(nonceBytes)
	req := protoenroll.EnrollRequest{
		SchemaVersion:   protocol.SchemaVersion,
		EnrollmentToken: token,
		PublicKey:       pub,
		Nonce:           nonce,
		Signature:       ed25519.Sign(priv, []byte(nonce)),
		Hostname:        "desktop-host",
		Platform:        "windows",
		AgentVersion:    "test",
		Permissions: permission.PermissionReport{
			Supported: []string{string(permission.ProbeTCP)},
			Granted:   []string{string(permission.ProbeTCP)},
			Effective: []string{string(permission.ProbeTCP)},
			Source:    string(permission.SourceEnvironment),
		},
	}

	resp, err := srv.EnrollAgent(ctx, req)
	if err != nil {
		t.Fatalf("EnrollAgent: %v", err)
	}
	if resp.AgentID == "" || resp.AgentToken == "" {
		t.Fatalf("EnrollAgent returned empty identity: %+v", resp)
	}

	// Dial the in-process link with the freshly minted bearer token.
	c, err := srv.DialAgent(ctx, resp.AgentToken)
	if err != nil {
		t.Fatalf("DialAgent: %v", err)
	}
	defer c.Close(wire.CloseNormalClosure, "done")

	hello := wire.Frame{Hello: &wire.Hello{
		SchemaVersion: protocol.SchemaVersion,
		Hostname:      "desktop-host",
		Platform:      "windows",
		AgentVersion:  "test",
		Permissions: permission.PermissionReport{
			Supported: []string{string(permission.ProbeTCP)},
			Granted:   []string{string(permission.ProbeTCP)},
			Effective: []string{string(permission.ProbeTCP)},
			Source:    string(permission.SourceEnvironment),
		},
	}}
	if err := c.WriteFrame(ctx, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	f, err := c.ReadFrame(rctx)
	if err != nil {
		t.Fatalf("read desired state: %v", err)
	}
	if f.DesiredState == nil {
		t.Fatalf("first push = %+v, want DesiredState", f)
	}

	// Send a telemetry packet and expect an ack over the pipe.
	now := time.Now().UTC().Truncate(time.Second)
	pkt := wire.Frame{Packet: &telemetry.Packet{
		SchemaVersion: protocol.SchemaVersion,
		AgentID:       resp.AgentID,
		SiteID:        resp.SiteID,
		Sequence:      1,
		SentAt:        now,
		Metrics: []telemetry.Metric{
			{TS: now, Kind: telemetry.ICMPRTTms, Target: "1.1.1.1", Value: 12.3, Unit: telemetry.UnitMs},
		},
	}}
	if err := c.WriteFrame(ctx, pkt); err != nil {
		t.Fatalf("write packet: %v", err)
	}
	f, err = c.ReadFrame(rctx)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if f.Ack == nil || f.Ack.HighestSequence != 1 {
		t.Fatalf("ack = %+v, want HighestSequence 1", f)
	}
}
