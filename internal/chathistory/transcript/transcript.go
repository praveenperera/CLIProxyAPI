// Package transcript rebuilds model calls from coding agents' local session files,
// Claude Code's project transcripts and Codex's rollouts, so history recorded
// before the proxy captured it can be imported into chat history
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// Stats counts what one file gave
type Stats struct {
	// Calls is how many model calls the file rebuilt
	Calls int
	// Unresolved counts messages a request referenced that the transcript does not
	// hold, each stored as a placeholder naming the missing hash
	Unresolved int
	// Chained counts calls rebuilt from the message chain because the file has no
	// request records for them, which older Claude Code versions did not write
	Chained int
	// BadLines counts lines that are not JSON, such as a line cut off by a crash
	BadLines int
}

// Add sums two files' stats
func (s Stats) Add(other Stats) Stats {
	return Stats{
		Calls:      s.Calls + other.Calls,
		Unresolved: s.Unresolved + other.Unresolved,
		Chained:    s.Chained + other.Chained,
		BadLines:   s.BadLines + other.BadLines,
	}
}

// eachLine calls fn with every non-empty line of path. Lines have no length limit,
// since one tool result can be megabytes
func eachLine(path string, fn func(line []byte)) error {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	defer func() { _ = file.Close() }()

	reader := bufio.NewReaderSize(file, 1<<20)
	for {
		line, errRead := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			fn(trimmed)
		}

		if errors.Is(errRead, io.EOF) {
			return nil
		}

		if errRead != nil {
			return errRead
		}
	}
}

// rawObject reads a field that some client versions write as a JSON object and
// others as a string holding that object
func rawObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || raw[0] != '"' {
		return raw
	}

	var text string
	if json.Unmarshal(raw, &text) != nil {
		return raw
	}

	return json.RawMessage(text)
}

func parseTime(text string) time.Time {
	parsed, errParse := time.Parse(time.RFC3339Nano, text)
	if errParse != nil {
		return time.Time{}
	}

	return parsed
}

// number reads an integer from a decoded JSON value, which Decode keeps as a
// json.Number
func number(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	case float64:
		return int64(typed)
	}

	return 0
}

func field(value any, keys ...string) any {
	for _, key := range keys {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}

		value = object[key]
	}

	return value
}

func text(value any, keys ...string) string {
	found, _ := field(value, keys...).(string)
	return found
}
