// Command asndb turns a downloaded iptoasn.com ip2asn-v4 snapshot into a
// publishable release: it validates the file with the same parser and sanity
// floor the server applies at install time, then writes the snapshot, its
// manifest and SHA256SUMS into an output directory for the release step.
//
//	go run ./ci/asndb -in ip2asn-v4.tsv.gz -out dist -version 20261011
//
// Validation runs here, before anything is published, because a deployment
// that installed a truncated or malformed table would silently lose AS
// attribution on most hops; the server re-checks on install, but refusing at
// the source keeps a bad upstream day from ever producing a release.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/nettact/server-core/asnlookup"
	"github.com/nettact/server/internal/asndb"
)

const upstreamURL = "https://iptoasn.com/data/ip2asn-v4.tsv.gz"

func main() {
	in := flag.String("in", "", "downloaded ip2asn-v4.tsv.gz")
	out := flag.String("out", "dist", "output directory for the release assets")
	version := flag.String("version", "", "snapshot version, YYYYMMDD (UTC publication date)")
	flag.Parse()
	if *in == "" || *version == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*in, *out, *version); err != nil {
		log.Fatal(err)
	}
}

func run(in, out, version string) error {
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	if len(data) > asndb.MaxDataBytes {
		return fmt.Errorf("%s is %d bytes, above the %d cap", in, len(data), asndb.MaxDataBytes)
	}
	// Refuse anything that is not gzip: the published asset name promises it,
	// and an HTML error page served with 200 would otherwise parse as zero
	// ranges only by luck.
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return fmt.Errorf("%s is not gzip", in)
	}
	t, err := asnlookup.LoadIP2ASN(bytes.NewReader(data), version)
	if err != nil {
		return fmt.Errorf("parse %s: %w", in, err)
	}
	if err := asndb.Sanity(t); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	m := asndb.Manifest{
		Schema:  asndb.SchemaVersion,
		Version: version,
		Tag:     asndb.TagFor(version),
		Asset:   asndb.DataAsset,
		SHA256:  hex.EncodeToString(sum[:]),
		Size:    int64(len(data)),
		Ranges:  t.Len(),
		Source:  upstreamURL,
		License: "PDDL-1.0",
	}
	if err := m.Check(); err != nil {
		return err
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifest = append(manifest, '\n')
	msum := sha256.Sum256(manifest)

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	sums := fmt.Sprintf("%s  %s\n%s  %s\n", m.SHA256, asndb.DataAsset, hex.EncodeToString(msum[:]), asndb.ManifestAsset)
	for name, body := range map[string][]byte{
		asndb.DataAsset:     data,
		asndb.ManifestAsset: manifest,
		asndb.ChecksumAsset: []byte(sums),
	} {
		if err := os.WriteFile(filepath.Join(out, name), body, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("snapshot %s: %d ranges, %d bytes, sha256 %s\n", m.Tag, m.Ranges, m.Size, m.SHA256)
	return nil
}
