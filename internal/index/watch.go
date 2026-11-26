package index

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

// Watch pacing. A rescan walks the whole library, so the pause before the
// next one scales with how long the last one took: however large the library
// and however busy the folders, scanning stays a small share of wall time.
var (
	watchDebounce    = 350 * time.Millisecond
	watchPollEvery   = 5 * time.Second
	watchMaxInterval = 2 * time.Minute
)

const watchBackoffFactor = 10

// watchByPolling selects timer-driven rescans over filesystem notifications.
// It is on wherever notification would cost one open descriptor per watched
// file: the kqueue backend must open every file in a watched directory, which
// exhausts the process descriptor limit on a large library.
var watchByPolling = notificationCostsADescriptorPerFile(runtime.GOOS)

func notificationCostsADescriptorPerFile(goos string) bool {
	switch goos {
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		return true
	default:
		return false
	}
}

// Watch keeps the index in step with the configured folders and their
// sidecar trees until ctx is cancelled, so files and edits made outside this
// process appear without a restart. It blocks; when it returns no scan is
// running any more, and the caller may close the index.
//
// Scans never overlap: one loop owns them, a change that arrives during a
// scan is remembered and served by the next one, and the first scan runs
// right after the watches are registered so nothing that changed in between
// is missed.
func (idx *Index) Watch(ctx context.Context) error {
	var (
		events <-chan fsnotify.Event
		errs   <-chan error
		addDir = func(string) {}
	)
	polling := watchByPolling
	if !polling {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			return err
		}
		defer func() { _ = watcher.Close() }()
		events, errs = watcher.Events, watcher.Errors
		addDir = func(root string) { addWatchTree(watcher, root) }
		for _, fc := range idx.Folders() {
			addDir(fc.Path)
			if sRoot := sidecar.SidecarRoot(fc.Path, fc.ID, idx.sidecarDir); isDir(sRoot) {
				addDir(sRoot)
			}
		}
	}

	var (
		timer    *time.Timer
		fire     <-chan time.Time
		scanDone chan time.Duration
		dirty    = true // first scan closes the gap since the caller's last one
		earliest time.Time
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer, fire = nil, nil
		}
	}
	defer stopTimer()
	arm := func(delay time.Duration) {
		if scanDone != nil || timer != nil {
			return
		}
		if wait := time.Until(earliest); wait > delay {
			delay = wait
		}
		timer = time.NewTimer(delay)
		fire = timer.C
	}
	arm(0)

	for {
		select {
		case <-ctx.Done():
			if scanDone != nil {
				<-scanDone
			}
			return nil

		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Has(fsnotify.Create) && isDir(ev.Name) {
				addDir(ev.Name)
			}
			if strings.Contains(ev.Name, string(filepath.Separator)+".tmp"+string(filepath.Separator)) {
				continue
			}
			dirty = true
			arm(watchDebounce)

		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			// A dropped notification (queue overflow) means unknown changes.
			dirty = true
			arm(watchDebounce)

		case <-fire:
			timer, fire = nil, nil
			if !dirty && !polling {
				continue
			}
			dirty = false
			done := make(chan time.Duration, 1)
			scanDone = done
			go func() {
				start := time.Now()
				_ = idx.ScanAll(ctx)
				done <- time.Since(start)
			}()

		case took := <-scanDone:
			scanDone = nil
			pause := min(took*watchBackoffFactor, watchMaxInterval)
			earliest = time.Now().Add(pause)
			switch {
			case polling:
				arm(max(watchPollEvery, pause))
			case dirty:
				arm(watchDebounce)
			}
		}
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// addWatchTree registers root and every directory below it, skipping the
// sidecar store's scratch and quarantine areas.
func addWatchTree(watcher *fsnotify.Watcher, root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best-effort directory watcher registration
		}
		base := d.Name()
		if base == ".tmp" || base == ".quarantine" || strings.HasPrefix(base, "._") {
			return filepath.SkipDir
		}
		_ = watcher.Add(p)
		return nil
	})
}
