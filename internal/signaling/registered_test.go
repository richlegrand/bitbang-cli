package signaling

import (
	"encoding/json"
	"testing"
)

// decode mirrors what ReadJSON hands applyRegistered: an untyped map,
// where every value is whatever encoding/json chose. Writing the tests
// against real JSON rather than a hand-built map[string]interface{} is
// the point -- the bug this guards against is a type assertion that
// looks right and never fires.
func decode(t *testing.T, s string) Message {
	t.Helper()
	var m Message
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApplyRegistered(t *testing.T) {
	t.Run("code and versions", func(t *testing.T) {
		c := &Client{}
		c.applyRegistered(decode(t, `{"type":"registered","code":"123456",
			"versions":{"cli":"0.4.8","octoprint":"0.2.11"}}`))
		if c.PairingCode != "123456" {
			t.Errorf("code = %q", c.PairingCode)
		}
		if c.LatestVersions["cli"] != "0.4.8" || c.LatestVersions["octoprint"] != "0.2.11" {
			t.Errorf("versions = %v", c.LatestVersions)
		}
	})

	// An older server, or one tracking nothing, omits the field.
	t.Run("no versions field", func(t *testing.T) {
		c := &Client{}
		c.applyRegistered(decode(t, `{"type":"registered","code":"123456"}`))
		if c.LatestVersions != nil {
			t.Errorf("got %v, want nil", c.LatestVersions)
		}
	})

	// A reconnect to a server that stopped tracking must not leave the
	// previous table in place.
	t.Run("reconnect clears a stale table", func(t *testing.T) {
		c := &Client{LatestVersions: map[string]string{"cli": "0.4.8"}, PairingCode: "999999"}
		c.applyRegistered(decode(t, `{"type":"registered"}`))
		if c.LatestVersions != nil {
			t.Errorf("versions = %v, want nil", c.LatestVersions)
		}
		if c.PairingCode != "" {
			t.Errorf("code = %q, want empty", c.PairingCode)
		}
	})

	t.Run("empty table reads as nil", func(t *testing.T) {
		c := &Client{}
		c.applyRegistered(decode(t, `{"type":"registered","versions":{}}`))
		if c.LatestVersions != nil {
			t.Errorf("got %v, want nil", c.LatestVersions)
		}
	})

	// Junk in the table must not take the good rows down with it.
	t.Run("non-string values are skipped", func(t *testing.T) {
		c := &Client{}
		c.applyRegistered(decode(t, `{"type":"registered",
			"versions":{"cli":"0.4.8","weird":42,"nested":{"a":"b"},"null":null}}`))
		if c.LatestVersions["cli"] != "0.4.8" {
			t.Errorf("lost the good row: %v", c.LatestVersions)
		}
		if len(c.LatestVersions) != 1 {
			t.Errorf("kept junk: %v", c.LatestVersions)
		}
	})

	t.Run("wrong type for versions is ignored", func(t *testing.T) {
		c := &Client{}
		c.applyRegistered(decode(t, `{"type":"registered","versions":"0.4.8"}`))
		if c.LatestVersions != nil {
			t.Errorf("got %v, want nil", c.LatestVersions)
		}
	})
}
