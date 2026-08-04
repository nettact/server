package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/store/storetest"
)

// TestRollupRunsAtStartup pins the startup half of the rollup worker's schedule.
//
// The steady-state cadence is five minutes, which is long enough to matter: the
// desktop tray is routinely opened and closed inside it, so on a plain ticker a
// short session would never roll up at all and every session would begin with
// minutes of raw samples missing from the tiers that serve charts wider than 2h.
// Pre-seeded raw samples must therefore be downsampled shortly after Start, not
// one tick later.
func TestRollupRunsAtStartup(t *testing.T) {
	dbPath := filepath.Join(storetest.Dir(t), "rollup-startup.db")

	// Seed raw samples an hour back — comfortably inside raw retention and below
	// the current minute boundary, so the first rollup pass has real work to do.
	seed := time.Now().UTC().Add(-time.Hour).Unix() / 60 * 60
	func() {
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("open seed db: %v", err)
		}
		defer db.Close()
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO series(id, agent_id, site_id, monitor_id, kind, target, unit, config_serial)
			VALUES(1,'agent_a','site_default','mon_a','icmp_rtt_ms','192.168.1.1','ms',1)`); err != nil {
			t.Fatalf("seed series: %v", err)
		}
		for i := int64(0); i < 10; i++ {
			if _, err := db.ExecContext(context.Background(),
				`INSERT INTO samples(series_id, ts, value) VALUES(1,?,?)`, seed+i, float64(i)); err != nil {
				t.Fatalf("seed sample %d: %v", i, err)
			}
		}
	}()

	startDesktopTestServer(t, dbPath, time.Minute)

	// The startup pass runs on the workers goroutine, so poll rather than assume
	// it has already landed. Anything under the five-minute tick proves the point.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := srvDB(t, dbPath, &n); err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no rollup_1m rows after startup: the rollup worker waits for its first tick, " +
				"so a session shorter than the interval never downsamples")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// srvDB counts rollup_1m rows through a separate read-only handle, so the check
// never disturbs the running server's own connections.
func srvDB(t *testing.T, dbPath string, n *int) error {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Read().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM rollup_1m`).Scan(n)
}
