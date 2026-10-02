package node

import (
	"errors"
	"io"
	"net/http"
	"sync"
)

var errReverseUploadStopped = errors.New("the final response ended the request upload")

// The MCP JSON request budget also bounds the unread tail of an early reply.
// All source bytes count, including those read before that reply. A response
// body has no corresponding size limit and may remain an open event stream.
const reverseRequestLimit = 1 << 20

// reverseUpload reads on demand into one fixed buffer. After a final reply it
// no longer uploads, but consumes the bounded tail to observe client closure.
// It lives only as long as the response: exchange cleanup interrupts its source
// read and joins it before returning. It never buffers the whole request.
type reverseUpload struct {
	source   io.ReadCloser
	want     chan int
	read     chan reverseRead
	consumed chan struct{}
	stop     chan struct{}
	end      chan struct{}
	done     chan struct{}
	failure  chan struct{}
	cause    error
	total    int64
	once     sync.Once
	endOnce  sync.Once
	failOnce sync.Once
}

type reverseRead struct {
	bytes []byte
	err   error
}

func newReverseUpload(source io.ReadCloser) *reverseUpload {
	u := &reverseUpload{source: source, want: make(chan int), read: make(chan reverseRead), consumed: make(chan struct{}), stop: make(chan struct{}), end: make(chan struct{}), done: make(chan struct{}), failure: make(chan struct{})}
	go u.run()
	return u
}

func (u *reverseUpload) run() {
	defer close(u.done)
	buffer := make([]byte, 32<<10)
	for {
		select {
		case size := <-u.want:
			n, err := u.readSource(buffer[:min(size, len(buffer))])
			select {
			case u.read <- reverseRead{buffer[:n], err}:
				// The consumer may still be copying while a final reply stops
				// upload. Do not reuse the buffer for tail reads before its ack.
				<-u.consumed
			case <-u.stop:
				if err == nil {
					u.discard(buffer)
				}
				return
			case <-u.end:
				return
			}
			if err != nil {
				return
			}
		case <-u.stop:
			u.discard(buffer)
			return
		case <-u.end:
			return
		}
	}
}

func (u *reverseUpload) readSource(p []byte) (int, error) {
	n, err := u.source.Read(p[:min(int64(len(p)), reverseRequestLimit-u.total+1)])
	u.total += int64(n)
	if u.total > reverseRequestLimit {
		n -= int(u.total - reverseRequestLimit)
		err = &http.MaxBytesError{Limit: reverseRequestLimit}
	}
	if err != nil && err != io.EOF {
		u.failOnce.Do(func() { u.cause = err; close(u.failure) })
	}
	return n, err
}

func (u *reverseUpload) discard(buffer []byte) {
	for {
		select {
		case <-u.end:
			return
		default:
		}
		if _, err := u.readSource(buffer); err != nil {
			return
		}
	}
}

func (u *reverseUpload) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case <-u.stop:
		return 0, errReverseUploadStopped
	default:
	}
	select {
	case u.want <- len(p):
	case <-u.done:
		return 0, io.EOF
	case <-u.stop:
		return 0, errReverseUploadStopped
	}
	select {
	case result := <-u.read:
		n := copy(p, result.bytes)
		u.consumed <- struct{}{}
		return n, result.err
	case <-u.stop:
		return 0, errReverseUploadStopped
	}
}

func (u *reverseUpload) Close() error {
	u.once.Do(func() { close(u.stop) })
	return nil
}

func (u *reverseUpload) finish() {
	u.endOnce.Do(func() { close(u.end) })
	u.Close()
}

func (u *reverseUpload) err() error {
	select {
	case <-u.failure:
		return u.cause
	default:
		return nil
	}
}

type reverseResponseBody struct {
	io.ReadCloser
	finish func()
	upload *reverseUpload
}

func (b *reverseResponseBody) Read(p []byte) (int, error) {
	if b.upload != nil {
		if err := b.upload.err(); err != nil {
			b.finish()
			return 0, err
		}
	}
	n, err := b.ReadCloser.Read(p)
	if b.upload != nil {
		if failed := b.upload.err(); failed != nil {
			err = failed
		}
	}
	if err != nil {
		b.finish()
	}
	return n, err
}

func (b *reverseResponseBody) Close() error {
	// End the stream before closing a partially read HTTP body: Close must not
	// try to drain an unbounded response that the caller has stopped reading.
	b.finish()
	return b.ReadCloser.Close()
}
