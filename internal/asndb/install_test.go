package asndb

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// syntheticTable builds a gzipped ip2asn-v4 file with n filler ranges in
// 100.0.0.0/8-free space plus the sanity probes, so Sanity passes without a
// real multi-megabyte upstream file in testdata.
func syntheticTable(t *testing.T, n int, googleASN uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	fmt.Fprintf(zw, "1.1.1.0\t1.1.1.255\t13335\tUS\tCLOUDFLARENET\n")
	fmt.Fprintf(zw, "8.8.8.0\t8.8.8.255\t%d\tUS\tGOOGLE\n", googleASN)
	// Filler: consecutive /24s from 20.0.0.0 upward, one AS each.
	for i := 0; i < n; i++ {
		a := netip.AddrFrom4([4]byte{20 + byte(i>>16), byte(i >> 8), byte(i), 0})
		b := netip.AddrFrom4([4]byte{20 + byte(i>>16), byte(i >> 8), byte(i), 255})
		fmt.Fprintf(zw, "%s\t%s\t%d\tZZ\tFILLER-%d\n", a, b, 64512+i%1000, i)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func manifestFor(version string, data []byte, ranges int) Manifest {
	sum := sha256.Sum256(data)
	return Manifest{
		Schema: SchemaVersion, Version: version, Tag: TagFor(version), Asset: DataAsset,
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Ranges: ranges,
		Source: "https://iptoasn.com/data/ip2asn-v4.tsv.gz", License: "PDDL-1.0",
	}
}

// fakeCenter is an in-memory download center behind an http.RoundTripper, so
// the tests never bind a socket.
type fakeCenter struct {
	mu       sync.Mutex
	files    map[string][]byte // path under the base URL → body
	requests []string
}

func (f *fakeCenter) publish(t *testing.T, m Manifest, data []byte) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files["/"+ManifestAsset] = raw
	f.files["/"+m.Tag+"/"+m.Asset] = data
}

func (f *fakeCenter) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/asn")
	f.requests = append(f.requests, p)
	body, ok := f.files[p]
	status := http.StatusOK
	if !ok {
		status, body = http.StatusNotFound, []byte("not found")
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

func newTestInstaller(t *testing.T) (*Installer, *fakeCenter) {
	t.Helper()
	center := &fakeCenter{files: map[string][]byte{}}
	in := NewInstaller(filepath.Join(t.TempDir(), "asn"))
	in.baseURL = "https://center.test/asn"
	in.client = &http.Client{Transport: center}
	return in, center
}

const fillerRanges = MinRanges // + 2 probe ranges

func TestUpdateInstallsVerifiesAndReloads(t *testing.T) {
	in, center := newTestInstaller(t)
	data := syntheticTable(t, fillerRanges, 15169)
	center.publish(t, manifestFor("20261005", data, fillerRanges+2), data)

	tbl, err := in.Update(context.Background(), "")
	if err != nil || tbl == nil {
		t.Fatalf("Update = %v, %v; want a table", tbl, err)
	}
	if tbl.Version() != "20261005" {
		t.Fatalf("version = %q", tbl.Version())
	}

	loaded, err := in.Load()
	if err != nil || loaded == nil || loaded.Version() != "20261005" || loaded.Len() != fillerRanges+2 {
		t.Fatalf("Load after install = %v, %v", loaded, err)
	}
}

func TestUpdateOnlyMovesForward(t *testing.T) {
	in, center := newTestInstaller(t)
	data := syntheticTable(t, fillerRanges, 15169)
	center.publish(t, manifestFor("20261005", data, fillerRanges+2), data)

	for _, installed := range []string{"20261005", "20261012"} {
		tbl, err := in.Update(context.Background(), installed)
		if err != nil || tbl != nil {
			t.Fatalf("installed %s: Update = %v, %v; want no-op", installed, tbl, err)
		}
	}
	for _, p := range center.requests {
		if strings.HasSuffix(p, DataAsset) {
			t.Fatalf("data downloaded for a non-newer manifest: %v", center.requests)
		}
	}
}

func TestUpdateRejectsBadSnapshots(t *testing.T) {
	good := syntheticTable(t, fillerRanges, 15169)
	cases := map[string]func(*testing.T) (Manifest, []byte){
		"checksum mismatch": func(t *testing.T) (Manifest, []byte) {
			m := manifestFor("20261005", good, fillerRanges+2)
			m.SHA256 = strings.Repeat("0", 64)
			return m, good
		},
		"range count disagrees with manifest": func(t *testing.T) (Manifest, []byte) {
			return manifestFor("20261005", good, fillerRanges+99), good
		},
		"sanity probe misattributed": func(t *testing.T) (Manifest, []byte) {
			bad := syntheticTable(t, fillerRanges, 64500)
			return manifestFor("20261005", bad, fillerRanges+2), bad
		},
		"manifest below the range floor": func(t *testing.T) (Manifest, []byte) {
			return manifestFor("20261005", good, 10), good
		},
		"tag does not match version": func(t *testing.T) (Manifest, []byte) {
			m := manifestFor("20261005", good, fillerRanges+2)
			m.Tag = "asn-20990101"
			return m, good
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			in, center := newTestInstaller(t)
			m, data := mk(t)
			center.publish(t, m, data)
			if tbl, err := in.Update(context.Background(), ""); err == nil || tbl != nil {
				t.Fatalf("Update = %v, %v; want rejection", tbl, err)
			}
			if tbl, _ := in.Load(); tbl != nil {
				t.Fatal("a rejected snapshot was installed")
			}
		})
	}
}

// TestLoadTreatsTornInstallAsAbsent: a crash between the data and manifest
// renames leaves new data under the old manifest; Load must not trust it.
func TestLoadTreatsTornInstallAsAbsent(t *testing.T) {
	in, center := newTestInstaller(t)
	data := syntheticTable(t, fillerRanges, 15169)
	center.publish(t, manifestFor("20261005", data, fillerRanges+2), data)
	if _, err := in.Update(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.dataPath(), []byte("torn"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tbl, err := in.Load(); err != nil || tbl != nil {
		t.Fatalf("Load of a torn install = %v, %v; want nothing installed", tbl, err)
	}
}

func TestLoadWithNothingInstalled(t *testing.T) {
	in, _ := newTestInstaller(t)
	if tbl, err := in.Load(); err != nil || tbl != nil {
		t.Fatalf("Load = %v, %v; want (nil, nil)", tbl, err)
	}
}

// TestUpdateSkipsAnIdenticalTable: a newer snapshot whose table is
// byte-identical to the installed one is not downloaded again.
func TestUpdateSkipsAnIdenticalTable(t *testing.T) {
	in, center := newTestInstaller(t)
	data := syntheticTable(t, fillerRanges, 15169)
	center.publish(t, manifestFor("20261005", data, fillerRanges+2), data)
	if _, err := in.Update(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	center.requests = nil
	center.publish(t, manifestFor("20261012", data, fillerRanges+2), data)
	tbl, err := in.Update(context.Background(), "20261005")
	if err != nil || tbl != nil {
		t.Fatalf("Update = %v, %v; want no-op for an identical table", tbl, err)
	}
	for _, p := range center.requests {
		if strings.HasSuffix(p, DataAsset) {
			t.Fatalf("identical table downloaded again: %v", center.requests)
		}
	}
}
