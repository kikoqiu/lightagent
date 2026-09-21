package utils

import (
	"encoding/json"
	"testing"
	"time"
)

// TestPageHidesTheAutomationFlag pins the script every page gets before it loads
// (see automationFlagScript): a browser that a protocol client drives answers
// navigator.webdriver with true, and a page must not be able to read that. The
// test also pins the two things that would give the patch away — the property has
// to stay where the browser defines it, with the shape and the source text of the
// native getter.
func TestPageHidesTheAutomationFlag(t *testing.T) {
	requireBrowser(t)
	browser, err := LaunchBrowser(testContext(t), BrowserOptions{
		UserDataDir: testProfileDir(t),
		KeepAlive:   -1,
	})
	if err != nil {
		t.Fatalf("LaunchBrowser: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	page, err := browser.NewPage(testContext(t))
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	defer closePage(page)

	// The document writes the flag into its own markup while it loads, so what is
	// read here is what a script of the page itself would read.
	const document = `data:text/html,<html><body><script>` +
		`document.documentElement.setAttribute('data-webdriver', String(navigator.webdriver))` +
		`</script>loaded</body></html>`
	if _, err := page.Navigate(testContext(t), document, 20*time.Second, 0); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	value, err := page.evaluateString(testContext(t), "document.documentElement.getAttribute('data-webdriver')")
	if err != nil {
		t.Fatalf("read the flag the page saw: %v", err)
	}
	if value != "false" {
		t.Errorf("navigator.webdriver as the page read it = %q, want \"false\"", value)
	}

	shape, err := page.evaluateString(testContext(t), `JSON.stringify((() => {
		const descriptor = Object.getOwnPropertyDescriptor(Navigator.prototype, 'webdriver');
		return {
			value: navigator.webdriver,
			name: descriptor.get.name,
			directSource: descriptor.get.toString(),
			calledSource: Function.prototype.toString.call(descriptor.get),
			enumerable: descriptor.enumerable,
			configurable: descriptor.configurable,
			setter: typeof descriptor.set,
			toStringSource: Function.prototype.toString.call(Function.prototype.toString),
			otherSource: Function.prototype.toString.call(Array.prototype.push)
		};
	})())`)
	if err != nil {
		t.Fatalf("read the accessor: %v", err)
	}
	var accessor struct {
		Value          bool   `json:"value"`
		Name           string `json:"name"`
		DirectSource   string `json:"directSource"`
		CalledSource   string `json:"calledSource"`
		Enumerable     bool   `json:"enumerable"`
		Configurable   bool   `json:"configurable"`
		Setter         string `json:"setter"`
		ToStringSource string `json:"toStringSource"`
		OtherSource    string `json:"otherSource"`
	}
	if err := json.Unmarshal([]byte(shape), &accessor); err != nil {
		t.Fatalf("the accessor is not the shape expected: %v (%s)", err, shape)
	}
	const nativeGetter = "function get webdriver() { [native code] }"
	if accessor.Value {
		t.Errorf("navigator.webdriver = true, want the page to be told otherwise")
	}
	if accessor.Name != "get webdriver" {
		t.Errorf("the getter is named %q, want the name of the native one", accessor.Name)
	}
	if accessor.DirectSource != nativeGetter || accessor.CalledSource != nativeGetter {
		t.Errorf("the getter reads as %q / %q, want %q", accessor.DirectSource, accessor.CalledSource, nativeGetter)
	}
	if !accessor.Enumerable || !accessor.Configurable {
		t.Errorf("the descriptor = {enumerable: %v, configurable: %v}, want the shape of the native one",
			accessor.Enumerable, accessor.Configurable)
	}
	if accessor.Setter != "undefined" {
		t.Errorf("the accessor has a setter (%s), the native one does not", accessor.Setter)
	}
	if accessor.ToStringSource != "function toString() { [native code] }" {
		t.Errorf("Function.prototype.toString reads as %q, want it to look native", accessor.ToStringSource)
	}
	if accessor.OtherSource != "function push() { [native code] }" {
		t.Errorf("another function reads as %q, want the trap to pass everything else through", accessor.OtherSource)
	}
}
