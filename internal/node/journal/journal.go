// Package journal stores the two ordered line streams of a harness process.
// It has no network or process knowledge. A failed journal must never become
// backpressure that kills the process whose output it was meant to preserve.
package journal

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxLine      = 8 << 20
	SegmentBytes = 64 << 20
	MaxBytes     = 256 << 20
	Retention    = time.Hour
)

var (
	ErrTooOld      = errors.New("too old")
	ErrAhead       = errors.New("journal: cursor ahead of stream")
	ErrLineTooLong = errors.New("journal: line exceeds 8 MiB")
	ErrIncomplete  = errors.New("journal: expected one complete line")
	ErrUnresumable = errors.New("stream is not resumable")
)

// Options permits small limits in tests; zero fields use production limits.
type Options struct {
	SegmentBytes int64
	MaxBytes     int64
	SyncInterval time.Duration
}

type Journal struct {
	In, Out *Log
	Dir     string
	mu      sync.Mutex
	opts    Options
	total   int64
	order   uint64
	err     error
	closed  bool
	done    chan struct{}
	stopped chan struct{}
}

type Log struct {
	j            *Journal
	name         string
	seq, dropped uint64
	segments     []*segment
}

type checkpoint struct {
	seq    uint64
	offset int64
}
type segment struct {
	path               string
	file               *os.File
	size               int64
	first, last, order uint64
	index              []checkpoint
}

func ValidID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func New(state, id string, opts Options) (*Journal, error) {
	if !ValidID(id) || state == "" {
		return nil, fmt.Errorf("journal: invalid state directory or stream id")
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = SegmentBytes
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = MaxBytes
	}
	if opts.SyncInterval <= 0 {
		opts.SyncInterval = 100 * time.Millisecond
	}
	dir := filepath.Join(state, "streams", id)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	// Never overwrite the evidence of a previous process with the same id.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	j := &Journal{Dir: dir, opts: opts, done: make(chan struct{}), stopped: make(chan struct{})}
	j.In, j.Out = &Log{j: j, name: "in"}, &Log{j: j, name: "out"}
	for _, l := range []*Log{j.In, j.Out} {
		if err := l.rotate(); err != nil {
			for _, log := range []*Log{j.In, j.Out} {
				for _, s := range log.segments {
					_ = s.file.Close()
				}
			}
			return nil, err
		}
	}
	go j.syncLoop()
	return j, nil
}

func (j *Journal) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

func (j *Journal) Disable(cause error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fail(cause)
}

func (j *Journal) fail(cause error) error {
	if j.err == nil {
		j.err = fmt.Errorf("%w: %w", ErrUnresumable, cause)
		_ = os.WriteFile(filepath.Join(j.Dir, "unresumable"), []byte(j.err.Error()+"\n"), 0o600)
	}
	return j.err
}

func (l *Log) Last() uint64 {
	l.j.mu.Lock()
	defer l.j.mu.Unlock()
	return l.seq
}

// Append accepts exactly one line including its newline. Sequence numbers
// start at one. Writes precede delivery; fsync is shared across both logs.
func (l *Log) Append(line []byte) (uint64, error) {
	j := l.j
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return l.seq, j.err
	}
	if j.closed {
		return l.seq, os.ErrClosed
	}
	if len(line) > MaxLine {
		return l.seq, j.fail(ErrLineTooLong)
	}
	if len(line) == 0 || line[len(line)-1] != '\n' || bytes.IndexByte(line[:len(line)-1], '\n') >= 0 {
		return l.seq, ErrIncomplete
	}
	seq := l.seq + 1
	record := append(strconv.AppendUint(nil, seq, 10), '\t')
	record = append(record, line...)
	s := l.segments[len(l.segments)-1]
	if s.size > 0 && s.size+int64(len(record)) > j.opts.SegmentBytes {
		if err := l.rotate(); err != nil {
			return l.seq, j.fail(err)
		}
		s = l.segments[len(l.segments)-1]
	}
	if (seq-s.first)%256 == 0 {
		s.index = append(s.index, checkpoint{seq, s.size})
	}
	n, err := s.file.Write(record)
	if err == nil && n != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return l.seq, j.fail(err)
	}
	s.size += int64(n)
	s.last, l.seq = seq, seq
	j.total += int64(n)
	if err := j.trim(); err != nil {
		return seq, j.fail(err)
	}
	return seq, nil
}

