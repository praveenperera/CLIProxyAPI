package chathistory

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Event is one finished upstream model call
type Event struct {
	RequestedAt time.Time
	RequestID   string
	// Source names where the call was recorded: empty or proxy for calls the proxy
	// captured, or the client whose transcript an importer read, such as
	// claude-code or codex
	Source          string
	SessionID       string
	ParentSessionID string
	Provider        string
	Model           string
	ResponseModel   string
	AuthID          string
	ReasoningEffort string
	Stream          bool
	Failed          bool
	StatusCode      int
	Error           string
	Latency         time.Duration
	TTFT            time.Duration

	InputTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	OutputTokens     int64
	ReasoningTokens  int64

	// RequestBody is the final body sent upstream, after translation
	RequestBody []byte
	// Chunks are the raw upstream response: one JSON body, SSE lines, or
	// websocket frames
	Chunks [][]byte
}

// Options configures a Recorder
type Options struct {
	// SpoolDir holds segments waiting for upload
	SpoolDir string
	// Endpoint is the ClickHouse HTTP URL, such as http://code.local:8123. Empty
	// spools without uploading, for an importer's dry run
	Endpoint string
	Database string
	User     string
	Password string
	// MaxSpoolBytes stops recording new requests while this much is waiting, so a
	// long ClickHouse outage cannot fill the disk; zero means no limit
	MaxSpoolBytes int64
	// FlushInterval bounds how long a row waits before upload
	FlushInterval time.Duration
	// QueueSize is how many finished calls may wait for processing before new
	// ones are dropped rather than slowing the request path
	QueueSize int
}

// Recorder turns finished calls into deduplicated rows, spools them, and uploads
// them in the background. Record never blocks
type Recorder struct {
	queue    chan Event
	spool    *spool
	uploader *uploader
	interval time.Duration

	blobs     *seenSet
	nodes     *seenSet
	responses *leafMap

	dropped atomic.Int64
	// rejected counts segments ClickHouse refused, so an importer can fail
	// instead of reporting success
	rejected atomic.Int64
	// forget asks the worker to clear what it has written, after ClickHouse
	// rejected a segment whose blobs and nodes later rows would point at
	forget atomic.Bool
	// the worker stops first and the uploader second, so the final upload sees
	// every call that was queued before Close
	stopProcess context.CancelFunc
	stopShip    context.CancelFunc
	processDone sync.WaitGroup
	shipDone    sync.WaitGroup
}

// NewRecorder opens the spool, uploads anything a previous process left behind,
// and starts the background workers
func NewRecorder(opts Options) (*Recorder, error) {
	opts = withDefaults(opts)
	sp, errSpool := newSpool(opts.SpoolDir, opts.MaxSpoolBytes, opts.FlushInterval, 32<<20)
	if errSpool != nil {
		return nil, errSpool
	}

	processCtx, stopProcess := context.WithCancel(context.Background())
	shipCtx, stopShip := context.WithCancel(context.Background())
	r := &Recorder{
		queue: make(chan Event, opts.QueueSize),
		spool: sp,
		uploader: &uploader{
			endpoint: strings.TrimRight(opts.Endpoint, "/"),
			database: opts.Database,
			user:     opts.User,
			password: opts.Password,
			client:   &http.Client{Timeout: 2 * time.Minute},
		},
		interval:  opts.FlushInterval,
		blobs:     newSeenSet(200_000),
		nodes:     newSeenSet(200_000),
		responses: newLeafMap(20_000),

		stopProcess: stopProcess,
		stopShip:    stopShip,
	}

	r.processDone.Add(1)
	go r.process(processCtx)
	if opts.Endpoint != "" {
		r.shipDone.Add(1)
		go r.ship(shipCtx)
	}

	return r, nil
}

func withDefaults(opts Options) Options {
	if opts.Database == "" {
		opts.Database = "chat"
	}

	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 30 * time.Second
	}

	if opts.QueueSize <= 0 {
		// each queued call holds its whole request body, which reaches megabytes
		// late in long sessions
		opts.QueueSize = 64
	}

	return opts
}

