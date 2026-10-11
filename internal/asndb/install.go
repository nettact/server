package asndb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nettact/server-core/asnlookup"
)

const (
	// DefaultBaseURL is the download center's ASN table location.
	DefaultBaseURL = "https://d.nettact.org/asn"

	// EnvBaseURL overrides DefaultBaseURL (mirror / air-gapped deployments).
	// The source must serve <base>/asn-db.json and <base>/<tag>/<asset>.
	EnvBaseURL = "NETTACT_ASN_BASE_URL"
)

// Installer keeps an installed snapshot in dir current with the download
// center and loads it into a table.
//
// Updates only move forward: a manifest whose version is not newer than the
// installed one is ignored, so a stale cached manifest, a mirror that lags
// behind, or a rolled-back upstream can never replace a newer table with an
// older one. Every download is verified (size, sha256, parse, Sanity, range
// count matching the manifest) before it replaces anything, and the install is
// two renames — data, then manifest — so a crash between them leaves a data
// file whose checksum no longer matches the old manifest, which Load treats as
// "nothing installed" and the next check repairs.
type Installer struct {
	dir     string
	baseURL string

	// Seams for tests.
	client *http.Client
}

// NewInstaller manages the snapshot under dir, downloading from
// NETTACT_ASN_BASE_URL or DefaultBaseURL.
func NewInstaller(dir string) *Installer {
	base := DefaultBaseURL
	if v := os.Getenv(EnvBaseURL); v != "" {
		base = v
	}
	return &Installer{
		dir:     dir,
		baseURL: strings.TrimRight(base, "/"),
		client:  &http.Client{Timeout: 5 * time.Minute},
	}
}

func (in *Installer) dataPath() string     { return filepath.Join(in.dir, DataAsset) }
func (in *Installer) manifestPath() string { return filepath.Join(in.dir, ManifestAsset) }

// Load parses the installed snapshot. It returns (nil, nil) when nothing valid
// is installed — first start, or an install interrupted between its renames —
// since both cases are repaired by the next Update rather than being errors.
func (in *Installer) Load() (*asnlookup.Table, error) {
	raw, err := os.ReadFile(in.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil
	}
	if m.Check() != nil {
		return nil, nil
	}
	data, err := os.ReadFile(in.dataPath())
	if err != nil {
		return nil, nil
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, nil
	}
	t, err := asnlookup.LoadIP2ASN(bytes.NewReader(data), m.Version)
	if err != nil {
		return nil, fmt.Errorf("installed asn table: %w", err)
	}
	return t, nil
}

// Update fetches the published manifest and, when it names a newer snapshot
// than installed (installed "" = none), downloads, verifies and installs it.
// It returns the new table, or nil when nothing newer was published.
func (in *Installer) Update(ctx context.Context, installed string) (*asnlookup.Table, error) {
	m, err := in.fetchManifest(ctx)
	if err != nil {
		return nil, err
	}
	if installed != "" && m.Version <= installed {
		return nil, nil
	}
	data, err := in.fetchData(ctx, m)
	if err != nil {
		return nil, err
	}
	t, err := asnlookup.LoadIP2ASN(bytes.NewReader(data), m.Version)
	if err != nil {
		return nil, fmt.Errorf("parse %s %s: %w", m.Tag, DataAsset, err)
	}
	if t.Len() != m.Ranges {
		return nil, fmt.Errorf("%s parsed to %d ranges, manifest declares %d", m.Tag, t.Len(), m.Ranges)
	}
	if err := Sanity(t); err != nil {
		return nil, fmt.Errorf("%s: %w", m.Tag, err)
	}
	if err := in.install(m, data); err != nil {
		return nil, err
	}
	return t, nil
}

func (in *Installer) fetchManifest(ctx context.Context) (Manifest, error) {
	body, err := in.get(ctx, in.baseURL+"/"+ManifestAsset, 64<<10)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", ManifestAsset, err)
	}
	if err := m.Check(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func (in *Installer) fetchData(ctx context.Context, m Manifest) ([]byte, error) {
	data, err := in.get(ctx, in.baseURL+"/"+m.Tag+"/"+m.Asset, MaxDataBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != m.Size {
		return nil, fmt.Errorf("%s/%s is %d bytes, manifest declares %d", m.Tag, m.Asset, len(data), m.Size)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != m.SHA256 {
		return nil, fmt.Errorf("checksum mismatch for %s/%s", m.Tag, m.Asset)
	}
	return data, nil
}

// get reads a whole response, refusing anything larger than limit.
func (in *Installer) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := in.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, limit)
	}
	return data, nil
}

func (in *Installer) install(m Manifest, data []byte) error {
	if err := os.MkdirAll(in.dir, 0o755); err != nil {
		return fmt.Errorf("create asn dir: %w", err)
	}
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(in.dir, in.dataPath(), data); err != nil {
		return err
	}
	return writeAtomic(in.dir, in.manifestPath(), append(manifest, '\n'))
}

// writeAtomic writes data to a temp file in dir and renames it over path.
func writeAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".asn-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		os.Remove(f.Name())
		return fmt.Errorf("write %s: %w", filepath.Base(path), errors.Join(werr, cerr))
	}
	if err := os.Rename(f.Name(), path); err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("install %s: %w", filepath.Base(path), err)
	}
	return nil
}
