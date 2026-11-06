// Command shrink_chat_images rewrites chat.blobs rows stored before the proxy shrank
// images: it reads the rows as JSONEachRow on stdin and writes the changed ones to
// stdout with the same hash, so nodes and requests still point at them. Inserting
// the output into chat.blobs replaces each row when parts merge, since the table is
// a ReplacingMergeTree keyed by hash and the last insert wins.
//
//	clickhouse-client -q "SELECT * FROM chat.blobs FINAL WHERE image_count > 0 FORMAT JSONEachRow" > before.jsonl
//	shrink_chat_images < before.jsonl > after.jsonl
//	clickhouse-client -q "INSERT INTO chat.blobs FORMAT JSONEachRow" < after.jsonl
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/chathistory"
)

func main() {
	if errRun := run(os.Stdin, os.Stdout, os.Stderr); errRun != nil {
		fmt.Fprintln(os.Stderr, "shrink_chat_images:", errRun)
		os.Exit(1)
	}
}

func run(in io.Reader, out, log io.Writer) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	writer := bufio.NewWriter(out)
	var rows, changed, before, after int
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			// every other column is passed through untouched
			var row map[string]json.RawMessage
			if errJSON := json.Unmarshal(line, &row); errJSON != nil {
				return fmt.Errorf("row %d: %w", rows+1, errJSON)
			}

			var content string
			if errJSON := json.Unmarshal(row["content"], &content); errJSON != nil {
				return fmt.Errorf("row %d content: %w", rows+1, errJSON)
			}

			rows++
			shrunk, ok, errShrink := chathistory.ShrinkStoredMessage([]byte(content))
			if errShrink != nil {
				return fmt.Errorf("row %d: %w", rows, errShrink)
			}

			if ok {
				changed++
				before += len(content)
				after += len(shrunk)
				row["content"], _ = json.Marshal(string(shrunk))
				row["bytes"], _ = json.Marshal(len(shrunk))
				encoded, errEncode := json.Marshal(row)
				if errEncode != nil {
					return errEncode
				}

				if _, errWrite := writer.Write(append(encoded, '\n')); errWrite != nil {
					return errWrite
				}
			}
		}

		if errRead == io.EOF {
			break
		}

		if errRead != nil {
			return errRead
		}
	}

	if errFlush := writer.Flush(); errFlush != nil {
		return errFlush
	}

	_, _ = fmt.Fprintf(log, "%d rows read, %d rewritten, content %d -> %d bytes\n", rows, changed, before, after)
	return nil
}
