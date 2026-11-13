package ingest

import (
	"fmt"
	"time"

	"torza/internal/db"
	"torza/internal/source"
)

// Decision is what to do with a held download after a poll.
type Decision struct {
	Fail   string // non-empty: give up, with this reason for the user
	Switch string // non-empty: hand the item to this fetcher (stage 5: RD/TorBox)
}

// Policy holds the rules a fetcher's downloads are judged by.
type Policy struct {
	MetadataTimeout time.Duration // 0 = wait for peers forever
}

// Decide applies the policy table to one poll:
//
//	condition                                         → decision
//	no metadata after MetadataTimeout                 → fail (stage 5: switch, if the user has fallback)
//	size known and over the starting user's limit     → fail
//	otherwise                                         → keep going
func (p Policy) Decide(job db.Job, r source.Report, now time.Time) Decision {
	switch {
	case r.BytesTotal <= 0 && p.MetadataTimeout > 0 && now.Sub(job.ActiveSince) > p.MetadataTimeout:
		return Decision{Fail: "no peers sent this torrent's file list in " + p.MetadataTimeout.String()}
	case r.BytesTotal > 0 && job.MaxItemBytes > 0 && r.BytesTotal > job.MaxItemBytes:
		return Decision{Fail: fmt.Sprintf("it is %s; your limit is %s", humanBytes(r.BytesTotal), humanBytes(job.MaxItemBytes))}
	}
	return Decision{}
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
