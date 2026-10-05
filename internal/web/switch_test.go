package web

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"lightagent/internal/config"
)

// apiInfosFixture is the interface list the switch tests wire into the mirror.
func apiInfosFixture() []config.APIInfo {
	return []config.APIInfo{
		{Index: 1, Name: "default", Model: "gpt-4o-mini", Enabled: true, Active: true},
		{Index: 2, Name: "backup", Model: "m2", Enabled: true},
	}
}

// drainHistory reads and discards the snapshot frames a fresh connection gets.
func drainHistory(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read history snapshot: %v", err)
		}
		if strings.Contains(string(payload), `"type":"history_end"`) {
			return
		}
	}
}

// TestHistoryFrameCarriesNameAndAPIs pins that the snapshot header tells the
// page which provider is active and which interfaces exist, so the line under
// the logo and the dropdown are right before any settings frame arrives.
func TestHistoryFrameCarriesNameAndAPIs(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAPIList(apiInfosFixture)
	_, reader := dialWS(t, srv)

	var header struct {
		Type string           `json:"type"`
		Name string           `json:"name"`
		APIs []config.APIInfo `json:"apis"`
	}
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		s := string(payload)
		if strings.Contains(s, `"type":"history_end"`) {
			t.Fatal("no history_start frame arrived")
		}
		if !strings.Contains(s, `"type":"history_start"`) {
			continue
		}
		if err := json.Unmarshal(payload, &header); err != nil {
			t.Fatalf("unmarshal header: %v (payload %s)", err, payload)
		}
		break
	}
	if header.Name != "default" {
		t.Fatalf("name = %q, want the active provider's name", header.Name)
	}
	if len(header.APIs) != 2 || header.APIs[0].Name != "default" || !header.APIs[0].Active {
		t.Fatalf("apis = %+v", header.APIs)
	}
}

// TestSwitchAPIOverSocket drives the page's own path: the socket gets a
// /switchapi line and the switcher's confirmation comes back as an info row,
// with no turn started.
func TestSwitchAPIOverSocket(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAPIList(apiInfosFixture)
	srv.SetAPISwitcher(func(spec string) (string, error) {
		if spec != "2" {
			return "", io.ErrUnexpectedEOF
		}
		return `switched to "backup" (m2)`, nil
	})
	conn, reader := dialWS(t, srv)
	drainHistory(t, reader)

	if err := writeMaskedFrame(conn, opText, []byte(`{"text":"/switchapi 2"}`)); err != nil {
		t.Fatal(err)
	}
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the answer to /switchapi: %v", err)
		}
		s := string(payload)
		if strings.Contains(s, `"type":"history_`) {
			continue
		}
		if !strings.Contains(s, "switched to") {
			t.Fatalf("frame = %s, want the switch confirmation", payload)
		}
		break
	}
	if srv.agent.Busy() {
		t.Fatal("/switchapi over the socket must not start a turn")
	}
	if len(srv.agent.History()) != 0 {
		t.Fatal("/switchapi must not reach the model")
	}
}

// TestIndexInjectsNameAndAPIs pins that the page carries the active provider
// name (the line under the logo) and the interface list (the dropdown) before
// the socket even opens.
func TestIndexInjectsNameAndAPIs(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAPIList(apiInfosFixture)
	resp, err := http.Get(baseURL(srv) + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	if !strings.Contains(page, `name: "default"`) {
		t.Fatal("the page carries no active provider name")
	}
	if !strings.Contains(page, `"name":"default"`) {
		t.Fatal("the page carries no provider list")
	}
	if strings.Contains(page, "__LIGHTAGENT_NAME__") || strings.Contains(page, "__LIGHTAGENT_APIS__") {
		t.Fatal("the name/apis placeholders were left unsubstituted")
	}
}

// TestSettingsFrameCarriesNameAndAPIs pins the runtime broadcast: a switch
// pushes a settings frame carrying the new provider name and the list, which is
// what updates every open page.
func TestSettingsFrameCarriesNameAndAPIs(t *testing.T) {
	srv := newTestServer(t, "")
	srv.SetAPIList(apiInfosFixture)
	conn, reader := dialWS(t, srv)
	drainHistory(t, reader)

	srv.BroadcastAPI()
	for {
		_, payload, err := readServerFrame(reader)
		if err != nil {
			t.Fatalf("read the settings frame: %v", err)
		}
		s := string(payload)
		if strings.Contains(s, `"type":"history_`) {
			continue
		}
		if !strings.Contains(s, `"type":"settings"`) {
			continue
		}
		if !strings.Contains(s, `"name":"default"`) {
			t.Fatalf("settings frame = %s, want the provider name", payload)
		}
		if !strings.Contains(s, `"apis":[`) || !strings.Contains(s, `"name":"backup"`) {
			t.Fatalf("settings frame = %s, want the interface list", payload)
		}
		break
	}
	_ = conn
}