func (l *Log) rotate() error {
	j := l.j
	if len(l.segments) > 0 {
		s := l.segments[len(l.segments)-1]
		if err := s.file.Sync(); err != nil {
			return err
		}
		if err := s.file.Close(); err != nil {
			return err
		}
		s.file = nil
		path := filepath.Join(j.Dir, fmt.Sprintf("%s.%020d.jsonl", l.name, s.first))
		if err := os.Rename(s.path, path); err != nil {
			return err
		}
		s.path = path
	}
	path := filepath.Join(j.Dir, l.name+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	j.order++
	l.segments = append(l.segments, &segment{path: path, file: f, first: l.seq + 1, order: j.order})
	return nil
}

func (j *Journal) trim() error {
	for j.total > j.opts.MaxBytes {
		var oldest *Log
		for _, l := range []*Log{j.In, j.Out} {
			if len(l.segments) > 1 && (oldest == nil || l.segments[0].order < oldest.segments[0].order) {
				oldest = l
			}
		}
		if oldest == nil {
			return fmt.Errorf("journal: retention limit smaller than active segments")
		}
		s := oldest.segments[0]
		if err := os.Remove(s.path); err != nil {
			return err
		}
		oldest.dropped = s.last
		oldest.segments = oldest.segments[1:]
		j.total -= s.size
	}
	return nil
}

// Replay writes complete lines after the cursor, without the on-disk seq\t
// prefix. The snapshot is finite: appends continue while the writer blocks.
func (l *Log) Replay(after uint64, w io.Writer) error {
	return l.ReplayUntil(after, ^uint64(0), w)
}

// ReplayUntil bounds a replay so callers can atomically switch to live output.
func (l *Log) ReplayUntil(after, through uint64, w io.Writer) error {
	type piece struct {
		f            *os.File
		offset, size int64
	}
	var pieces []piece
	defer func() {
		for _, p := range pieces {
			_ = p.f.Close()
		}
	}()
	j := l.j
	j.mu.Lock()
	err := j.err
	if err == nil && after < l.dropped {
		err = ErrTooOld
	}
	if err == nil && after > l.seq {
		err = ErrAhead
	}
	through = min(through, l.seq)
	if err == nil {
		for _, s := range l.segments {
			if s.last <= after || s.first > through {
				continue
			}
			f, openErr := os.Open(s.path)
			if openErr != nil {
				err = j.fail(openErr)
				break
			}
			i := sort.Search(len(s.index), func(i int) bool { return s.index[i].seq > after+1 }) - 1
			var offset int64
			if i >= 0 {
				offset = s.index[i].offset
			}
			pieces = append(pieces, piece{f, offset, s.size})
		}
	}
	j.mu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range pieces {
		r := bufio.NewReader(io.NewSectionReader(p.f, p.offset, p.size-p.offset))
		for {
			record, err := r.ReadBytes('\n')
			if err == io.EOF && len(record) == 0 {
				break
			}
			if err != nil {
				return err
			}
			tab := bytes.IndexByte(record, '\t')
			if tab < 0 {
				return fmt.Errorf("journal: corrupt record")
			}
			seq, err := strconv.ParseUint(string(record[:tab]), 10, 64)
			if err != nil {
				return err
			}
			if seq > through {
				return nil
			}
			if seq > after {
				n, err := w.Write(record[tab+1:])
				if err != nil {
					return err
				}
				if n != len(record)-tab-1 {
					return io.ErrShortWrite
				}
			}
		}
	}
	return nil
}

// Finish records the process exit as the last output line. The marker also
// lets a later node invocation prune evidence left by an earlier invocation.
func (j *Journal) Finish(code int) error {
	_, err := j.Out.Append([]byte(fmt.Sprintf("exit %d\n", code)))
	markerErr := os.WriteFile(filepath.Join(j.Dir, "ended"), []byte(fmt.Sprintf("exit %d\n", code)), 0o600)
	return errors.Join(err, markerErr)
}

func (j *Journal) syncLoop() {
	defer close(j.stopped)
	ticker := time.NewTicker(j.opts.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			j.mu.Lock()
			j.sync()
			j.mu.Unlock()
		case <-j.done:
			return
		}
	}
}

func (j *Journal) sync() {
	for _, l := range []*Log{j.In, j.Out} {
		for _, s := range l.segments {
			if s.file != nil {
				if err := s.file.Sync(); err != nil {
					j.fail(err)
				}
			}
		}
	}
}

func (j *Journal) Close() error {
	j.mu.Lock()
	if !j.closed {
		j.closed = true
		close(j.done)
		j.sync()
		for _, l := range []*Log{j.In, j.Out} {
			for _, s := range l.segments {
				if s.file != nil {
					if err := s.file.Close(); err != nil {
						j.fail(err)
					}
					s.file = nil
				}
			}
		}
	}
	err := j.err
	j.mu.Unlock()
	<-j.stopped
	return err
}

// Prune removes only finished streams whose retention window has elapsed.
func Prune(state string, now time.Time) error {
	root := filepath.Join(state, "streams")
	dirs, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if !d.IsDir() || !ValidID(d.Name()) {
			continue
		}
		path := filepath.Join(root, d.Name())
		ended, err := os.Stat(filepath.Join(path, "ended"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if now.Sub(ended.ModTime()) >= Retention {
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// IsExit distinguishes the terminal record from ACP JSON lines.
func IsExit(line []byte) (string, bool) {
	reason := strings.TrimSuffix(string(line), "\n")
	code, ok := strings.CutPrefix(reason, "exit ")
	if !ok {
		return "", false
	}
	_, err := strconv.Atoi(code)
	return reason, err == nil
}
