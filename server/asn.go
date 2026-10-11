package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/nettact/server-core/api"
	"github.com/nettact/server-core/asnlookup"
	"github.com/nettact/server/internal/asndb"
)

const (
	// asnUpdateInterval is how often the managed table asks the download
	// center for a newer snapshot. Snapshots are published weekly; daily keeps
	// a fresh install at most a day behind without polling for something that
	// changes so rarely. A failed check is retried on the next hourly tick
	// instead.
	asnUpdateInterval = 24 * time.Hour

	// asnTickInterval is the worker cadence that drives refresh (see
	// workers.go). It is only used to tell the console when the next check
	// will run — a file is re-examined, and a failed managed check retried, on
	// every tick.
	asnTickInterval = time.Hour
)

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
// asnSource also implements api.ASNTableAPI, the console's status panel and
// "check now" button. Two locks keep that safe without letting a status read
// wait behind a download: run serializes refreshes (the hourly worker and a
// manual check never run the installer concurrently), and mu guards only the
// short field reads and writes. Lookups never touch either; they go through
// the holder, which swaps atomically.
type asnSource struct {
	holder *asnlookup.Holder
	now    func() time.Time

	run sync.Mutex // held for a whole refresh

	mu sync.Mutex // guards everything below
	// file mode
	path       string
	loadedMod  time.Time
	loadedSize int64
	// managed mode
	installer *asndb.Installer
	installed string    // version currently loaded; "" = none
	ranges    int       // range count of the loaded table
	lastCheck time.Time // last successful managed check; starts the daily interval
	// status reporting
	loadedAt    time.Time
	lastAttempt time.Time // last check of either mode, successful or not
	lastErr     string
	lastErrAt   time.Time
	checking    bool
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
	s.run.Lock()
	defer s.run.Unlock()
	t, err := s.installer.Load()
	if err != nil {
		log.Printf("asn table: %v (will download again)", err)
		s.recordErr(err)
		return
	}
	if t != nil {
		s.store(t)
		log.Printf("asn table: loaded snapshot %s (%d ranges)", t.Version(), t.Len())
	}
}

// refresh is the hourly step: re-read a changed operator file, or check the
// download center when the daily interval has passed (sooner after a failure).
func (s *asnSource) refresh(ctx context.Context) {
	s.run.Lock()
	defer s.run.Unlock()
	s.refreshLocked(ctx, false)
}

// refreshLocked runs one refresh with s.run held. force skips the "nothing to
// do yet" shortcuts: the managed daily interval and the unchanged-file check.
func (s *asnSource) refreshLocked(ctx context.Context, force bool) {
	switch {
	case s.path != "":
		s.refreshFile(force)
	case s.installer != nil:
		s.refreshManaged(ctx, force)
	}
}

func (s *asnSource) refreshManaged(ctx context.Context, force bool) {
	s.mu.Lock()
	due := force || s.lastCheck.IsZero() || s.now().Sub(s.lastCheck) >= asnUpdateInterval
	installed := s.installed
	s.mu.Unlock()
	if !due {
		return
	}
	s.markAttempt()
	t, err := s.installer.Update(ctx, installed)
	if err != nil {
		log.Printf("asn table: update check failed (retrying within the hour): %v", err)
		s.recordErr(err)
		return
	}
	s.mu.Lock()
	s.lastCheck = s.now()
	s.lastErr, s.lastErrAt = "", time.Time{}
	s.mu.Unlock()
	if t == nil {
		return
	}
	s.store(t)
	log.Printf("asn table: installed snapshot %s (%d ranges)", t.Version(), t.Len())
}

