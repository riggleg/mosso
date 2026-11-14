package worker

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// maxLogBytes bounds the event log. Past it, tool inputs and results
	// are replaced with a placeholder; Claude's text and progress are
	// still recorded.
	maxLogBytes = 64 << 20
	// maxInputBytes bounds one tool input (for example the content of a
	// Write). Larger inputs are replaced with a placeholder.
	maxInputBytes = 64 << 10
	// maxLineBytes bounds one line read back from the log file.
	maxLineBytes = 32 << 20
)

// Omitted replaces tool data once the log is full.
const Omitted = "[mosso: not kept, the worker's event log is full]"

// Log is an append-only event log: a JSON Lines file, fsync'd after every
// event so an abrupt stop loses at most the event being written, mirrored
// in memory for serving. Sequence numbers start at 1 and never repeat.
type Log struct {
	mu      sync.Mutex
	f       *os.File
	events  []Event
	bytes   int64
	full    bool
	changed chan struct{}
}

// OpenLog opens or creates the log at path and loads its events. A
// truncated last line (from a crash mid-write) is discarded.
func OpenLog(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, changed: make(chan struct{})}
	if err := l.load(); err != nil {
		f.Close()
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return l, nil
}

func (l *Log) load() error {
	r := bufio.NewReaderSize(l.f, 64<<10)
	var good int64 // bytes up to the end of the last complete, valid line
	for {
		line, err := readLine(r)
		if len(line) > 0 && bytes.HasSuffix(line, []byte("\n")) {
			var e Event
			if json.Unmarshal(line, &e) == nil && e.Seq == int64(len(l.events))+1 {
				l.events = append(l.events, e)
				good += int64(len(line))
				l.bytes = good
				continue
			}
		}
		if err == io.EOF || len(line) > 0 {
			break // a partial or corrupt tail ends the log
		}
		if err != nil {
			return err
		}
	}
	if err := l.f.Truncate(good); err != nil {
		return err
	}
	if _, err := l.f.Seek(good, io.SeekStart); err != nil {
		return err
	}
	l.full = l.bytes >= maxLogBytes
	return nil
}

// readLine reads one line including its newline, or whatever is left.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		line = append(line, part...)
		if err == nil || err != bufio.ErrBufferFull {
			if len(line) > maxLineBytes {
				return line, errors.New("line too long")
			}
			return line, err
		}
	}
}

// Append records e with the next sequence number and the current time,
// bounds its size, writes and syncs it, and wakes followers. It returns the
// event as recorded.
func (l *Log) Append(e Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Seq = int64(len(l.events)) + 1
	e.Time = time.Now().UTC()
	e = l.boundLocked(e)
	line, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	line = append(line, '\n')
	if _, err := l.f.Write(line); err != nil {
		return e, err
	}
	if err := l.f.Sync(); err != nil {
		return e, err
	}
	l.events = append(l.events, e)
	l.bytes += int64(len(line))
	close(l.changed)
	l.changed = make(chan struct{})
	return e, nil
}

// boundLocked trims oversized tool data and, once the log is full, all of
// it. A notice is recorded once when the log fills up.
func (l *Log) boundLocked(e Event) Event {
	if e.Event == nil {
		return e
	}
	ev := *e.Event
	if len(ev.Input) > maxInputBytes {
		ev.Input, _ = json.Marshal(fmt.Sprintf("[mosso: tool input omitted, %d bytes]", len(ev.Input)))
	}
	if !l.full && l.bytes+int64(len(ev.Input)+len(ev.Content)+len(ev.Text)) > maxLogBytes {
		l.full = true
		notice := Event{Seq: e.Seq, Time: e.Time, Type: EventNotice, TaskID: e.TaskID,
			Text: "The worker's event log reached its size limit, so further tool inputs and outputs are not recorded. Claude keeps working."}
		if line, err := json.Marshal(notice); err == nil {
			line = append(line, '\n')
			if _, err := l.f.Write(line); err == nil {
				l.events = append(l.events, notice)
				l.bytes += int64(len(line))
				e.Seq++
			}
		}
	}
	if l.full {
		ev.Input = nil
		if ev.Content != "" {
			ev.Content = Omitted
		}
	}
	e.Event = &ev
	return e
}

// After returns copies of the events with a sequence number greater than seq.
func (l *Log) After(seq int64) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < 0 {
		seq = 0
	}
	if seq >= int64(len(l.events)) {
		return nil
	}
	out := make([]Event, len(l.events)-int(seq))
	copy(out, l.events[seq:])
	return out
}

// Seq returns the latest sequence number, 0 for an empty log.
func (l *Log) Seq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.events))
}

// Last returns the latest event, if any.
func (l *Log) Last() (Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) == 0 {
		return Event{}, false
	}
	return l.events[len(l.events)-1], true
}

// Changed returns a channel that is closed on the next Append.
func (l *Log) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// Close closes the file.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

// LastSessionID returns the most recent Claude Code session ID in the log.
func (l *Log) LastSessionID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		if l.events[i].SessionID != "" {
			return l.events[i].SessionID
		}
	}
	return ""
}

// OpenTask returns the ID of the task whose events have not reached a
// terminal event yet, or "".
func (l *Log) OpenTask() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.events) - 1; i >= 0; i-- {
		e := l.events[i]
		if e.TaskID == "" {
			continue
		}
		if e.Terminal() {
			return ""
		}
		return e.TaskID
	}
	return ""
}
