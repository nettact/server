package webui

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxDistBytes caps the total extracted size — a built SPA is a few MB, so
// anything near this limit is corrupt or malicious.
const maxDistBytes = 512 << 20

// run retries downloadAndInstall until success or ctx cancellation, with
// backoff. On success it swaps the handler and cleans up old versions.
func (m *Manager) run(ctx context.Context) {
	defer close(m.done)
	for attempt := 0; ; attempt++ {
		err := m.downloadAndInstall(ctx)
		if err == nil {
			m.store(spaHandler(os.DirFS(filepath.Join(m.dir, m.version))))
			m.logf("webui: %s installed and serving", m.version)
			m.cleanupOthers()
			return
		}
		if ctx.Err() != nil {
			return
		}
		d := m.backoff(attempt)
		m.logf("webui: download %s failed (attempt %d, retrying in %s): %v", m.version, attempt+1, d, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// downloadAndInstall performs one bounded attempt: fetch SHA256SUMS, stream the
// tarball through a sha256 check, extract to a temp dir with tar hardening,
// then atomically rename into place.
func (m *Manager) downloadAndInstall(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return fmt.Errorf("create webui dir: %w", err)
	}

	asset := fmt.Sprintf("web-console-dist-%s.tar.gz", m.version)
	wantSum, err := m.fetchChecksum(ctx, asset)
	if err != nil {
		return err
	}

	tarPath, err := m.fetchTarball(ctx, asset, wantSum)
	if err != nil {
		return err
	}
	defer os.Remove(tarPath)

	tmpDir, err := m.extract(tarPath)
	if err != nil {
		return err
	}

	if !hasIndex(os.DirFS(tmpDir)) {
		os.RemoveAll(tmpDir)
		return errors.New("downloaded dist has no index.html")
	}
	// Write the marker into the extracted tree before the final rename so the
	// UI and its version become visible atomically. A crash can therefore
	// produce only a complete, matching install or an invalid install that the
	// next start downloads again.
	if err := os.WriteFile(filepath.Join(tmpDir, installedVersionFile), []byte(m.version), 0o644); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("write installed version: %w", err)
	}

	target := filepath.Join(m.dir, m.version)
	// A stale partial install can only exist after a crash mid-rename chain; it
	// was never served, so removing it races with nothing.
	if err := os.RemoveAll(target); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("clear stale install: %w", err)
	}
	if err := os.Rename(tmpDir, target); err != nil {
		os.RemoveAll(tmpDir)
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

func (m *Manager) fetchChecksum(ctx context.Context, asset string) (string, error) {
	body, err := m.get(ctx, m.baseURL+"/"+m.version+"/SHA256SUMS")
	if err != nil {
		return "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read SHA256SUMS: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", asset)
}

func (m *Manager) fetchTarball(ctx context.Context, asset, wantSum string) (string, error) {
	body, err := m.get(ctx, m.baseURL+"/"+m.version+"/"+asset)
	if err != nil {
		return "", err
	}
	defer body.Close()

	f, err := os.CreateTemp(m.dir, ".download-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	h := sha256.New()
	_, err = io.Copy(f, io.TeeReader(io.LimitReader(body, maxDistBytes), h))
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("download %s: %w", asset, errors.Join(err, cerr))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		os.Remove(f.Name())
		return "", fmt.Errorf("checksum mismatch for %s: got %s want %s", asset, got, wantSum)
	}
	return f.Name(), nil
}

func (m *Manager) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// extract unpacks the verified tarball into a fresh .tmp-* dir inside m.dir
// (same volume as the final rename target). Tar hardening: only regular files
// and directories, no absolute paths or .. escapes, bounded total size.
func (m *Manager) extract(tarPath string) (string, error) {
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmpDir := filepath.Join(m.dir, ".tmp-"+m.version+"-"+hex.EncodeToString(rnd[:]))
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("create extract dir: %w", err)
	}
	if err := extractTo(tmpDir, tarPath); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}
	return tmpDir, nil
}

func extractTo(dst, tarPath string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("tar entry escapes dist dir: %q", hdr.Name)
		}
		path := filepath.Join(dst, name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += hdr.Size; total > maxDistBytes {
				return errors.New("dist exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, maxDistBytes))
			cerr := out.Close()
			if err != nil || cerr != nil {
				return errors.Join(err, cerr)
			}
		default:
			// Symlinks, hardlinks, devices etc. have no business in a web dist.
			return fmt.Errorf("unsupported tar entry type %d: %q", hdr.Typeflag, hdr.Name)
		}
	}
}

// cleanupOthers best-effort removes leftovers in m.dir: other installed
// versions (v… dirs), stale extract dirs (.tmp-*), and orphaned downloads
// (.download-*). Anything else is left alone — the operator may have pointed
// WebUIDir at a directory that also holds unrelated data, and deleting
// unrecognized entries would destroy it.
func (m *Manager) cleanupOthers() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == m.version {
			continue
		}
		owned := strings.HasPrefix(name, ".tmp-") || strings.HasPrefix(name, ".download-") ||
			(e.IsDir() && len(name) > 1 && name[0] == 'v' && name[1] >= '0' && name[1] <= '9')
		if !owned {
			continue
		}
		if err := os.RemoveAll(filepath.Join(m.dir, name)); err != nil {
			m.logf("webui: cleanup %s: %v", name, err)
		}
	}
}
