package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestReadAloudScriptIsServed checks the read-aloud script is embedded and served
// like the rest of the UI: it carries no data, so it is public, and it must not
// ask the server for anything (the switches are browser-local).
func TestReadAloudScriptIsServed(t *testing.T) {
	srv := newTestServer(t, "secret")
	resp, err := http.Get(baseURL(srv) + "/tts.js")
	if err != nil {
		t.Fatalf("GET /tts.js: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /tts.js: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/tts.js status = %d, want 200 (the UI is public)", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("/tts.js served an empty body")
	}
	if strings.Contains(string(body), "/api/") {
		t.Error("the read-aloud switches are browser-local: tts.js must not call the server")
	}
	// The served page must carry both the panel and the script tag (the page is
	// assembled by the server, so this also covers the injection path).
	resp, err = http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	page, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /: %v", readErr)
	}
	for _, want := range []string{`id="ttsModal"`, `src="tts.js"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("the served page is missing %q", want)
		}
	}
}

// TestReadAloudPanelWiring pins the pieces the feature is made of: the panel in
// the page (master switch, language picker, the two reading modes and the tick
// boxes), the browser API behind it, and the event feed from the transcript.
func TestReadAloudPanelWiring(t *testing.T) {
	for _, want := range []string{
		"tts.js",                            // the embedded script, loaded by the page
		`id="ttsModal"`,                     // the panel
		`id="ttsPanel"`,                     // its focus target
		`id="ttsOn"`,                        // master switch
		`id="ttsRailOn"`,                    // the rail card's quick switch
		`id="ttsLang"`,                      // language / voice picker
		`name="ttsMode"`,                    // one summary per round, or the ticked rows
		`value="final"`,                     // the round's final text
		`value="custom"`,                    // the ticked rows
		`id="ttsThinking"`,                  // thinking process
		`id="ttsTools"`,                     // tool calls (tool name)
		`id="ttsText"`,                      // text feedback
		"data-tts-open",                     // header / rail entry point
		"on: false",                         // off by default
		"if (!synth) { state.on = false; }", // an unsupported browser never reads
		"speechSynthesis",                   // the browser TTS API
		"SpeechSynthesisUtterance",          // one utterance per chunk
		"localStorage",                      // remembered per browser
		"window.TTS",                        // what app.js feeds
		"if (!replaying) { TTS.event(kind, ev); }", // the live feed, replay excluded
		"TTS.reset()",     // a rebuilt log silences the voice
		"turn_done",       // when the round's summary is read
		"reasoning_delta", // what the thinking buffer follows
	} {
		if !strings.Contains(pageSource(), want) {
			t.Errorf("the read-aloud feature is missing %q", want)
		}
	}
}
