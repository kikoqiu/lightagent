package utils

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// prepareProfileDir makes a profile directory ready for a launch of our browser.
//
// A browser that was killed — by a fetch that ran out of time, by the agent
// exiting, by a machine that went down — leaves two things behind that spoil the
// next launch: the port file it published, which names a port nobody listens on
// any more, and the "did not end cleanly" marker Chrome keeps in its
// preferences, which makes the next start offer to restore the pages of that
// session. Both are cleared here, right before the browser starts on it, so the
// agent's window always begins on its own blank page instead of asking about a
// session nobody wants back.
//
// An empty directory (the default profile of the user, which is not ours to
// touch) is left alone.
func prepareProfileDir(dir string) {
	if dir == "" {
		return
	}
	clearDevToolsPortFile(dir)
	clearProfileExitState(dir)
}

// clearDevToolsPortFile drops the port a previous browser published in a profile,
// but only when nothing answers on it: a file that names a live endpoint belongs
// to a browser that is still running on the profile (one that was left behind),
// and it is the only thing that tells where that browser is. A file naming a dead
// port is a leftover of a browser that was killed, and keeping it only risks the
// next launch reading the dead port before the new browser writes its own (see
// waitForDevTools).
func clearDevToolsPortFile(dir string) {
	address, err := readActivePort(dir)
	if err != nil {
		// No usable file: removing it cannot lose anything.
		_ = os.Remove(ActivePortFile(dir))
		return
	}
	if probePort(context.Background(), address) == nil {
		return
	}
	_ = os.Remove(ActivePortFile(dir))
}

// clearProfileExitState rewrites the last-exit marker of a profile to "ended
// cleanly", so Chrome does not offer to restore the session of a browser that was
// killed. The rest of the preferences file is left as it is, and a file that
// cannot be read or parsed is left alone too: Chrome rebuilds a broken one
// itself, and a profile without preferences has nothing to restore.
func clearProfileExitState(dir string) {
	path := filepath.Join(dir, "Default", "Preferences")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	profile, _ := doc["profile"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
	}
	if profile["exit_type"] == "Normal" && profile["exited_cleanly"] == true {
		return
	}
	profile["exit_type"] = "Normal"
	profile["exited_cleanly"] = true
	doc["profile"] = profile
	patched, err := json.Marshal(doc)
	if err != nil {
		return
	}
	_ = replaceFile(path, patched)
}

// replaceFile writes data over path in one step, so a reader never sees a
// half-written file (Chrome reads its preferences while it starts).
func replaceFile(path string, data []byte) error {
	tmp := path + ".lightagent-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
