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
	ID       int64                  `json:"id"`
	Time     string                 `json:"time"`
	Category string                 `json:"category"`
	Type     string                 `json:"type"`
	Message  string                 `json:"message"`
	Fields   map[string]interface{} `json:"fields,omitempty"`
}

func NewEventLog(limit int) *EventLog {
	if limit <= 0 {
		limit = 200
	}
	return &EventLog{limit: limit}
}

func (l *EventLog) Add(eventType, message string, fields map[string]interface{}) {
	l.AddCategory("trade", eventType, message, fields)
}

func (l *EventLog) AddSystem(eventType, message string, fields map[string]interface{}) {
	l.AddCategory("system", eventType, message, fields)
}

func (l *EventLog) AddReject(eventType, message string, fields map[string]interface{}) {
	l.AddCategory("reject", eventType, message, fields)
}

func (l *EventLog) AddOperation(eventType, message string, fields map[string]interface{}) {
	l.AddCategory("operation", eventType, message, fields)
}

func (l *EventLog) AddCategory(category, eventType, message string, fields map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.nextID++
	l.entries = append(l.entries, EventLogEntry{
		ID:       l.nextID,
		Time:     time.Now().Format(time.RFC3339),
		Category: category,
		Type:     eventType,
		Message:  message,
		Fields:   fields,
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

func (l *EventLog) RecentByCategory(category string) []EventLogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	out := make([]EventLogEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		if entry.Category == category {
			out = append(out, entry)
		}
	}
	return out
}

func (l *EventLog) ClearCategory(category string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if category == "all" {
		l.entries = nil
		return
	}
	kept := l.entries[:0]
	for _, entry := range l.entries {
		if entry.Category != category {
			kept = append(kept, entry)
		}
	}
	l.entries = kept
}
