package utils

import (
	"path/filepath"
	"strings"
	"testing"
)

// launchArgIndex returns where a switch sits on a command line (-1 when it is
// not there): tests use it to tell a switch we pass from one the caller passes,
// and to pin which of the two comes first.
func launchArgIndex(args []string, want string) int {
	for index, arg := range args {
		if arg == want {
			return index
		}
	}
	return -1
}

// TestLaunchArgsHeadfulLooksLikeAnOrdinaryBrowser pins what a launch with a
// window is started with: the switches that only silence prompts, and nothing
// else. Everything of the automation set is left to a headless launch — the page
// rendered in that window is read by the site's own scripts, and the switches
// that give an automation setup away are what they look for. Two switches that
// such a setup usually carries are refused here for a visible reason: Chrome
// answers them with a bar across the window (see browserLaunchArgs).
func TestLaunchArgsHeadfulLooksLikeAnOrdinaryBrowser(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	extra := "--no-sandbox"
	args := browserLaunchArgs(profile, 9333, BrowserOptions{Headful: true, ExtraArgs: []string{extra}})

	if last := args[len(args)-1]; last != "about:blank" {
		t.Errorf("the launch ends with %q, want the defined first tab", last)
	}
	allowed := map[string]bool{
		"--remote-debugging-port=9333": true,
		"--user-data-dir=" + profile:   true,
		extra:                          true,
		"about:blank":                  true,
	}
	for _, arg := range quietLaunchArgs() {
		allowed[arg] = true
	}
	for _, arg := range args {
		if !allowed[arg] {
			t.Errorf("headful launch args %v carry %q, which a page rendered there can read as automation", args, arg)
		}
	}
	for _, arg := range headlessLaunchArgs() {
		if launchArgIndex(args, arg) >= 0 {
			t.Errorf("headful launch args %v carry %q, want it left to a headless launch", args, arg)
		}
	}
	for _, banned := range []string{"--disable-blink-features=AutomationControlled"} {
		if launchArgIndex(args, banned) >= 0 {
			t.Errorf("headful launch args %v carry %q, which Chrome answers with a bar across the window", args, banned)
		}
	}
	if launchArgIndex(args, "--no-first-run") < 0 {
		t.Errorf("headful launch args %v do not keep Chrome's first-run sign-in out of the way", args)
	}
}

// TestLaunchArgsKeepTheCallerArgumentsAfterOurs pins the order of the switches:
// the caller's own arguments follow ours, so a switch Chrome reads once is the
// caller's to decide. It also pins that a launch on the default profile of the
// user names no directory — that profile is not ours to name.
func TestLaunchArgsKeepTheCallerArgumentsAfterOurs(t *testing.T) {
	theirs := "--window-size=800,600"
	args := browserLaunchArgs("", 9333, BrowserOptions{Headful: true, ExtraArgs: []string{theirs}})

	ours := launchArgIndex(args, "--remote-debugging-port=9333")
	if theirsAt := launchArgIndex(args, theirs); ours < 0 || theirsAt <= ours {
		t.Errorf("launch args %v: the caller's switch (at %d) must follow ours (at %d)", args, theirsAt, ours)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--user-data-dir=") {
			t.Errorf("launch args %v name a profile although the options name none", args)
		}
	}
}

// TestLaunchArgsHeadlessKeepsTheAutomationSet pins that the invisible launch is
// untouched by the stealth work: it keeps the switches that make an unattended
// run quiet and light, and it takes nothing that belongs to a window — a headless
// browser is recognized as one whatever the command line says.
func TestLaunchArgsHeadlessKeepsTheAutomationSet(t *testing.T) {
	args := browserLaunchArgs("", 9333, BrowserOptions{})

	allowed := map[string]bool{"--remote-debugging-port=9333": true, "about:blank": true}
	for _, arg := range append(quietLaunchArgs(), headlessLaunchArgs()...) {
		allowed[arg] = true
	}
	for _, arg := range args {
		if !allowed[arg] {
			t.Errorf("headless launch args %v carry %q, which belongs to a launch with a window", args, arg)
		}
	}
	for _, want := range []string{
		"--headless=new",
		"--window-size=1280,900",
		"--disable-extensions",
		"--disable-background-networking",
		"--mute-audio",
	} {
		if launchArgIndex(args, want) < 0 {
			t.Errorf("headless launch args %v are missing %q", args, want)
		}
	}
}
