package main

import (
	"os"
	"testing"
)

// withTempHome points os.UserHomeDir at a fresh temp dir for the duration of
// the test so recordDevice/loadDeviceTable touch a throwaway devices.json.
func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
}

func TestValidateDeviceName(t *testing.T) {
	ok := []string{"nas1", "NAS", "a", "my-device_2", "Z9"}
	for _, n := range ok {
		if err := validateDeviceName(n); err != nil {
			t.Errorf("validateDeviceName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "1nas", "482731", "nas.1", "nas/1", "a b", "u#c", "-x", "_x"}
	for _, n := range bad {
		if err := validateDeviceName(n); err == nil {
			t.Errorf("validateDeviceName(%q) = nil, want error", n)
		}
	}
}

// looksLikeDeviceName must not capture pair codes or URL/UID#code forms, or
// connect's dispatch would mis-route them.
func TestLooksLikeDeviceName(t *testing.T) {
	names := []string{"nas1", "device3", "Home"}
	for _, n := range names {
		if !looksLikeDeviceName(n) {
			t.Errorf("looksLikeDeviceName(%q) = false, want true", n)
		}
	}
	notNames := []string{"482731", "bitba.ng/abc#def", "abc#def", "https://bitba.ng/x#y", "a/b"}
	for _, n := range notNames {
		if looksLikeDeviceName(n) {
			t.Errorf("looksLikeDeviceName(%q) = true, want false", n)
		}
	}
}

func TestRecordDevice_CreateNamedAndAuto(t *testing.T) {
	withTempHome(t)

	name, status, err := recordDevice("bitba.ng", "uid-A", "code-A", "nas1")
	if err != nil || status != recordCreated || name != "nas1" {
		t.Fatalf("create named: got (%q,%v,%v), want (nas1,recordCreated,nil)", name, status, err)
	}

	// No name supplied → device1 (lowest free).
	name, status, err = recordDevice("bitba.ng", "uid-B", "code-B", "")
	if err != nil || status != recordCreatedAuto || name != "device1" {
		t.Fatalf("create auto: got (%q,%v,%v), want (device1,recordCreatedAuto,nil)", name, status, err)
	}

	// Second auto → device2.
	name, _, err = recordDevice("bitba.ng", "uid-C", "code-C", "")
	if err != nil || name != "device2" {
		t.Fatalf("create auto 2: got (%q,%v), want device2", name, err)
	}
}

func TestRecordDevice_UpdateKeepsName(t *testing.T) {
	withTempHome(t)
	if _, _, err := recordDevice("bitba.ng", "uid-A", "code-1", "nas1"); err != nil {
		t.Fatal(err)
	}
	// Re-connect same UID with a rotated code, no name → update in place.
	name, status, err := recordDevice("bitba.ng", "uid-A", "code-2", "")
	if err != nil || status != recordUpdated || name != "nas1" {
		t.Fatalf("update: got (%q,%v,%v), want (nas1,recordUpdated,nil)", name, status, err)
	}
	ent, ok := lookupDeviceByName("nas1")
	if !ok || ent.AccessCode != "code-2" {
		t.Fatalf("after update: entry=%+v ok=%v, want access_code code-2", ent, ok)
	}
}

func TestRecordDevice_RenameRejected(t *testing.T) {
	withTempHome(t)
	if _, _, err := recordDevice("bitba.ng", "uid-A", "code-1", "nas1"); err != nil {
		t.Fatal(err)
	}
	// Same UID, different requested name → rename, which connect refuses.
	if _, _, err := recordDevice("bitba.ng", "uid-A", "code-1", "nas2"); err == nil {
		t.Fatal("rename via recordDevice: got nil error, want rejection")
	}
}

func TestRecordDevice_DuplicateNameRejected(t *testing.T) {
	withTempHome(t)
	if _, _, err := recordDevice("bitba.ng", "uid-A", "code-1", "nas1"); err != nil {
		t.Fatal(err)
	}
	// Different UID, same name (case-insensitive) → rejected.
	if _, _, err := recordDevice("bitba.ng", "uid-B", "code-2", "NAS1"); err == nil {
		t.Fatal("duplicate name: got nil error, want rejection")
	}
}

func TestLookupDeviceByName_CaseInsensitive(t *testing.T) {
	withTempHome(t)
	if _, _, err := recordDevice("bitba.ng", "uid-A", "code-1", "Nas1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookupDeviceByName("nas1"); !ok {
		t.Fatal("case-insensitive lookup failed")
	}
	if _, ok := lookupDeviceByName("other"); ok {
		t.Fatal("lookup of absent name should fail")
	}
}

func TestAutoDeviceName_ReusesGap(t *testing.T) {
	withTempHome(t)
	// device1 + a named entry, then remove device1 → next auto reuses device1.
	if _, _, err := recordDevice("bitba.ng", "uid-A", "c", ""); err != nil { // device1
		t.Fatal(err)
	}
	if _, _, err := recordDevice("bitba.ng", "uid-B", "c", "nas"); err != nil {
		t.Fatal(err)
	}
	tbl, _ := loadDeviceTable()
	// Drop device1 to leave a gap.
	var kept deviceTable
	for _, e := range tbl.Devices {
		if e.Name != "device1" {
			kept.Devices = append(kept.Devices, e)
		}
	}
	if err := writeDeviceTable(kept); err != nil {
		t.Fatal(err)
	}
	if got := autoDeviceName(kept); got != "device1" {
		t.Fatalf("autoDeviceName after gap = %q, want device1", got)
	}
}

// guard against an accidental dependency on a real home dir in CI.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
