package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/nettact/server-core/asnlookup"
	"github.com/nettact/server/internal/asndb"
)

// asnUpdateInterval is how often the managed table asks the download center
// for a newer snapshot. Snapshots are published weekly; daily keeps a fresh
// install at most a day behind without polling for something that changes so
// rarely. A failed check is retried on the next hourly tick instead.
const asnUpdateInterval = 24 * time.Hour

// asnSource keeps the route monitor's IP→ASN table current. It runs in one of
// three modes, chosen once from Config:
//
//   - file: Config.ASNDBPath names an operator-supplied ip2asn-v4 file. It is
//     re-read when its modification time or size changes and never fetched —
//     an explicit file means the operator pins, mirrors or refreshes the data
//     on their own schedule.
//   - managed: no file, Config.ASNAutoUpdate set. Snapshots published through
//     the download center are installed under the data directory (see
//     internal/asndb) and checked for updates daily.
//   - off: neither. Lookups miss.
//
// No mode ever fails startup or ingest. Without a table the holder stays empty
// (or keeps the last good table) and route history records hops without AS
// attribution — the route monitor degrades to IP paths rather than taking the
// server down over an optional enrichment.
//
// refresh is called synchronously once from Start and then only from the
// hourly worker, so the fields below need no locking; readers go through the
// holder, which swaps atomically.
type asnSource struct {
	holder *asnlookup.Holder

	// file mode
	path       string
	loadedMod  time.Time
	loadedSize int64

	// managed mode
	installer *asndb.Installer
	installed string // version currently loaded; "" = none
	lastCheck time.Time
	now       func() time.Time
}

func newASNSource(path string, managedDir string) *asnSource {
	s := &asnSource{path: path, holder: asnlookup.NewHolder(nil), now: time.Now}
	if path == "" && managedDir != "" {
		s.installer = asndb.NewInstaller(managedDir)
	}
	return s
}

// load prepares the table available without the network: the operator file,
// or the previously installed snapshot. Called once from Start.
func (s *asnSource) load(ctx context.Context) {
	if s.installer == nil {
		s.refresh(ctx)
		return
	}
	t, err := s.installer.Load()
	if err != nil {
		log.Printf("asn table: %v (will download again)", err)
		return
	}
	if t != nil {
		s.holder.Store(t)
		s.installed = t.Version()
		log.Printf("asn table: loaded snapshot %s (%d ranges)", t.Version(), t.Len())
	}
}

// refresh is the hourly step: re-read a changed operator file, or check the
// download center when the daily interval has passed (sooner after a failure).
func (s *asnSource) refresh(ctx context.Context) {
	switch {
	case s.path != "":
		s.refreshFile()
	case s.installer != nil:
		s.refreshManaged(ctx)
	}
}

func (s *asnSource) refreshManaged(ctx context.Context) {
	if !s.lastCheck.IsZero() && s.now().Sub(s.lastCheck) < asnUpdateInterval {
		return
	}
	t, err := s.installer.Update(ctx, s.installed)
	if err != nil {
		log.Printf("asn table: update check failed (retrying within the hour): %v", err)
		return
	}
	s.lastCheck = s.now()
	if t == nil {
		return
	}
	s.holder.Store(t)
	s.installed = t.Version()
	log.Printf("asn table: installed snapshot %s (%d ranges)", t.Version(), t.Len())
}

func (s *asnSource) refreshFile() {
	fi, err := os.Stat(s.path)
	if err != nil {
		log.Printf("asn table: %v (route hops stay without AS attribution)", err)
		return
	}
	if fi.ModTime().Equal(s.loadedMod) && fi.Size() == s.loadedSize {
		return
	}
	t, err := loadASNFile(s.path, fi)
	if err != nil {
		log.Printf("asn table: %v (keeping previous table)", err)
		return
	}
	s.holder.Store(t)
	s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	log.Printf("asn table: loaded %d ranges from %s (version %s)", t.Len(), s.path, t.Version())
}

// loadASNFile parses an ip2asn-v4 file (plain or gzip). The version recorded
// with every attribution is the file's modification time: it changes exactly
// when the content an operator dropped in changes, which is the property the
// stored asn_db_version needs, without requiring a version field in the file.
func loadASNFile(path string, fi os.FileInfo) (*asnlookup.Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	version := fi.ModTime().UTC().Format("20060102T150405Z")
	t, err := asnlookup.LoadIP2ASN(f, version)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return t, nil
}
