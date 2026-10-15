package api

import (
	"sync"
	"time"
)

// LogLine is one cursor-addressed log record.
type LogLine struct {
	Cursor uint64    `json:"cursor"`
	Time   time.Time `json:"time"`
	Text   string    `json:"text"`
}

// LogStore is an in-memory cursor log. File capture and reload across
// agent restarts are a later slice; the paging contract is implemented here.
type LogStore struct {
	mu    sync.Mutex
	next  uint64
	lines []LogLine
}

// NewLogStore returns an empty log.
func NewLogStore() *LogStore { return &LogStore{} }

// Append adds a line and returns it with its cursor.
func (s *LogStore) Append(text string) LogLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	line := LogLine{Cursor: s.next, Time: time.Now().UTC(), Text: text}
	s.lines = append(s.lines, line)
	return line
}

// Page returns lines with a cursor greater than after, up to limit.
// next is the last returned cursor, or after when the page is empty.
func (s *LogStore) Page(after uint64, limit int) (items []LogLine, next uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next = after
	for _, line := range s.lines {
		if line.Cursor <= after {
			continue
		}
		items = append(items, line)
		next = line.Cursor
		if len(items) == limit {
			break
		}
	}
	if items == nil {
		items = []LogLine{}
	}
	return items, next
}
