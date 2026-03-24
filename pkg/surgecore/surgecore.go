package surgecore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/surge-downloader/surge-core/internal/config"
	"github.com/surge-downloader/surge-core/internal/download"
	"github.com/surge-downloader/surge-core/internal/engine/state"
	"github.com/surge-downloader/surge-core/internal/engine/types"
	"github.com/surge-downloader/surge-core/internal/processing"
)

// DownloadID uniquely identifies a managed download.
type DownloadID string

// Config holds all options for initializing a Downloader.
type Config struct {
	StateDir    string        // where SQLite state DB is stored
	Connections int           // max parallel connections per download (0 = auto)
	MaxWorkers  int           // max concurrent downloads (0 = 3)
	Timeout     time.Duration // per-request timeout
	UserAgent   string        // optional custom User-Agent header
	ProxyURL    string        // optional proxy URL
	// Called on download complete or error — lets the app show OS notifications.
	// Optional: if nil, no notification is sent.
	Notify func(title, message string)
}

// AddRequest describes a download to be queued.
type AddRequest struct {
	URL         string
	Mirrors     []string
	Destination string // directory to save into
	Filename    string // optional override
	Connections int    // 0 = inherit from Config
	Headers     map[string]string
}

// EventType classifies a streaming event.
type EventType int

const (
	EventProgress  EventType = iota
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
	SpeedMBps       float64
	Connections     int
}

// Event is emitted on the channel returned by Get.
type Event struct {
	Type     EventType
	ID       DownloadID
	Progress *ProgressInfo // non-nil on EventProgress
	Error    error         // non-nil on EventError
	Path     string        // non-nil on EventComplete
}

// Downloader is the main interface for surge-core consumers.
type Downloader interface {
	// Get downloads a file and streams events until done or errored.
	// The channel is closed when the download finishes.
	Get(ctx context.Context, req AddRequest) (<-chan Event, error)

	// Add queues a download and returns its ID immediately (non-blocking).
	Add(ctx context.Context, req AddRequest) (DownloadID, error)

	// Pause suspends an active download.
	Pause(ctx context.Context, id DownloadID) error

	// Resume continues a paused download.
	Resume(ctx context.Context, id DownloadID) error

	// Cancel stops and removes a download.
	Cancel(ctx context.Context, id DownloadID) error

	// Close shuts down the engine cleanly, pausing any active downloads.
	Close() error
}

// downloader is the concrete implementation.
type downloader struct {
	pool *download.WorkerPool
	mgr  *processing.LifecycleManager
	cfg  Config
	ch   chan any // internal progress channel
}

// New creates a ready-to-use Downloader.
func New(cfg Config) (Downloader, error) {
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("surgecore: Config.StateDir is required")
	}
	if cfg.Connections == 0 {
		cfg.Connections = 16
	}
	if cfg.MaxWorkers == 0 {
		cfg.MaxWorkers = 3
	}

	// Init the SQLite state DB at the caller-supplied path.
	state.Configure(cfg.StateDir)

	// Internal progress channel — pool writes here, lifecycle manager reads here.
	progressCh := make(chan any, 256)

	pool := download.NewWorkerPool(progressCh, cfg.MaxWorkers)

	// addFunc hands a resolved download into the worker pool.
	addFunc := func(url, destPath, filename string, mirrors []string, headers map[string]string, isExplicitCategory bool, fileSize int64, supportsRange bool) (string, error) {
		id := uuid.New().String()
		settings, _ := config.LoadSettings()
		var proxyURL string
		if settings != nil {
			proxyURL = settings.Network.ProxyURL
		}
		if cfg.ProxyURL != "" {
			proxyURL = cfg.ProxyURL
		}
		connections := cfg.Connections
		poolCfg := types.DownloadConfig{
			ID:            id,
			URL:           url,
			OutputPath:    destPath,
			Filename:      filename,
			Mirrors:       mirrors,
			Headers:       headers,
			TotalSize:     fileSize,
			SupportsRange: supportsRange,
			ProgressCh:    progressCh,
			State:         types.NewProgressState(id, fileSize),
			Runtime: &types.RuntimeConfig{
				MaxConnectionsPerHost: connections,
				MinChunkSize:          types.MinChunk,
				WorkerBufferSize:      types.WorkerBuffer,
				ProxyURL:              proxyURL,
			},
		}
		pool.Add(poolCfg)
		return id, nil
	}

	addWithIDFunc := func(url, destPath, filename string, mirrors []string, headers map[string]string, id string, fileSize int64, supportsRange bool) (string, error) {
		settings, _ := config.LoadSettings()
		var proxyURL string
		if settings != nil {
			proxyURL = settings.Network.ProxyURL
		}
		if cfg.ProxyURL != "" {
			proxyURL = cfg.ProxyURL
		}
		connections := cfg.Connections
		poolCfg := types.DownloadConfig{
			ID:            id,
			URL:           url,
			OutputPath:    destPath,
			Filename:      filename,
			Mirrors:       mirrors,
			Headers:       headers,
			TotalSize:     fileSize,
			SupportsRange: supportsRange,
			ProgressCh:    progressCh,
			State:         types.NewProgressState(id, fileSize),
			Runtime: &types.RuntimeConfig{
				MaxConnectionsPerHost: connections,
				MinChunkSize:          types.MinChunk,
				WorkerBufferSize:      types.WorkerBuffer,
				ProxyURL:              proxyURL,
			},
		}
		pool.Add(poolCfg)
		return id, nil
	}

	mgr := processing.NewLifecycleManager(addFunc, addWithIDFunc)
	if cfg.Notify != nil {
		mgr.Notify = cfg.Notify
	}

	// Start the lifecycle event worker (persists state, handles file finalization).
	go mgr.StartEventWorker(progressCh)

	return &downloader{
		pool: pool,
		mgr:  mgr,
		cfg:  cfg,
		ch:   progressCh,
	}, nil
}

