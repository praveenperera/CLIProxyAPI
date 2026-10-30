package helps

import (
	"bufio"
	"bytes"
	"context"
	"io"

	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// ReadCodexQuotaSSE preserves the response bytes and observes complete data lines at receipt
// Oversized lines still reach legacy translation but cannot grow the observation buffer
func ReadCodexQuotaSSE(ctx context.Context, body io.Reader) ([]byte, error) {
	reader := bufio.NewReader(body)
	var response bytes.Buffer
	var line []byte
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		response.Write(fragment)
		if !oversized && len(line)+len(fragment) <= 4*1024*1024 {
			line = append(line, fragment...)
		} else {
			oversized = true
			line = nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !oversized && bytes.HasPrefix(line, []byte("data:")) {
			exec.ObserveQuotaEvent(ctx, bytes.TrimSpace(line[5:]))
		}
		line = nil
		oversized = false
		if err == io.EOF {
			return response.Bytes(), nil
		}
		if err != nil {
			return response.Bytes(), err
		}
	}
}
