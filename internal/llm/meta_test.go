package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMessageMetaStaysInSessionFilesAndOutOfRequests verifies the engine meta is
// its own json field on disk — a session file keeps it, a resume reads it back —
// and never reaches the provider: requestMessages drops it, leaving the message
// content the model reads untouched.
func TestMessageMetaStaysInSessionFilesAndOutOfRequests(t *testing.T) {
	msg := Message{Role: "user", Content: "question", Meta: "[engine meta info]\nlisting"}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stored["meta"] != msg.Meta {
		t.Fatalf("session json = %s, want the meta kept", data)
	}
	var restored Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal back: %v", err)
	}
	if restored.Meta != msg.Meta || restored.Content != msg.Content {
		t.Fatalf("restored = %+v, want the meta and content back", restored)
	}

	wire, err := json.Marshal(requestMessages([]Message{msg}))
	if err != nil {
		t.Fatalf("marshal wire: %v", err)
	}
	if strings.Contains(string(wire), "engine meta info") || strings.Contains(string(wire), `"meta"`) {
		t.Fatalf("the provider must not see the engine meta: %s", wire)
	}
	if msg.Meta == "" || msg.Content != "question" {
		t.Fatalf("requestMessages modified its input: %+v", msg)
	}
}