// Get is the convenience method — queue + stream events until done.
func (d *downloader) Get(ctx context.Context, req AddRequest) (<-chan Event, error) {
	id, err := d.Add(ctx, req)
	if err != nil {
		return nil, err
	}

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				out <- Event{Type: EventCancelled, ID: id}
				return
			case raw, ok := <-d.ch:
				if !ok {
					return
				}
				ev, done := translateEvent(string(id), raw)
				if ev != nil {
					out <- *ev
				}
				if done {
					return
				}
			}
		}
	}()
	return out, nil
}

// Add queues a download and returns its ID immediately.
func (d *downloader) Add(ctx context.Context, req AddRequest) (DownloadID, error) {
	if req.URL == "" {
		return "", fmt.Errorf("surgecore: URL is required")
	}
	if req.Destination == "" {
		return "", fmt.Errorf("surgecore: Destination is required")
	}

	dreq := &processing.DownloadRequest{
		URL:      req.URL,
		Filename: req.Filename,
		Path:     req.Destination,
		Mirrors:  req.Mirrors,
		Headers:  req.Headers,
	}

	id, err := d.mgr.Enqueue(ctx, dreq)
	if err != nil {
		return "", err
	}
	return DownloadID(id), nil
}

func (d *downloader) Pause(_ context.Context, id DownloadID) error {
	if !d.pool.Pause(string(id)) {
		return fmt.Errorf("surgecore: download %s not found or already paused", id)
	}
	return nil
}

func (d *downloader) Resume(_ context.Context, id DownloadID) error {
	if !d.pool.Resume(string(id)) {
		return fmt.Errorf("surgecore: download %s not found or not paused", id)
	}
	return nil
}

func (d *downloader) Cancel(_ context.Context, id DownloadID) error {
	d.pool.Cancel(string(id))
	return nil
}

func (d *downloader) Close() error {
	d.pool.GracefulShutdown()
	return nil
}

// translateEvent converts a raw pool event into a public Event.
// Returns (event, isDone) — isDone signals the caller to stop listening.
func translateEvent(id string, raw any) (*Event, bool) {
	type hasID interface{ GetDownloadID() string }

	switch m := raw.(type) {
	case interface {
		GetDownloadID() string
		GetDownloaded() int64
		GetTotal() int64
		GetSpeed() float64
	}:
		if m.GetDownloadID() != id {
			return nil, false
		}
		return &Event{
			Type: EventProgress,
			ID:   DownloadID(id),
			Progress: &ProgressInfo{
				BytesDownloaded: m.GetDownloaded(),
				TotalBytes:      m.GetTotal(),
				SpeedMBps:       m.GetSpeed(),
			},
		}, false

	default:
		// Use type names via fmt for event types that don't have a common interface yet.
		// This is intentionally simple — extend as needed.
		return nil, false
	}
}
