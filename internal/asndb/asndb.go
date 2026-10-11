// Package asndb publishes and installs the route monitor's IP→ASN table.
//
// The table is iptoasn.com's ip2asn-v4 file, republished unchanged through the
// NetTact download center rather than fetched from iptoasn.com by every server:
// the publishing job validates each snapshot before it ships (a truncated or
// malformed upstream file never reaches a deployment), every server then
// installs a byte-identical, checksummed file with a stable version name, and a
// mirror or air-gapped deployment only has to host the same three files.
//
// One snapshot is one release tagged asn-YYYYMMDD carrying:
//
//	ip2asn-v4.tsv.gz  the upstream file, byte for byte
//	asn-db.json       Manifest — version, checksum, size, range count
//	SHA256SUMS        checksum of both, in sha256sum format
//
// and the download center serves the newest snapshot's manifest at
// <base>/asn-db.json and each snapshot's files at <base>/<tag>/<asset>.
//
// The file is kept in upstream format rather than converted to something more
// compact: asnlookup already parses it, a converter would be a second parser to
// keep honest, and gzip gets the upstream TSV within a few percent of a binary
// encoding.
package asndb

import (
	"fmt"
	"net/netip"
	"regexp"

	"github.com/nettact/server-core/asnlookup"
)

const (
	// DataAsset is the table file inside a snapshot.
	DataAsset = "ip2asn-v4.tsv.gz"
	// ManifestAsset describes a snapshot; the download center also serves the
	// newest one at <base>/ManifestAsset.
	ManifestAsset = "asn-db.json"
	// ChecksumAsset lists sha256 sums for DataAsset and ManifestAsset.
	ChecksumAsset = "SHA256SUMS"

	// SchemaVersion is Manifest.Schema. A reader rejects any other value
	// rather than guess at fields it does not know.
	SchemaVersion = 1

	// MaxDataBytes caps a download. The gzipped table is single-digit MB; a
	// file anywhere near this is not the table.
	MaxDataBytes = 64 << 20

	// MinRanges is the smallest range count a snapshot may carry. The full
	// table holds several hundred thousand ranges; a file far below that is a
	// truncated or placeholder upstream response that would silently strip AS
	// attribution from most hops, so it is refused at publish time and again
	// at install time.
	MinRanges = 100_000
)

// versionPattern is a snapshot version: its UTC publication date.
var versionPattern = regexp.MustCompile(`^[0-9]{8}$`)

// Manifest describes one published snapshot.
type Manifest struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"` // YYYYMMDD; also the stored asn_db_version
	Tag     string `json:"tag"`     // release tag holding the snapshot: "asn-" + Version
	Asset   string `json:"asset"`   // DataAsset
	SHA256  string `json:"sha256"`  // lowercase hex of the Asset bytes
	Size    int64  `json:"size"`    // Asset bytes
	Ranges  int    `json:"ranges"`  // ranges parsed from Asset
	Source  string `json:"source"`  // upstream URL the snapshot was taken from
	License string `json:"license"` // upstream data licence (SPDX)
}

// TagFor is the release tag for a snapshot version.
func TagFor(version string) string { return "asn-" + version }

// Check validates a manifest's shape before anything is downloaded on its word.
func (m Manifest) Check() error {
	switch {
	case m.Schema != SchemaVersion:
		return fmt.Errorf("manifest schema %d, want %d", m.Schema, SchemaVersion)
	case !versionPattern.MatchString(m.Version):
		return fmt.Errorf("manifest version %q is not YYYYMMDD", m.Version)
	case m.Tag != TagFor(m.Version):
		return fmt.Errorf("manifest tag %q does not match version %q", m.Tag, m.Version)
	case m.Asset != DataAsset:
		return fmt.Errorf("manifest asset %q, want %q", m.Asset, DataAsset)
	case len(m.SHA256) != 64:
		return fmt.Errorf("manifest sha256 %q is not a hex sha256", m.SHA256)
	case m.Size <= 0 || m.Size > MaxDataBytes:
		return fmt.Errorf("manifest size %d out of range", m.Size)
	case m.Ranges < MinRanges:
		return fmt.Errorf("manifest declares %d ranges, below the %d floor", m.Ranges, MinRanges)
	}
	return nil
}

// sanityProbes are addresses whose origin AS has been stable for over a
// decade. A parsed table that misattributes them is not a real ip2asn file
// (wrong column order, a different dataset), however many ranges it holds.
var sanityProbes = []struct {
	addr string
	asn  uint32
}{
	{"8.8.8.8", 15169}, // Google Public DNS
	{"1.1.1.1", 13335}, // Cloudflare
}

// Sanity checks a parsed table against the publish/install floor: enough
// ranges, and the well-known probes resolve to their long-standing origins.
func Sanity(t *asnlookup.Table) error {
	if n := t.Len(); n < MinRanges {
		return fmt.Errorf("table has %d ranges, below the %d floor", n, MinRanges)
	}
	for _, p := range sanityProbes {
		info, ok := t.Lookup(netip.MustParseAddr(p.addr))
		if !ok || info.ASN != p.asn {
			return fmt.Errorf("sanity probe %s resolved to AS%d (found=%v), want AS%d", p.addr, info.ASN, ok, p.asn)
		}
	}
	return nil
}
