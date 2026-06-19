package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The known-hosts table lives at ~/.bitbang/devices.json and remembers every
// device we've successfully connected to — whether reached by pair code or by
// URL. Each remembered host carries a human name so the operator can reconnect
// with `bitbang connect <name>` instead of re-typing a code or URL.
//
// The name is the only user-facing identifier; the UID is the stable internal
// key (it's hash(pubkey), so it survives access-code rotation and re-pairing).
// Names are therefore a unique secondary index over a UID-keyed table.

// deviceEntry is one remembered host.
type deviceEntry struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	AccessCode string `json:"access_code"`
	Server     string `json:"server"`
	PairedAt   string `json:"paired_at"`
}

// deviceTable is the on-disk shape of devices.json.
type deviceTable struct {
	Devices []deviceEntry `json:"devices"`
}

// deviceNamePattern constrains names so a name can never be mistaken for one of
// the other two `connect` arg shapes. Starting with a letter rules out the
// 6-digit pair code; excluding '.', ':', '/', '#', '@' rules out any URL or
// UID#code form. This is the single rule that keeps connect's three-way
// dispatch (name | code | URL) unambiguous, so it is enforced at save time.
var deviceNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// autoNameRoot is the stem for names assigned when the operator doesn't pass
// -name: device1, device2, ... The lowest free index is chosen so removals
// don't leave permanent gaps.
const autoNameRoot = "device"

// recordStatus reports what recordDevice did, so the caller can print the right
// line (and only nudge about -name when it actually auto-assigned).
type recordStatus int

const (
	recordUpdated     recordStatus = iota // UID already known; refreshed in place
	recordCreated                         // new host saved under an operator-chosen name
	recordCreatedAuto                     // new host saved under an auto-assigned device<N>
)

// validateDeviceName returns an error if name can't be used as a device name.
// Empty is rejected here; callers that allow "no name given" check for "" first
// and let recordDevice auto-assign.
func validateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("name is empty")
	}
	if !deviceNamePattern.MatchString(name) {
		return fmt.Errorf("name %q is invalid: must start with a letter and contain only letters, digits, '-' or '_'", name)
	}
	return nil
}

// looksLikeDeviceName reports whether arg is shaped like a bare device name
// (and therefore should be resolved against the table rather than parsed as a
// URL or pair code). It is a syntax test only — it does not check the table.
func looksLikeDeviceName(arg string) bool {
	return deviceNamePattern.MatchString(arg)
}

// bitbangDir returns ~/.bitbang. The device table is shared across all program
// names, so it does not live under identity's per-program directory.
func bitbangDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return home + "/.bitbang", nil
}

func devicesPath() (string, error) {
	dir, err := bitbangDir()
	if err != nil {
		return "", err
	}
	return dir + "/devices.json", nil
}

// loadDeviceTable reads devices.json. A missing file yields an empty table; a
// corrupt file is treated as empty too (best-effort — the table is a
// convenience cache, not a source of truth, so resetting it is acceptable).
func loadDeviceTable() (deviceTable, error) {
	path, err := devicesPath()
	if err != nil {
		return deviceTable{}, err
	}
	var t deviceTable
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return deviceTable{}, nil
		}
		return deviceTable{}, fmt.Errorf("read %s: %w", path, err)
	}
	_ = json.Unmarshal(data, &t) // corrupt file → empty table
	return t, nil
}

// writeDeviceTable writes the table atomically (tmp + rename) with 0600 perms.
func writeDeviceTable(t deviceTable) error {
	dir, err := bitbangDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := dir + "/devices.json"
	out, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// lookupDeviceByName returns the entry whose name matches (case-insensitively),
// if any. Names are stored as typed but matched case-insensitively so "NAS1"
// and "nas1" resolve to the same host (and can't both exist — see recordDevice).
func lookupDeviceByName(name string) (deviceEntry, bool) {
	t, err := loadDeviceTable()
	if err != nil {
		return deviceEntry{}, false
	}
	for _, e := range t.Devices {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return deviceEntry{}, false
}

// nameTaken reports whether some entry other than the one at exceptIdx already
// uses name (case-insensitive). exceptIdx of -1 checks all entries.
func nameTaken(t deviceTable, name string, exceptIdx int) bool {
	for i, e := range t.Devices {
		if i == exceptIdx {
			continue
		}
		if strings.EqualFold(e.Name, name) {
			return true
		}
	}
	return false
}

// autoDeviceName returns the lowest-numbered device<N> (N>=1) not already in
// use (case-insensitive), so gaps left by removed entries are reused.
func autoDeviceName(t deviceTable) string {
	for n := 1; ; n++ {
		candidate := autoNameRoot + strconv.Itoa(n)
		if !nameTaken(t, candidate, -1) {
			return candidate
		}
	}
}

// recordDevice persists a successful connection to the table.
//
//   - If the UID is already known, its access code / server / timestamp are
//     refreshed in place and the existing name is kept (recordUpdated).
//     Passing a requestedName that differs from the stored name is a rename,
//     which connect does not support — that returns an error.
//   - If the UID is new, it's saved under requestedName (validated, must be
//     unique) or, when requestedName is empty, an auto-assigned device<N>.
//
// The returned name is the name the host is now stored under (useful for the
// caller's output regardless of which branch ran).
func recordDevice(server, uid, accessCode, requestedName string) (name string, status recordStatus, err error) {
	t, err := loadDeviceTable()
	if err != nil {
		return "", 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)

	for i := range t.Devices {
		if t.Devices[i].UID != uid {
			continue
		}
		// Known host. Refresh credentials; keep the name.
		if requestedName != "" && !strings.EqualFold(requestedName, t.Devices[i].Name) {
			return "", 0, fmt.Errorf("device already saved as %q; renaming via connect isn't supported", t.Devices[i].Name)
		}
		t.Devices[i].AccessCode = accessCode
		t.Devices[i].Server = server
		t.Devices[i].PairedAt = now
		stored := t.Devices[i].Name
		if err := writeDeviceTable(t); err != nil {
			return "", 0, err
		}
		return stored, recordUpdated, nil
	}

	// New host.
	status = recordCreated
	name = requestedName
	if name == "" {
		name = autoDeviceName(t)
		status = recordCreatedAuto
	} else {
		if err := validateDeviceName(name); err != nil {
			return "", 0, err
		}
		if nameTaken(t, name, -1) {
			return "", 0, fmt.Errorf("name %q is already used by another device", name)
		}
	}
	t.Devices = append(t.Devices, deviceEntry{
		Name:       name,
		UID:        uid,
		AccessCode: accessCode,
		Server:     server,
		PairedAt:   now,
	})
	if err := writeDeviceTable(t); err != nil {
		return "", 0, err
	}
	return name, status, nil
}
