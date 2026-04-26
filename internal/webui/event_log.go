package webui

import (
	"sync"
	"time"
)

type EventLog struct {
	mu      sync.RWMutex
	entries []EventLogEntry
	limit   int
	nextID  int64
}

type EventLogEntry struct {
	ID      int64                  `json:"id"`
	Time    string                 `json:"time"`
	Type    string                 `json:"type"`
	Message string                 `json:"message"`
	Fields  map[string]interface{} `json:"fields,omitempty"`
}

func NewEventLog(limit int) *EventLog {
	if limit <= 0 {
		limit = 200
	}
	return &EventLog{limit: limit}
}

func (l *EventLog) Add(eventType, message string, fields map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextID++
	l.entries = append(l.entries, EventLogEntry{
		ID:      l.nextID,
		Time:    time.Now().Format(time.RFC3339),
		Type:    eventType,
		Message: message,
		Fields:  fields,
	})
	if len(l.entries) > l.limit {
		l.entries = l.entries[len(l.entries)-l.limit:]
	}
}

func (l *EventLog) Recent() []EventLogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]EventLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}
