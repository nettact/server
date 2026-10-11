package server

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nettact/server-core/api"
)

func writeASNFile(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestASNSourceReloadsOnlyWhenTheFileChanges pins the refresh contract: an
// untouched file is not reparsed, a replaced one is swapped in, and a broken
// replacement keeps the last good table instead of dropping attribution.
func TestASNSourceReloadsOnlyWhenTheFileChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ip2asn-v4.tsv")
	addr := netip.MustParseAddr("8.8.8.8")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	writeASNFile(t, path, "8.8.8.0\t8.8.8.255\t15169\tUS\tGOOGLE\n", t0)
	src := newASNSource(path, "")
	src.load(context.Background())
	if info, ok := src.holder.Lookup(addr); !ok || info.ASN != 15169 {
		t.Fatalf("initial lookup = %+v, %v; want AS15169", info, ok)
	}
	v1 := src.holder.Version()

	src.refresh(context.Background())
	if src.holder.Version() != v1 {
		t.Fatalf("unchanged file reloaded: version %q -> %q", v1, src.holder.Version())
	}

	writeASNFile(t, path, "8.8.8.0\t8.8.8.255\t64500\tUS\tEXAMPLE\n", t0.Add(time.Hour))
	src.refresh(context.Background())
	if info, ok := src.holder.Lookup(addr); !ok || info.ASN != 64500 {
		t.Fatalf("after replace lookup = %+v, %v; want AS64500", info, ok)
	}

	writeASNFile(t, path, "not\ta\tvalid\tline", t0.Add(2*time.Hour))
	src.refresh(context.Background())
	if info, ok := src.holder.Lookup(addr); !ok || info.ASN != 64500 {
		t.Fatalf("broken replacement dropped the table: %+v, %v", info, ok)
	}
}

// TestASNSourceWithoutFileDegrades: no configured or readable file is not an
// error — lookups simply miss.
func TestASNSourceWithoutFileDegrades(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.tsv")} {
		src := newASNSource(path, "")
		src.load(context.Background())
		if _, ok := src.holder.Lookup(netip.MustParseAddr("8.8.8.8")); ok {
			t.Fatalf("path %q: lookup hit without a table", path)
		}
	}
}

// TestASNSourceManagedRetriesAFailedCheck: a failed update check must not
// start the daily interval — the next hourly tick tries again — and an
// explicit file always wins over the managed table.
func TestASNSourceManagedRetriesAFailedCheck(t *testing.T) {
	t.Setenv("NETTACT_ASN_BASE_URL", "http://invalid.invalid/asn")
	src := newASNSource("", filepath.Join(t.TempDir(), "asn"))
	if src.installer == nil {
		t.Fatal("managed mode not selected")
	}
	src.load(context.Background())
	src.refresh(context.Background())
	if !src.lastCheck.IsZero() {
		t.Fatalf("failed check recorded as done at %v", src.lastCheck)
	}

	if f := newASNSource("/some/file.tsv", t.TempDir()); f.installer != nil {
		t.Fatal("an explicit file must disable the managed table")
	}
}

// TestASNSourceStatusAndManualCheck covers the console seam: file mode reports
// its path and table, a manual check re-reads even an unchanged file, a broken
// file surfaces as last_error while the previous table stays loaded, and off
// mode refuses a check.
func TestASNSourceStatusAndManualCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ip2asn-v4.tsv")
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	writeASNFile(t, path, "8.8.8.0\t8.8.8.255\t15169\tUS\tGOOGLE\n", t0)

	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	src := newASNSource(path, "")
	src.now = func() time.Time { return now }
	src.load(context.Background())

	st := src.ASNTableStatus()
	if st.Mode != api.ASNTableModeFile || st.Location != path || !st.Loaded || st.Ranges != 1 || !st.CanCheck {
		t.Fatalf("file status = %+v", st)
	}
	if st.NextCheckAt == nil || !st.NextCheckAt.Equal(now.Add(asnTickInterval)) {
		t.Fatalf("next check = %v, want %v", st.NextCheckAt, now.Add(asnTickInterval))
	}

	now = now.Add(10 * time.Minute)
	st, err := src.CheckASNTable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Checking || st.LoadedAt == nil || !st.LoadedAt.Equal(now) {
		t.Fatalf("manual check did not re-read the unchanged file: %+v", st)
	}

	writeASNFile(t, path, "garbage", t0.Add(time.Hour))
	st, _ = src.CheckASNTable(context.Background())
	if st.LastError == "" || st.LastErrorAt == nil || !st.Loaded {
		t.Fatalf("broken file: want last_error with table kept, got %+v", st)
	}

	off := newASNSource("", "")
	if st := off.ASNTableStatus(); st.Mode != api.ASNTableModeOff || st.CanCheck || st.Loaded {
		t.Fatalf("off status = %+v", st)
	}
	if _, err := off.CheckASNTable(context.Background()); !errors.Is(err, api.ErrASNTableCheckUnsupported) {
		t.Fatalf("off check err = %v", err)
	}
}

// TestASNSourceCheckWhileRefreshing: a manual check arriving during a refresh
// returns at once with the current status instead of queueing a second run.
func TestASNSourceCheckWhileRefreshing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ip2asn-v4.tsv")
	writeASNFile(t, path, "8.8.8.0\t8.8.8.255\t15169\tUS\tGOOGLE\n", time.Now())
	src := newASNSource(path, "")
	src.run.Lock()
	src.checking = true
	done := make(chan api.ASNTableStatus)
	go func() {
		st, _ := src.CheckASNTable(context.Background())
		done <- st
	}()
	select {
	case st := <-done:
		if !st.Checking || st.Loaded {
			t.Fatalf("concurrent check status = %+v", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manual check blocked behind a running refresh")
	}
	src.run.Unlock()
}

// TestASNSourceManagedStatus: managed mode reports its install directory and
// source, and a failed check is surfaced and retried on the next tick.
func TestASNSourceManagedStatus(t *testing.T) {
	t.Setenv("NETTACT_ASN_BASE_URL", "http://invalid.invalid/asn")
	dir := filepath.Join(t.TempDir(), "asn")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	src := newASNSource("", dir)
	src.now = func() time.Time { return now }
	src.load(context.Background())
	if st := src.ASNTableStatus(); st.Mode != api.ASNTableModeManaged || st.Location != dir ||
		st.Source != "http://invalid.invalid/asn" || st.Loaded || st.LastCheckAt != nil {
		t.Fatalf("managed status before any check = %+v", st)
	}
	src.refresh(context.Background())
	st := src.ASNTableStatus()
	if st.LastError == "" || st.LastCheckAt == nil || st.NextCheckAt == nil || !st.NextCheckAt.Equal(now.Add(asnTickInterval)) {
		t.Fatalf("managed status after failed check = %+v", st)
	}
}
