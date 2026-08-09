package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/store/storetest"
	"github.com/nettact/server-core/tsstore"
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
	dir := storetest.Dir(t)
	dbPath := filepath.Join(dir, "rollup-startup.db")

	// Seed raw samples an hour back — comfortably inside raw retention and below
	// the current minute boundary, so the first rollup pass has real work to do.
	// The dataset UUID is minted HERE so the seed tsstore and the server open
	// the same-identity tsdb directory (Start reuses an existing uuid).
	const seedUUID = "rollup-startup-test"
	seed := time.Now().UTC().Add(-time.Hour).Unix() / 60 * 60
	func() {
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("open seed db: %v", err)
		}
		defer db.Close()
		ctx := context.Background()
		if _, err := db.ExecContext(ctx,
			`INSERT INTO app_settings(key, value) VALUES('dataset_uuid', ?)`, seedUUID); err != nil {
			t.Fatalf("seed dataset uuid: %v", err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO series(id, agent_id, site_id, monitor_id, kind, target, unit, config_serial)
			VALUES(1,'agent_a','site_default','mon_a','icmp_rtt_ms','192.168.1.1','ms',1)`); err != nil {
			t.Fatalf("seed series: %v", err)
		}
		tss, err := tsstore.Open(filepath.Join(dir, "tsdb"), tsstore.Config{}, seedUUID)
		if err != nil {
			t.Fatalf("open seed tsdb: %v", err)
		}
		defer tss.Close()
		var batch []tsstore.RawSample
		for i := int64(0); i < 10; i++ {
			batch = append(batch, tsstore.RawSample{SID: 1, TS: seed + i, Value: float64(i)})
		}
		if res, err := tss.AppendRaw(ctx, batch); err != nil || res.Appended != 10 {
			t.Fatalf("seed samples: res=%+v err=%v", res, err)
		}
	}()

	startDesktopTestServer(t, dbPath, time.Minute)

	// The startup pass runs on the workers goroutine, so poll rather than assume
	// it has already landed. Anything under the five-minute tick proves the point.
	// The observable is the 1m watermark in rollup_state (still SQLite): the pass
	// advances it exactly when it aggregated something.
	deadline := time.Now().Add(15 * time.Second)
	for {
		var n int
		if err := srvDB(t, dbPath, &n); err == nil && n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no 1m rollup watermark after startup: the rollup worker waits for its first tick, " +
				"so a session shorter than the interval never downsamples")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// srvDB counts advanced 1m watermarks through a separate read-only handle, so
// the check never disturbs the running server's own connections.
func srvDB(t *testing.T, dbPath string, n *int) error {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Read().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM rollup_state WHERE resolution='1m' AND last_ts > 0`).Scan(n)
}
