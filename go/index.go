package fff

/*
#include <fff.h>
*/
import "C"

import (
	"encoding/json"
	"time"
	"unsafe"
)

// ScanProgress reports the state of indexing and the file watcher.
type ScanProgress struct {
	ScannedFiles     uint64
	IsScanning       bool
	IsWatcherReady   bool
	IsWarmupComplete bool
}

// Health is FFF's diagnostic report. Fields FFF omits stay at their zero value.
type Health struct {
	Version      string       `json:"version"`
	Git          GitHealth    `json:"git"`
	FilePicker   PickerHealth `json:"file_picker"`
	Frecency     StoreHealth  `json:"frecency"`
	QueryTracker StoreHealth  `json:"query_tracker"`
}

// GitHealth reports libgit2 and the repository found at the checked path.
type GitHealth struct {
	LibGit2Version  string `json:"libgit2_version"`
	Available       bool   `json:"available"`
	RepositoryFound bool   `json:"repository_found"`
	Workdir         string `json:"workdir"`
	Error           string `json:"error"`
}

// PickerHealth reports the file index; it is empty for the package HealthCheck.
type PickerHealth struct {
	Initialized  bool   `json:"initialized"`
	BasePath     string `json:"base_path"`
	IsScanning   bool   `json:"is_scanning"`
	IndexedFiles uint64 `json:"indexed_files"`
	Error        string `json:"error"`
}

// StoreHealth reports the frecency or query history database.
type StoreHealth struct {
	Initialized bool     `json:"initialized"`
	DB          DBHealth `json:"db_healthcheck"`
}

// DBHealth locates a database on disk.
type DBHealth struct {
	Path     string `json:"path"`
	DiskSize uint64 `json:"disk_size"`
}

// ScanFiles rescans the base path in the background.
func (i *Instance) ScanFiles() error {
	if i.handle == nil {
		return ErrClosed
	}
	_, _, err := unwrap(C.fff_scan_files(i.handle))
	return err
}

// IsScanning reports whether a scan is running. It is false once the instance is closed.
func (i *Instance) IsScanning() bool {
	if i.handle == nil {
		return false
	}
	return bool(C.fff_is_scanning(i.handle))
}

// BasePath returns the absolute directory being indexed.
func (i *Instance) BasePath() (string, error) {
	if i.handle == nil {
		return "", ErrClosed
	}
	handle, _, err := unwrap(C.fff_get_base_path(i.handle))
	if err != nil {
		return "", err
	}
	return takeString(handle), nil
}

// ScanProgress returns a snapshot of indexing and watcher state.
func (i *Instance) ScanProgress() (ScanProgress, error) {
	if i.handle == nil {
		return ScanProgress{}, ErrClosed
	}
	handle, _, err := unwrap(C.fff_get_scan_progress(i.handle))
	if err != nil {
		return ScanProgress{}, err
	}
	c := (*C.struct_FffScanProgress)(handle)
	progress := ScanProgress{
		ScannedFiles:     uint64(c.scanned_files_count),
		IsScanning:       bool(c.is_scanning),
		IsWatcherReady:   bool(c.is_watcher_ready),
		IsWarmupComplete: bool(c.is_warmup_complete),
	}
	C.fff_free_scan_progress(c)
	return progress, nil
}

// WaitForScan blocks until the current scan finishes and reports false on timeout.
// A timeout of zero or less checks once.
func (i *Instance) WaitForScan(timeout time.Duration) (bool, error) {
	if i.handle == nil {
		return false, ErrClosed
	}
	_, done, err := unwrap(C.fff_wait_for_scan(i.handle, millis(timeout)))
	return done == 1, err
}

// WaitForWatcher blocks until the file watcher is ready and reports false on timeout.
// A timeout of zero or less checks once.
func (i *Instance) WaitForWatcher(timeout time.Duration) (bool, error) {
	if i.handle == nil {
		return false, ErrClosed
	}
	_, done, err := unwrap(C.fff_wait_for_watcher(i.handle, millis(timeout)))
	return done == 1, err
}

// RestartIndex switches the instance to a new base path and rescans it. The content cache
// budget returns to automatic sizing, whatever the CacheBudget options were. Watch
// subscriptions stay live and keep their patterns, now relative to the new path.
func (i *Instance) RestartIndex(path string) error {
	if i.handle == nil {
		return ErrClosed
	}
	cpath, err := cString(path)
	if err != nil {
		return err
	}
	_, _, err = unwrap(C.fff_restart_index(i.handle, cpath))
	return err
}

// RefreshGitStatus reloads git status and returns how many paths git reported, clean
// files included.
func (i *Instance) RefreshGitStatus() (int, error) {
	if i.handle == nil {
		return 0, ErrClosed
	}
	_, n, err := unwrap(C.fff_refresh_git_status(i.handle))
	return int(n), err
}

// TrackQuery records query as opening filePath, which must exist (relative to the cwd, not the
// base path). It reports false with no index, and true but records nothing without HistoryDBPath.
func (i *Instance) TrackQuery(query, filePath string) (bool, error) {
	if i.handle == nil {
		return false, ErrClosed
	}
	cquery, err := cString(query)
	if err != nil {
		return false, err
	}
	cpath, err := cString(filePath)
	if err != nil {
		return false, err
	}
	_, tracked, err := unwrap(C.fff_track_query(i.handle, cquery, cpath))
	return tracked == 1, err
}

// HistoricalQuery returns the tracked query at offset, 0 being the most recent.
func (i *Instance) HistoricalQuery(offset uint64) (string, bool, error) {
	if i.handle == nil {
		return "", false, ErrClosed
	}
	handle, _, err := unwrap(C.fff_get_historical_query(i.handle, C.uint64_t(offset)))
	if err != nil || handle == nil {
		return "", false, err
	}
	return takeString(handle), true, nil
}

// HealthCheck reports on this instance, using testPath for the git check. An empty testPath
// checks the process working directory.
func (i *Instance) HealthCheck(testPath string) (Health, error) {
	if i.handle == nil {
		return Health{}, ErrClosed
	}
	return healthCheck(i.handle, testPath)
}

// HealthCheck reports the version and git availability without an instance. An empty testPath
// checks the process working directory.
func HealthCheck(testPath string) (Health, error) {
	return healthCheck(nil, testPath)
}

func healthCheck(handle unsafe.Pointer, testPath string) (Health, error) {
	path, err := cString(testPath)
	if err != nil {
		return Health{}, err
	}
	report, _, err := unwrap(C.fff_health_check(handle, path))
	if err != nil {
		return Health{}, err
	}
	var h Health
	v := cView((*C.char)(report))
	err = json.Unmarshal(unsafe.Slice(unsafe.StringData(v), len(v)), &h)
	C.fff_free_string((*C.char)(report))
	return h, err
}

func takeString(p unsafe.Pointer) string {
	s := C.GoString((*C.char)(p))
	C.fff_free_string((*C.char)(p))
	return s
}
