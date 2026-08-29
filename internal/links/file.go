package links

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Filename is the per-program link table, living beside identity.pem.
const Filename = "links.json"

// deferredFields are terms named in the design but not implemented in
// this release. They get their own message rather than the generic
// unknown-field one: someone pastes the full schema, encoding/json would
// silently drop these, and they end up with a link they believe is
// PIN-protected. A control that quietly does nothing is worse than a
// missing feature.
var deferredFields = map[string]string{
	"pin":  `per-link "pin" is not supported in this release (--pin protects the whole listener)`,
	"uses": `per-link "uses" is not supported in this release`,
	"ttl":  `per-link "ttl" is not supported in this release (use "expires" with an absolute time)`,
}

// Parse decodes and validates the file's entries. It does not synthesize
// the implicit `owner` row and does not mint codes; both belong to the
// caller, which knows the identity and what is served.
//
// Unknown fields are refused. Beyond catching the deferred terms above,
// minting re-serializes from the struct, so a field the struct does not
// model would be destroyed on the first write-back -- rejecting it at
// load makes that state unreachable.
func Parse(data []byte) ([]Terms, error) {
	// Decode once loosely to name a deferred field before the decoder
	// reports it as merely unknown.
	var loose []map[string]json.RawMessage
	if err := json.Unmarshal(data, &loose); err != nil {
		return nil, err
	}
	for i, entry := range loose {
		for field := range entry {
			if msg, ok := deferredFields[field]; ok {
				return nil, fmt.Errorf("entry %d: %s", i+1, msg)
			}
		}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var entries []Terms
	if err := dec.Decode(&entries); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the link list")
	}
	return entries, nil
}

// Validate applies the value-level rules the JSON decoder cannot see.
// Duplicate labels are checked by the caller, after the implicit `owner`
// row is synthesized, so a hand-written entry labeled `owner` collides.
func Validate(entries []Terms) error {
	for i, e := range entries {
		n := i + 1
		if strings.TrimSpace(e.Label) == "" {
			return fmt.Errorf("entry %d: every link needs a label", n)
		}
		// The grammar validates itself, and its errors are the ones the
		// command line gives -- so a typo in the file reads the same as a
		// typo at the prompt.
		if _, err := e.Spec(); err != nil {
			return err
		}
	}
	return nil
}

// Load reads and validates the file. A missing file yields no entries and
// no error, which the caller turns into today's behavior: one code,
// everything served. Any other failure is fatal to the caller -- an
// unreadable table must never degrade to "no links", because that is
// defined as granting everything.
//
// The returned modtime is what Save checks against to avoid clobbering an
// edit made while the listener was running.
func Load(path string) ([]Terms, time.Time, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	entries, err := Parse(data)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := Validate(entries); err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: %w", path, err)
	}
	return entries, info.ModTime(), nil
}

// ErrChangedOnDisk means the file moved under us between load and write.
// The listener is the sole writer in the steady state, but `link edit`
// is $EDITOR in another process: without this check, a save from the
// editor after a mint would drop the freshly written code, and the URL
// just printed would stop working.
var ErrChangedOnDisk = errors.New("links.json changed on disk, reload and try again")

// Save writes entries back atomically -- temp file plus rename -- after
// confirming the file still carries the modtime it had at load. Pass a
// zero loadedMod when the file did not exist.
func Save(path string, entries []Terms, loadedMod time.Time) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !loadedMod.IsZero() {
			return ErrChangedOnDisk
		}
	case err != nil:
		return err
	default:
		if !info.ModTime().Equal(loadedMod) {
			return ErrChangedOnDisk
		}
	}

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".links-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
