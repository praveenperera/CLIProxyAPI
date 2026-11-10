package chathistory

import (
	"time"

	log "github.com/sirupsen/logrus"
)

// QuotaPoll is one read of a provider's usage endpoint, such as Claude's
// /api/oauth/usage or Codex's /backend-api/wham/usage
type QuotaPoll struct {
	At         time.Time
	Provider   string
	AuthID     string
	StatusCode int
	// Body is the reply as received. It is stored whole, because the proxy parses
	// only the windows it routes on and drops the rest, such as per-model limits
	Body []byte
}

type quotaRow struct {
	TS         string `json:"ts"`
	Provider   string `json:"provider"`
	AuthID     string `json:"auth_id"`
	Source     string `json:"source"`
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

// RecordQuotaPoll spools a usage reply for the quota table. The poller calls it on
// its own goroutine, off the request path, and polls once a minute per account,
// so it writes directly instead of going through the call queue
func RecordQuotaPoll(poll QuotaPoll) {
	recorder := active.Load()
	if recorder == nil || poll.AuthID == "" {
		return
	}

	at := poll.At
	if at.IsZero() {
		at = time.Now()
	}

	row := quotaRow{
		TS:         at.UTC().Format(clickhouseTime),
		Provider:   poll.Provider,
		AuthID:     poll.AuthID,
		Source:     "poll",
		StatusCode: min(max(poll.StatusCode, 0), 65535),
		Body:       string(poll.Body),
	}

	if errWrite := recorder.spool.write(map[string][]any{"quota": {row}}); errWrite != nil {
		log.Warnf("%v", errWrite)
	}
}
