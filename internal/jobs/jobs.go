// Package jobs names the NATS messages Torza's services send each other.
//
// Postgres is the source of truth. These messages only wake a service early
// so it doesn't wait for its next scheduled check. If a message is lost (NATS
// restarted, a service was down), the next check picks the work up anyway.
package jobs

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

const (
	SubjectQueued  = "torza.torrent.queued"  // body: info hash
	SubjectBlocked = "torza.torrent.blocked" // body: info hash

	// IngestQueue is the NATS queue group ingest workers join, so each message
	// wakes one worker rather than all of them.
	IngestQueue = "ingest"
)

// Connect connects to NATS and keeps reconnecting forever, so a service
// starts even if NATS is briefly down.
func Connect(url string, log *slog.Logger) (*nats.Conn, error) {
	return nats.Connect(url,
		nats.Name("torza"),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("nats reconnected") }),
	)
}

// Notifier publishes wake-up messages.
type Notifier struct{ nc *nats.Conn }

func NewNotifier(nc *nats.Conn) Notifier { return Notifier{nc: nc} }

func (n Notifier) TorrentQueued(infohash string) error {
	return n.nc.Publish(SubjectQueued, []byte(infohash))
}

func (n Notifier) TorrentBlocked(infohash string) error {
	return n.nc.Publish(SubjectBlocked, []byte(infohash))
}

// Wake subscribes to subjects and turns every message into a signal on the
// returned channel. Signals never pile up: if one is already waiting, more
// are dropped, because one check handles all pending work.
func Wake(nc *nats.Conn, queue string, subjects ...string) (<-chan struct{}, error) {
	ch := make(chan struct{}, 1)
	for _, s := range subjects {
		if _, err := nc.QueueSubscribe(s, queue, func(*nats.Msg) {
			select {
			case ch <- struct{}{}:
			default:
			}
		}); err != nil {
			return nil, err
		}
	}
	return ch, nil
}
