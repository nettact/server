package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nettact/server-core/store"
	"github.com/nettact/server-core/store/storetest"
	"github.com/nettact/server-core/tsstore"
)

// TestHalfRestoredDatasetRefusesToStart pins the guard that makes the backup
// documentation true. The manifest's UUID only catches a data plane from a
// DIFFERENT installation; the likelier accident is restoring this
// installation's SQLite file without its tsdb folder, where both halves would
// carry the same UUID and an absent directory looks exactly like a first run.
// Starting anyway would serve a dictionary whose history silently vanished.
func TestHalfRestoredDatasetRefusesToStart(t *testing.T) {
	dir := storetest.Dir(t)
	dbPath := filepath.Join(dir, "half.db")

	// A normal run: creates the dataset and records a series.
	srv := startDesktopTestServer(t, dbPath, time.Minute)
	if _, err := srv.db.ExecContext(context.Background(), `
		INSERT INTO series(agent_id, site_id, monitor_id, kind, target, config_serial)
		VALUES('agent_a','site_default','mon_a','icmp.rtt_ms','1.1.1.1',1)`); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	cancel()

	// The half restore: the database survives, the data plane does not.
	if err := os.RemoveAll(filepath.Join(dir, "tsdb")); err != nil {
		t.Fatalf("remove tsdb: %v", err)
	}
	_, err := Start(context.Background(), Config{
		Addr: "127.0.0.1:0", DBPath: dbPath,
		AdminUser: "admin", AdminPass: "test-password", MaxAgents: 5,
		Desktop: &DesktopConfig{LoginTokenTTL: time.Minute},
	})
	if err == nil {
		t.Fatal("Start accepted a database whose tsdb directory is missing; " +
			"the server would come up with its metric history silently gone")
	}
	if !strings.Contains(err.Error(), "one dataset") {
		t.Fatalf("Start error = %v, want the two-halves explanation", err)
	}
}

// TestRolledBackDatabaseRefusesToStart pins the other half-restore, the one
// the dataset UUID cannot catch: an OLDER database restored beside the CURRENT
// data plane. Both halves say the same installation, but the rolled-back
// sqlite_sequence would re-issue series ids the data plane still holds samples
// for, attaching a dead monitor's history to a new one.
func TestRolledBackDatabaseRefusesToStart(t *testing.T) {
	dir := storetest.Dir(t)
	dbPath := filepath.Join(dir, "rollback.db")

	srv := startDesktopTestServer(t, dbPath, time.Minute)
	ctx := context.Background()
	if _, err := srv.db.ExecContext(ctx, `
		INSERT INTO series(id, agent_id, site_id, monitor_id, kind, target, config_serial)
		VALUES(90,'agent_a','site_default','mon_a','icmp.rtt_ms','1.1.1.1',1)`); err != nil {
		t.Fatalf("seed series: %v", err)
	}
	// Data for series 90 raises the data plane's high-water mark.
	if _, err := srv.tss.AppendRaw(ctx, []tsstore.RawSample{{SID: 90, TS: time.Now().Unix() - 60, Value: 1}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	cancel()

	// The rollback: an older database whose newest series predates that mark.
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM series WHERE id=90`); err != nil {
		t.Fatalf("roll back series: %v", err)
	}
	db.Close()

	_, err = Start(ctx, Config{
		Addr: "127.0.0.1:0", DBPath: dbPath,
		AdminUser: "admin", AdminPass: "test-password", MaxAgents: 5,
		Desktop: &DesktopConfig{LoginTokenTTL: time.Minute},
	})
	if err == nil {
		t.Fatal("Start accepted a database rolled back behind the data plane; " +
			"new monitors would inherit dead series' history")
	}
	if !strings.Contains(err.Error(), "older backup") {
		t.Fatalf("Start error = %v, want the rolled-back-database explanation", err)
	}
}

// TestFreshInstallStartsWithoutATSDBDirectory is the other side of the guard:
// no series yet means no history to lose, so a first run must proceed and
// create the data plane rather than mistake itself for a half restore.
func TestFreshInstallStartsWithoutATSDBDirectory(t *testing.T) {
	dir := storetest.Dir(t)
	srv := startDesktopTestServer(t, filepath.Join(dir, "fresh.db"), time.Minute)
	if _, err := os.Stat(filepath.Join(dir, "tsdb", "manifest.json")); err != nil {
		t.Fatalf("fresh start did not create the data plane manifest: %v", err)
	}
	_ = srv
}
