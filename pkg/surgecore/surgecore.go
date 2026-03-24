// Package surgecore provides a library interface for the Surge download engine.
// It is intended for use by package managers and other tools that need
// programmatic, embeddable download management.
package surgecore

import (
	"context"
	"time"
)

// DownloadID uniquely identifies a managed download.
type DownloadID string

// Config holds all options for initializing a Downloader.
// Callers own the paths — surge-core imposes no XDG opinions.
type Config struct {
	StateDir    string        // where SQLite state DB is stored
	Connections int           // max parallel connections per download (0 = auto)
	Timeout     time.Duration // per-request timeout
	UserAgent   string        // optional custom User-Agent header
}

// AddRequest describes a download to be queued.
type AddRequest struct {
	URL         string
	Mirrors     []string
	Destination string // directory or full file path
	Filename    string // optional override
	Connections int    // 0 = inherit from Config
	Headers     map[string]string
	Checksum    *Checksum
}

// Checksum describes an expected hash for verification after download.
type Checksum struct {
	Algorithm string // "sha256", "sha1", "md5"
	Value     string
}

// EventType classifies a streaming event.
type EventType int

const (
	EventProgress EventType = iota
	EventComplete
	EventError
	EventPaused
	EventResumed
	EventCancelled
)

// ProgressInfo carries download metrics on progress events.
type ProgressInfo struct {
	BytesDownloaded int64
	TotalBytes      int64
	SpeedBPS        float64 // bytes per second
	Connections     int
	Chunks          int
}

// Event is emitted on the channel returned by Get/Download.
type Event struct {
	Type     EventType
	ID       DownloadID
	Progress *ProgressInfo // non-nil on EventProgress
	Error    error         // non-nil on EventError
	Path     string        // non-nil on EventComplete — final file path
}

// DownloadStatus is a point-in-time snapshot of a download.
type DownloadStatus struct {
	ID       DownloadID
	URL      string
	Filename string
	Path     string
	State    string // "queued", "downloading", "paused", "complete", "error"
	Progress ProgressInfo
}

// Downloader is the main interface for surge-core consumers.
type Downloader interface {
	// Add queues a download and returns its ID immediately (non-blocking).
	Add(ctx context.Context, req AddRequest) (DownloadID, error)

	// Download starts/resumes a queued download and streams events until done.
	Download(ctx context.Context, id DownloadID) (<-chan Event, error)

	// Get is the convenience method: Add + Download in one call.
	// Blocks until the channel is closed (download finished or errored).
	Get(ctx context.Context, req AddRequest) (<-chan Event, error)

	// Pause suspends an active download (resumable).
	Pause(ctx context.Context, id DownloadID) error

	// Resume continues a paused download.
	Resume(ctx context.Context, id DownloadID) error

	// Cancel stops and removes a download.
	Cancel(ctx context.Context, id DownloadID) error

	// Status returns a snapshot of a download.
	Status(ctx context.Context, id DownloadID) (DownloadStatus, error)

	// List returns all known downloads.
	List(ctx context.Context) ([]DownloadStatus, error)

	// Close shuts down the engine cleanly.
	Close() error
}

// New creates a ready-to-use Downloader with the given config.
// This will wire up the internal engine, state DB, and worker pool.
func New(cfg Config) (Downloader, error) {
	// TODO: wire internal/download/manager.go, internal/engine, internal/processing
	panic("not yet implemented — wire your engine here")
}