// Record queues a finished call. When the queue is full the call is dropped and
// counted, because recording must never delay a response
func (r *Recorder) Record(event Event) {
	if r == nil {
		return
	}

	select {
	case r.queue <- event:
	default:
		// counted only: logging here could stall the request on a slow log writer,
		// so the uploader reports drops
		r.dropped.Add(1)
	}
}

// Close stops accepting work, writes what is queued, closes open segments and
// makes one last upload attempt
func (r *Recorder) Close() {
	if r == nil {
		return
	}

	r.stopProcess()
	r.processDone.Wait()
	r.stopShip()
	r.shipDone.Wait()
	if r.uploader.endpoint == "" {
		// nothing uploads a dry run's segments, so close them for reading
		r.spool.rotate(true)
	}
}

func (r *Recorder) process(ctx context.Context) {
	defer r.processDone.Done()

	for {
		select {
		case event := <-r.queue:
			r.write(event)
		case <-ctx.Done():
			for {
				select {
				case event := <-r.queue:
					r.write(event)
				default:
					return
				}
			}
		}
	}
}

func (r *Recorder) write(event Event) {
	// a bug in parsing one call must not take the proxy down with it
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Errorf("chathistory: dropped call %s after panic: %v", event.RequestID, recovered)
		}
	}()

	r.resetIfRejected()
	rows, responseID := r.rows(event)
	if errWrite := r.spool.write(rows); errWrite != nil {
		log.Warnf("%v", errWrite)
		return
	}

	r.remember(rows, responseID)
}

// resetIfRejected forgets what was written once ClickHouse rejected a segment, so
// later calls send their blobs and nodes again
func (r *Recorder) resetIfRejected() {
	if r.forget.Swap(false) {
		r.blobs = newSeenSet(r.blobs.limit)
		r.nodes = newSeenSet(r.nodes.limit)
	}
}

// remember marks spooled rows as written. It runs only once rows are safely
// spooled, so a dropped request does not stop a later one from storing the same
// content
func (r *Recorder) remember(rows map[string][]any, responseID string) {
	for _, row := range rows["blobs"] {
		r.blobs.add(row.(blobRow).Hash)
	}

	for _, row := range rows["nodes"] {
		node := row.(nodeRow)
		r.nodes.add(node.SessionID + "|" + node.Node)
	}

	// the response's own output is not a node; a request continuing it links to
	// the request's last message, and the output stays in requests.response
	if request, ok := rows["requests"][0].(requestRow); ok && responseID != "" && request.Leaf != "" {
		r.responses.put(responseID, leaf{node: request.Leaf, depth: request.MessageCount})
	}
}

func (r *Recorder) ship(ctx context.Context) {
	defer r.shipDone.Done()

	ticker := time.NewTicker(min(r.interval, 5*time.Second))
	defer ticker.Stop()

	var failures int
	var retryAt time.Time
	var reported int64
	for {
		select {
		case <-ctx.Done():
			// Close stops this loop only after process has spooled the queue
			r.spool.rotate(true)
			final, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r.uploadAll(final)
			cancel()
			return
		case <-ticker.C:
		}

		r.spool.rotate(false)
		if dropped := r.dropped.Load(); dropped > reported {
			log.Warnf("chathistory: queue full, dropped %d calls so far", dropped)
			reported = dropped
		}

		if time.Now().Before(retryAt) {
			continue
		}

		if errUpload := r.uploadAll(ctx); errUpload != nil {
			failures++
			retryAt = time.Now().Add(backoff(failures))
			if failures == 1 || failures%10 == 0 {
				log.Warnf("%v (attempt %d, retrying in %s)", errUpload, failures, backoff(failures))
			}

			continue
		}

		failures = 0
	}
}

// uploadAll uploads closed segments in order and stops at the first retryable
// failure, so rows keep their table order
func (r *Recorder) uploadAll(ctx context.Context) error {
	defer r.spool.refreshSize()

	for _, path := range r.spool.closed() {
		errUpload := r.uploader.upload(ctx, path)
		if errUpload == nil {
			continue
		}

		var retryable *retryableError
		if errors.As(errUpload, &retryable) {
			return errUpload
		}

		r.forget.Store(true)
		r.rejected.Add(1)
		log.Errorf("%v", errUpload)
	}

	return nil
}
