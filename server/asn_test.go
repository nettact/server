package server

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
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
