package agent

import (
	"sync"
	"time"
)

// EventType enumerates the observable agent events.
type EventType string

// Event types published on the bus. CLI and web subscribe to render them.
const (
	EventUser           EventType = "user"
	EventAssistantDelta EventType = "assistant_delta"
	// EventReasoningDelta carries the model's incremental "thinking" text when
	// the provider exposes it. It precedes the visible answer and is rendered
	// by the frontends; the assembled reasoning is stored on the assistant
	// message (llm.Message.ReasoningContent) and sent back on later requests.
	EventReasoningDelta EventType = "reasoning_delta"
	EventAssistant      EventType = "assistant"
	EventToolCall       EventType = "tool_call"
	EventToolResult     EventType = "tool_result"
	EventInfo           EventType = "info"
	EventError          EventType = "error"
	EventCompacted      EventType = "compacted"
	EventUsage          EventType = "usage"
	EventTurnDone       EventType = "turn_done"
	// EventInterrupted reports that a running turn was cancelled by the user.
	EventInterrupted EventType = "interrupted"
)

// Event is a single observable occurrence during a turn.
type Event struct {
	Type    EventType `json:"type"`
	Text    string    `json:"text,omitempty"`
	Name    string    `json:"name,omitempty"`
	Args    string    `json:"args,omitempty"`
	IsError bool      `json:"is_error,omitempty"`
	// Source identifies who submitted a user message ("cli", "web"). It is
	// empty for events that do not originate from user input.
	Source string `json:"source,omitempty"`
	// Tokens and ContextWindow carry context-usage numbers for EventUsage.
	Tokens        int       `json:"tokens,omitempty"`
	ContextWindow int       `json:"context_window,omitempty"`
	Time          time.Time `json:"time"`
}

// Bus is a simple broadcast event bus. Subscribers receive events on buffered
// channels; slow subscribers drop events rather than blocking the agent.
type Bus struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
}

// NewBus creates an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe registers a listener and returns the channel plus a cancel func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, 512)
	b.subs[id] = ch
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

// Publish delivers e to every subscriber without blocking.
func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Subscriber is behind; drop to keep the agent responsive.
		}
	}
}
