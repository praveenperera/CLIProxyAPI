package chathistory

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// tables lists the ClickHouse tables rows are spooled for, in upload order so that
// rows referencing blobs and nodes arrive after them; quota rows reference nothing
var tables = []string{"blobs", "nodes", "requests", "quota"}

const (
	openSuffix   = ".ndjson.open"
	closedSuffix = ".ndjson"
)

// spool appends rows to one open NDJSON segment per table and closes segments by
// renaming them, so the uploader only ever reads complete files and a crash leaves
// at most a truncated open segment, which is closed on the next start
type spool struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	maxSize  int64

	mu       sync.Mutex
	segments map[string]*segment
	// spooled tracks bytes on disk across closed and open segments, refreshed on each scan
	spooled int64
	dropped int64
}

type segment struct {
	path    string
	file    *os.File
	writer  *bufio.Writer
	size    int64
	created time.Time
}

func newSpool(dir string, maxBytes int64, maxAge time.Duration, maxSize int64) (*spool, error) {
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return nil, fmt.Errorf("chathistory: create spool dir: %w", errMkdir)
	}

	s := &spool{dir: dir, maxBytes: maxBytes, maxAge: maxAge, maxSize: maxSize, segments: map[string]*segment{}}

	// segments left open by a previous process hold complete lines up to the last
	// flush; trim any partial last line and close them so they upload
	leftovers, _ := filepath.Glob(filepath.Join(dir, "*"+openSuffix))
	for _, path := range leftovers {
		trimPartialLine(path)
		_ = os.Rename(path, strings.TrimSuffix(path, openSuffix)+closedSuffix)
	}

	s.refreshSize()
	return s, nil
}

// write appends rows for one request. All of a request's rows are kept or dropped
// together, so a full spool never stores nodes without their request
func (s *spool) write(rows map[string][]any) error {
	encoded := map[string][]byte{}
	var total int64
	for table, tableRows := range rows {
		var buf []byte
		for _, row := range tableRows {
			line, errMarshal := json.Marshal(row)
			if errMarshal != nil {
				return fmt.Errorf("chathistory: encode %s row: %w", table, errMarshal)
			}

			buf = append(buf, line...)
			buf = append(buf, '\n')
		}

		encoded[table] = buf
		total += int64(len(buf))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.maxBytes > 0 && s.spooled+total > s.maxBytes {
		s.dropped++
		return fmt.Errorf("chathistory: spool full (%d bytes), dropped request", s.spooled)
	}

	for _, table := range tables {
		data := encoded[table]
		if len(data) == 0 {
			continue
		}

		seg, errOpen := s.openSegment(table)
		if errOpen != nil {
			return errOpen
		}

		_, errWrite := seg.writer.Write(data)
		if errWrite == nil {
			// flush per request so a crash loses at most the request being written
			errWrite = seg.writer.Flush()
		}

		if errWrite != nil {
			// a bufio.Writer keeps failing after its first error and may have left a
			// partial line, which would get the whole segment rejected; keep the rows
			// written before this request and start a new segment
			_ = seg.file.Truncate(seg.size)
			s.closeSegment(table)
			return fmt.Errorf("chathistory: write %s segment: %w", table, errWrite)
		}

		seg.size += int64(len(data))
		s.spooled += int64(len(data))
		if seg.size >= s.maxSize {
			s.closeSegment(table)
		}
	}

	return nil
}

func (s *spool) openSegment(table string) (*segment, error) {
	if seg, ok := s.segments[table]; ok {
		return seg, nil
	}

	// the name doubles as the ClickHouse insert deduplication token, so it must be
	// unique per segment and stable across upload retries
	name := fmt.Sprintf("%s-%d%s", table, time.Now().UnixNano(), openSuffix)
	path := filepath.Join(s.dir, name)
	file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if errOpen != nil {
		return nil, fmt.Errorf("chathistory: open segment: %w", errOpen)
	}

	seg := &segment{path: path, file: file, writer: bufio.NewWriter(file), created: time.Now()}
	s.segments[table] = seg
	return seg, nil
}

// closeSegment flushes and renames a table's open segment; the caller holds mu
func (s *spool) closeSegment(table string) {
	seg, ok := s.segments[table]
	if !ok {
		return
	}

	delete(s.segments, table)
	_ = seg.writer.Flush()
	_ = seg.file.Close()
	trimPartialLine(seg.path)
	if seg.size == 0 {
		_ = os.Remove(seg.path)
		return
	}

	_ = os.Rename(seg.path, strings.TrimSuffix(seg.path, openSuffix)+closedSuffix)
}

// rotate closes segments older than maxAge so rows reach ClickHouse within about
// maxAge even when traffic is light
func (s *spool) rotate(force bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for table, seg := range s.segments {
		if force || time.Since(seg.created) >= s.maxAge {
			s.closeSegment(table)
		}
	}
}

// closed lists segments ready to upload, in table order and then oldest first
func (s *spool) closed() []string {
	var paths []string
	for _, table := range tables {
		matches, _ := filepath.Glob(filepath.Join(s.dir, table+"-*"+closedSuffix))
		sort.Strings(matches)
		paths = append(paths, matches...)
	}

	return paths
}

func (s *spool) refreshSize() {
	entries, _ := os.ReadDir(s.dir)
	var total int64
	for _, entry := range entries {
		// rejected segments are kept for inspection, not waiting for upload
		if strings.HasSuffix(entry.Name(), rejectedSuffix) {
			continue
		}

		if info, errInfo := entry.Info(); errInfo == nil && !entry.IsDir() {
			total += info.Size()
		}
	}

	s.mu.Lock()
	s.spooled = total
	s.mu.Unlock()
}

// trimPartialLine drops bytes after the last newline, which an interrupted write
// can leave behind and ClickHouse would reject
func trimPartialLine(path string) {
	data, errRead := os.ReadFile(path)
	if errRead != nil || len(data) == 0 || data[len(data)-1] == '\n' {
		return
	}

	end := strings.LastIndexByte(string(data), '\n') + 1
	_ = os.Truncate(path, int64(end))
}
