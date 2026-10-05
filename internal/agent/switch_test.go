package agent

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lightagent/internal/config"
	"lightagent/internal/llm"
	"lightagent/internal/tools"
)

// TestSwitchLLMSwapsTheClient pins the lightweight runtime switch: the client and
// the numbers derived from the interface (context window, output reserve) are
// replaced without rebuilding anything else.
func TestSwitchLLMSwapsTheClient(t *testing.T) {
	cfg := config.Default()
	a := New(cfg, llm.NewClient(config.OpenAIConfig{Model: "mA"}), tools.NewRegistry(), NewBus())
	if a.Model() != "mA" {
		t.Fatalf("model = %q, want mA", a.Model())
	}
	next := llm.NewClient(config.OpenAIConfig{Model: "mB"})
	if err := a.SwitchLLM(next, 5000, 123); err != nil {
		t.Fatalf("SwitchLLM: %v", err)
	}
	if a.Model() != "mB" {
		t.Fatalf("model = %q, want mB after the switch", a.Model())
	}
	if got := a.Stats().ContextWindow; got != 5000 {
		t.Fatalf("context window = %d, want 5000", got)
	}
}

// TestSwitchLLMRefusesWhileBusy pins that a switch never lands in the middle of a
// turn: it is refused while one runs and accepted once it has ended.
func TestSwitchLLMRefusesWhileBusy(t *testing.T) {
	requested := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(requested) })
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	cfg := config.Default()
	cfg.LLMs[0].APIBase = srv.URL
	bus := NewBus()
	events, cancel := bus.Subscribe()
	defer cancel()
	a := New(cfg, llm.NewClient(cfg.LLMs[0].OpenAIConfig), tools.NewRegistry(), bus)

	a.Submit("go")
	select {
	case <-requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the model call never started")
	}
	if err := a.SwitchLLM(llm.NewClient(config.OpenAIConfig{Model: "mB"}), 0, 0); err == nil {
		t.Fatal("SwitchLLM must be refused while a turn runs")
	}
	if !a.Interrupt() {
		t.Fatal("Interrupt reported no running turn")
	}
	if !waitForTurnEnd(t, events) {
		t.Fatal("no interrupted event was published")
	}
	if err := a.SwitchLLM(llm.NewClient(config.OpenAIConfig{Model: "mB"}), 0, 0); err != nil {
		t.Fatalf("SwitchLLM after the turn: %v", err)
	}
	if a.Model() != "mB" {
		t.Fatalf("model = %q, want mB", a.Model())
	}
}
