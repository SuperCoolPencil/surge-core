package surgecore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SuperCoolPencil/surge-core/internal/config"
	"github.com/SuperCoolPencil/surge-core/internal/download"
	"github.com/SuperCoolPencil/surge-core/internal/engine/events"
	"github.com/SuperCoolPencil/surge-core/internal/engine/state"
	"github.com/SuperCoolPencil/surge-core/internal/engine/types"
	"github.com/SuperCoolPencil/surge-core/internal/processing"
	"github.com/google/uuid"
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
	mu   sync.Mutex
	subs map[string]chan any // per-download subscribers
	done chan struct{}
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
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("surgecore: failed to create state dir: %w", err)
	}
	state.Configure(filepath.Join(cfg.StateDir, "surge.db"))

	// progressCh is written by the pool.
	// lifecycleCh feeds the lifecycle manager (persistence, file finalization).
	// Both are fed by a single broadcast goroutine so the pool only writes once.
	progressCh := make(chan any, 256)
	lifecycleCh := make(chan any, 256)

	pool := download.NewWorkerPool(progressCh, cfg.MaxWorkers)

	// addFunc hands a resolved download into the worker pool.
	addFunc := func(url, destPath, filename string, mirrors []string, headers map[string]string, fileSize int64, supportsRange bool) (string, error) {
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
	go mgr.StartEventWorker(lifecycleCh)

	d := &downloader{
		pool: pool,
		mgr:  mgr,
		cfg:  cfg,
		subs: make(map[string]chan any),
		done: make(chan struct{}),
	}

	// Fan-out: send every pool event to the lifecycle manager and all per-download subscribers.
	// Internal channel for downstream subscribers decoupled from lifecycle events.
	subsCh := make(chan any, 256)

	// Subscriber fan-out goroutine.
	go func() {
		for raw := range subsCh {
			isTerminal := false
			switch raw.(type) {
			case events.DownloadCompleteMsg, events.DownloadErrorMsg, events.DownloadPausedMsg:
				isTerminal = true
			}

			d.mu.Lock()
			for _, sub := range d.subs {
				if isTerminal {
					sub <- raw
				} else {
					select {
					case sub <- raw:
					default:
					}
				}
			}
			d.mu.Unlock()
		}
	}()

	// Primary broadcast goroutine (never blocks).
	go func() {
		for raw := range progressCh {
			isTerminal := false
			switch raw.(type) {
			case events.DownloadCompleteMsg, events.DownloadErrorMsg, events.DownloadPausedMsg:
				isTerminal = true
			}

			if isTerminal {
				go func(ev any) {
					// Feed lifecycle manager first to preserve enqueue ordering
					select {
					case lifecycleCh <- ev:
					case <-d.done:
						return
					}
					// Then feed subscribers
					select {
					case subsCh <- ev:
					case <-d.done:
						return
					}
				}(raw)
			} else {
				// Feed lifecycle manager
				select {
				case lifecycleCh <- raw:
				default:
				}

				// Feed subscribers
				select {
				case subsCh <- raw:
				default:
				}
			}
		}
	}()

	return d, nil
}

// Get is the convenience method — queue + stream events until done.
// It taps the shared progress channel and filters events by ID.
func (d *downloader) Get(ctx context.Context, req AddRequest) (<-chan Event, error) {
	id, err := d.Add(ctx, req)
	if err != nil {
		return nil, err
	}

	// Subscribe before the download starts so we don't miss early events.
	sub := make(chan any, 256)
	d.mu.Lock()
	d.subs[string(id)] = sub
	d.mu.Unlock()

	out := make(chan Event, 32)
	go func() {
		defer close(out)
		defer func() {
			d.mu.Lock()
			delete(d.subs, string(id))
			d.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				out <- Event{Type: EventCancelled, ID: id}
				return
			case raw, ok := <-sub:
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
	d.mu.Lock()
	select {
	case <-d.done:
	default:
		close(d.done)
	}
	d.mu.Unlock()
	d.pool.GracefulShutdown()
	return nil
}

// translateEvent converts a raw pool event into a public Event.
// Returns (event, isDone) — isDone signals the caller to stop listening.
func translateEvent(id string, raw any) (*Event, bool) {
	switch m := raw.(type) {
	case events.ProgressMsg:
		if m.DownloadID != id {
			return nil, false
		}
		return &Event{
			Type: EventProgress,
			ID:   DownloadID(id),
			Progress: &ProgressInfo{
				BytesDownloaded: m.Downloaded,
				TotalBytes:      m.Total,
				SpeedMBps:       m.Speed,
			},
		}, false

	case events.DownloadCompleteMsg:
		if m.DownloadID != id {
			return nil, false
		}
		// Recover the final path from the state DB.
		path := ""
		if entry, _ := state.GetDownload(m.DownloadID); entry != nil {
			path = entry.DestPath
		}
		return &Event{
			Type: EventComplete,
			ID:   DownloadID(id),
			Path: path,
		}, true

	case events.DownloadErrorMsg:
		if m.DownloadID != id {
			return nil, false
		}
		return &Event{
			Type:  EventError,
			ID:    DownloadID(id),
			Error: m.Err,
		}, true

	case events.DownloadPausedMsg:
		if m.DownloadID != id {
			return nil, false
		}
		return &Event{Type: EventPaused, ID: DownloadID(id)}, false

	case events.BatchProgressMsg:
		var latest *events.ProgressMsg
		for _, pm := range m {
			if pm.DownloadID == id {
				pmCopy := pm
				if latest == nil || pmCopy.Downloaded > latest.Downloaded {
					latest = &pmCopy
				}
			}
		}
		if latest != nil {
			// Recurse into translateEvent. This is safe because *latest is a single
			// events.ProgressMsg, which trivially matches and returns an EventProgress.
			return translateEvent(id, *latest)
		}
		return nil, false

	case events.DownloadResumedMsg:
		if m.DownloadID != id {
			return nil, false
		}
		return &Event{Type: EventResumed, ID: DownloadID(id)}, false

	default:
		return nil, false
	}
}
