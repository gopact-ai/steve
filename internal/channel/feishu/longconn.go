package feishu

import (
	"errors"
	"sync"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// newLongConn is c's long connection to the Feishu at baseURL. What the
// official client does with the connection is reported to c.onReconnect.
func newLongConn(c *Channel, appID, appSecret, baseURL string, events *dispatcher.EventDispatcher) *larkws.Client {
	c.watch = &connWatch{report: c.onReconnect, refused: make(chan error, 1)}
	return larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(events),
		larkws.WithDomain(baseURL),
		larkws.WithLogLevel(larkcore.LogLevelInfo),
		larkws.WithOnDisconnected(c.watch.lost),
		larkws.WithOnReconnecting(c.watch.reconnecting),
		larkws.WithOnError(c.watch.failed),
		larkws.WithOnReconnected(c.watch.back),
	)
}

// connWatch turns the official client's connection callbacks into
// Reconnect reports. The client calls them on its own goroutines, one of
// them while holding its connection lock, so none of them blocks.
type connWatch struct {
	report func(*Reconnect)
	// refused carries the first failure the client will not retry.
	refused chan error

	mu sync.Mutex
	// current is the reconnect in progress; nil while connected.
	current *Reconnect
	// rounds counts the client's reconnect rounds not yet back. A
	// connection that drops as soon as it is made starts the next round
	// before the round that made it reports back.
	rounds int
	// stopped is set once the channel stops; nothing is reported after.
	stopped bool
}

// lost starts a reconnect unless one is in progress: the client reports a
// loss as a disconnect followed by reconnecting.
func (w *connWatch) lost() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lostLocked()
}

// reconnecting starts a round of attempts to connect again.
func (w *connWatch) reconnecting() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rounds++
	w.lostLocked()
}

func (w *connWatch) lostLocked() {
	if w.stopped || w.current != nil {
		return
	}
	w.current = &Reconnect{Since: time.Now()}
	w.publishLocked()
}

// failed records an attempt to connect that failed. A failure the client
// retries is reported; one it does not ends the channel's Start instead.
// The first connection after Ready failing starts a reconnect too.
func (w *connWatch) failed(err error) {
	if clientErr := (*larkws.ClientError)(nil); errors.As(err, &clientErr) {
		select {
		case w.refused <- err:
		default:
		}
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if w.current == nil {
		w.current = &Reconnect{Since: time.Now()}
	}
	w.current.Failures++
	w.current.Err = err
	w.publishLocked()
}

// back ends a round; the reconnect in progress ends with the last round.
func (w *connWatch) back() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rounds--
	if w.stopped || w.current == nil || w.rounds > 0 {
		return
	}
	w.current = nil
	w.publishLocked()
}

// publishLocked reports a copy of current; w.mu keeps reports in order.
func (w *connWatch) publishLocked() {
	if w.report == nil {
		return
	}
	if w.current == nil {
		w.report(nil)
		return
	}
	copied := *w.current
	w.report(&copied)
}

// stop ends reporting; the client may keep calling back after Close.
func (w *connWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}

// refusals is nil, never ready, without a watch.
func (w *connWatch) refusals() <-chan error {
	if w == nil {
		return nil
	}
	return w.refused
}