func (s *asnSource) refreshFile(force bool) {
	s.markAttempt()
	fi, err := os.Stat(s.path)
	if err != nil {
		log.Printf("asn table: %v (route hops stay without AS attribution)", err)
		s.recordErr(err)
		return
	}
	s.mu.Lock()
	unchanged := fi.ModTime().Equal(s.loadedMod) && fi.Size() == s.loadedSize
	s.mu.Unlock()
	if unchanged && !force {
		return
	}
	t, err := loadASNFile(s.path, fi)
	if err != nil {
		log.Printf("asn table: %v (keeping previous table)", err)
		s.recordErr(err)
		return
	}
	s.store(t)
	s.mu.Lock()
	s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	s.lastErr, s.lastErrAt = "", time.Time{}
	s.mu.Unlock()
	log.Printf("asn table: loaded %d ranges from %s (version %s)", t.Len(), s.path, t.Version())
}

// store publishes t to lookups and records it for the status panel.
func (s *asnSource) store(t *asnlookup.Table) {
	s.holder.Store(t)
	s.mu.Lock()
	s.installed, s.ranges = t.Version(), t.Len()
	s.loadedAt = s.now()
	s.mu.Unlock()
}

func (s *asnSource) markAttempt() {
	s.mu.Lock()
	s.lastAttempt = s.now()
	s.mu.Unlock()
}

func (s *asnSource) recordErr(err error) {
	s.mu.Lock()
	s.lastErr, s.lastErrAt = err.Error(), s.now()
	s.mu.Unlock()
}

func (s *asnSource) mode() string {
	switch {
	case s.path != "":
		return api.ASNTableModeFile
	case s.installer != nil:
		return api.ASNTableModeManaged
	}
	return api.ASNTableModeOff
}

// ASNTableStatus implements api.ASNTableAPI.
func (s *asnSource) ASNTableStatus() api.ASNTableStatus {
	st := api.ASNTableStatus{Mode: s.mode()}
	switch st.Mode {
	case api.ASNTableModeFile:
		st.Location = s.path
	case api.ASNTableModeManaged:
		st.Location = s.installer.Dir()
		st.Source = s.installer.BaseURL()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installed != "" {
		st.Loaded, st.Version, st.Ranges = true, s.installed, s.ranges
	}
	st.Checking = s.checking
	st.CanCheck = st.Mode != api.ASNTableModeOff
	st.LastError = s.lastErr
	st.LoadedAt = timePtr(s.loadedAt)
	st.LastCheckAt = timePtr(s.lastAttempt)
	st.LastErrorAt = timePtr(s.lastErrAt)
	switch st.Mode {
	case api.ASNTableModeFile:
		if !s.lastAttempt.IsZero() {
			st.NextCheckAt = timePtr(s.lastAttempt.Add(asnTickInterval))
		}
	case api.ASNTableModeManaged:
		// A successful check starts the daily interval; until one succeeds the
		// next hourly tick tries again.
		switch {
		case !s.lastCheck.IsZero() && !s.lastCheck.Before(s.lastAttempt):
			st.NextCheckAt = timePtr(s.lastCheck.Add(asnUpdateInterval))
		case !s.lastAttempt.IsZero():
			st.NextCheckAt = timePtr(s.lastAttempt.Add(asnTickInterval))
		}
	}
	return st
}

// CheckASNTable implements api.ASNTableAPI: a refresh that ignores the daily
// interval (managed) or re-reads the file even if it looks unchanged. A check
// already in progress is not queued behind — the caller gets the current
// status with Checking set and polls. Failures land in LastError rather than
// the returned error, so the panel shows them the same way as a failed
// scheduled check.
func (s *asnSource) CheckASNTable(ctx context.Context) (api.ASNTableStatus, error) {
	if s.mode() == api.ASNTableModeOff {
		return api.ASNTableStatus{}, api.ErrASNTableCheckUnsupported
	}
	if !s.run.TryLock() {
		return s.ASNTableStatus(), nil
	}
	s.mu.Lock()
	s.checking = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.checking = false
		s.mu.Unlock()
		s.run.Unlock()
	}()
	// The request context ends when the browser tab closes; a download that
	// far along should still finish and install.
	s.refreshLocked(context.WithoutCancel(ctx), true)
	s.mu.Lock()
	s.checking = false
	s.mu.Unlock()
	return s.ASNTableStatus(), nil
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC()
	return &t
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
