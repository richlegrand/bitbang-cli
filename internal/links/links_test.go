package links

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// offered is what a full `bitbang serve` supports.
var offered = []string{ScopeFiles, ScopeShell, ScopeForward, ScopeProxy}

func mustBuild(t *testing.T, entries []Terms, srv []string) *Table {
	t.Helper()
	tb, _, err := Build(entries, srv, "IDENTITYCODE")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return tb
}

// -- Parsing --

func TestParse_DeferredFieldsAreNamed(t *testing.T) {
	for _, field := range []string{"pin", "uses", "ttl"} {
		data := []byte(`[{"label":"x","` + field + `":"1"}]`)
		_, err := Parse(data)
		if err == nil {
			t.Fatalf("%q accepted; a deferred term must not look like a live one", field)
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%q: error does not name the field: %v", field, err)
		}
	}
}

func TestParse_UnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte(`[{"label":"x","surfix":"typo"}]`)); err == nil {
		t.Error("unknown field accepted; a typo must not be silently dropped")
	}
}

func TestParse_Good(t *testing.T) {
	entries, err := Parse([]byte(`[
	  {"label":"contractor","scope":["files"],"expires":"2030-01-01T00:00:00Z"},
	  {"label":"kiosk"}
	]`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[1].Scope != nil {
		t.Error("absent scope should stay nil (means everything offered)")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		entry   Terms
		wantErr string
	}{
		{"empty scope", Terms{Label: "x", Scope: []string{}}, "grants nothing"},
		{"unknown scope", Terms{Label: "x", Scope: []string{"shel"}}, "unknown scope"},
		{"no label", Terms{Label: "  "}, "needs a label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]Terms{c.entry})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
	if err := Validate([]Terms{{Label: "ok", Scope: []string{"proxy", "files"}}}); err != nil {
		t.Errorf("valid entry rejected: %v", err)
	}
}

// -- Scope --

func TestGrants_AbsentScopeIsEverythingOffered(t *testing.T) {
	got := Terms{Label: "owner"}.Grants([]string{ScopeFiles, ScopeShell})
	if strings.Join(got, ",") != "files,shell" {
		t.Errorf("got %v, want everything offered", got)
	}
}

func TestGrants_ScopeNotOfferedIsDroppedNotGranted(t *testing.T) {
	got := Terms{Label: "s", Scope: []string{ScopeShell}}.Grants([]string{ScopeFiles})
	if len(got) != 0 {
		t.Errorf("got %v; a shell-scoped link on a files-only listener must conjure nothing", got)
	}
}

func TestBuild_WarnsWhenScopeNotServed(t *testing.T) {
	_, warnings, err := Build([]Terms{{Label: "s", Scope: []string{"shell"}}}, []string{"file"}, "C")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "shell") {
		t.Errorf("got %v, want a warning naming shell", warnings)
	}
}

// -- The implicit `owner` row --

func TestBuild_SynthesizesOwner(t *testing.T) {
	tb := mustBuild(t, nil, offered)
	owner, ok := tb.ByLabel(OwnerLabel)
	if !ok {
		t.Fatal("no owner row; the poll would close the operator's own session")
	}
	if owner.Expires != nil || owner.Scope != nil {
		t.Error("owner must never expire and must grant everything offered")
	}
	if got := owner.Grants(offered); len(got) != len(offered) {
		t.Errorf("owner grants %v, want everything offered", got)
	}
}

func TestBuild_HandWrittenOwnerCollides(t *testing.T) {
	_, _, err := Build([]Terms{{Label: OwnerLabel}}, offered, "C")
	if err == nil {
		t.Fatal("a hand-written `owner` entry must collide with the synthesized row")
	}
}

func TestBuild_DuplicateLabelsRejected(t *testing.T) {
	_, _, err := Build([]Terms{{Label: "a"}, {Label: "a"}}, offered, "C")
	if err == nil {
		t.Fatal("duplicate labels accepted; the label is the poll's lookup key")
	}
}

// -- Resolution --

func TestAuthorize_DefaultCodeGrantsEverything(t *testing.T) {
	tb := mustBuild(t, nil, offered)
	terms, ok := tb.Authorize("IDENTITYCODE", time.Now())
	if !ok {
		t.Fatal("the identity's own code must stay valid")
	}
	if len(terms.Grants(offered)) != len(offered) {
		t.Error("the identity's own code must grant everything offered")
	}
}

func TestAuthorize_ExpiredRefusedLikeUnknown(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	tb := mustBuild(t, []Terms{{Label: "old", Code: "OLDCODE1234", Expires: &past}}, offered)

	expiredTerms, expiredOK := tb.Authorize("OLDCODE1234", time.Now())
	unknownTerms, unknownOK := tb.Authorize("NEVERMINTED", time.Now())

	if expiredOK {
		t.Error("expired code accepted")
	}
	if expiredOK != unknownOK || expiredTerms.Label != unknownTerms.Label {
		t.Error("expired and unknown must be indistinguishable to the caller")
	}
}

func TestAuthorize_UnmintedEntryIsNotAMatch(t *testing.T) {
	tb := mustBuild(t, []Terms{{Label: "pending"}}, offered)
	if _, ok := tb.Authorize("", time.Now()); ok {
		t.Error("an empty presented code matched an unminted entry")
	}
}

func TestCheck_BoundaryIsExclusive(t *testing.T) {
	at := time.Now()
	terms := Terms{Label: "x", Expires: &at}
	if err := terms.Check(at); err == nil {
		t.Error("a link is expired at its expiry instant, not after it")
	}
	if err := terms.Check(at.Add(-time.Second)); err != nil {
		t.Errorf("still valid a second before: %v", err)
	}
}

// -- Handler filtering --

func TestGrants_NarrowingIsDetected(t *testing.T) {
	wide := Terms{Label: "c"}
	narrow := Terms{Label: "c", Scope: []string{ScopeFiles}}
	if SameGrants(wide, narrow, offered) {
		t.Error("a link narrowed from everything to files must not look unchanged")
	}
	if !SameGrants(narrow, Terms{Label: "c", Scope: []string{ScopeFiles}}, offered) {
		t.Error("identical scopes compared unequal")
	}
}

func TestGrantSet_FilesReachesNothingElse(t *testing.T) {
	set := Terms{Label: "c", Scope: []string{ScopeFiles}}.GrantSet(offered)
	if !set[ScopeFiles] {
		t.Error("files not granted")
	}
	for _, name := range []string{ScopeShell, ScopeForward, ScopeProxy} {
		if set[name] {
			t.Errorf("a files-scoped link must not reach %s", name)
		}
	}
}

// -- File I/O --

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	entries, mod, err := Load(filepath.Join(t.TempDir(), "links.json"))
	if err != nil || entries != nil || !mod.IsZero() {
		t.Errorf("missing file: got (%v, %v, %v), want today's behavior", entries, mod, err)
	}
}

func TestLoad_BadJSONIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil {
		t.Fatal("corrupt file accepted; an empty table means everything is offered")
	}
}

func TestSave_RefusesWhenFileChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	if err := os.WriteFile(path, []byte(`[{"label":"a","code":"AAAA"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, mod, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// $EDITOR saves while the listener holds a mint to write back.
	touched := mod.Add(2 * time.Second)
	if err := os.Chtimes(path, touched, touched); err != nil {
		t.Fatal(err)
	}

	err = Save(path, []Terms{{Label: "a", Code: "BBBB"}}, mod)
	if !errors.Is(err, ErrChangedOnDisk) {
		t.Fatalf("got %v, want ErrChangedOnDisk -- the mint would clobber the edit", err)
	}
}

func TestSave_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "links.json")
	want := []Terms{{Label: "a", Code: "AAAA", Scope: []string{"files"}}}
	if err := Save(path, want, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Code != "AAAA" || got[0].Scope[0] != "files" {
		t.Errorf("round trip changed the entry: %+v", got)
	}
}

func TestMint_FillsOnlyMissingCodes(t *testing.T) {
	n := 0
	gen := func() (string, error) { n++; return "MINTED", nil }
	out, minted, err := Mint([]Terms{{Label: "has", Code: "KEEP"}, {Label: "needs"}}, time.Now(), gen)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Code != "KEEP" {
		t.Error("an existing code was overwritten")
	}
	if out[1].Code != "MINTED" || len(minted) != 1 || minted[0] != "needs" {
		t.Errorf("mint did not fill the empty code: %+v %v", out, minted)
	}
}

// An expired code must die rather than sleep: left in place, extending
// the entry brings back the same fragment, so renewing a link for one
// person silently readmits everyone who was ever sent it.
func TestRetireExpired_ClearsTheCodeSoRenewalMintsAFreshOne(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)
	entries := []Terms{
		{Label: "lapsed", Code: "OLDCODE", Expires: &past},
		{Label: "live", Code: "LIVECODE", Expires: &future},
		{Label: "forever", Code: "FOREVER"},
	}

	out, retired := RetireExpired(entries, time.Now())
	if len(retired) != 1 || retired[0] != "lapsed" {
		t.Fatalf("retired = %v, want just the lapsed one", retired)
	}
	if out[0].Code != "" {
		t.Errorf("lapsed entry kept its code %q", out[0].Code)
	}
	if out[1].Code != "LIVECODE" || out[2].Code != "FOREVER" {
		t.Error("retiring touched an entry that had not expired")
	}

	// Still expired, so minting must leave it alone -- otherwise every
	// reload hands a dead link a fresh code and rewrites the file.
	out, minted, err := Mint(out, time.Now(), func() (string, error) { return "NEW", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 0 {
		t.Errorf("minted %v for an entry that is still expired", minted)
	}

	// Renewed: now it is live and codeless, so it gets a code, and a new
	// one -- the URL already handed out stays dead.
	out[0].Expires = &future
	out, minted, err = Mint(out, time.Now(), func() (string, error) { return "NEW", nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(minted) != 1 || out[0].Code != "NEW" {
		t.Fatalf("renewal did not mint a fresh code: minted=%v code=%q", minted, out[0].Code)
	}
	if out[0].Code == "OLDCODE" {
		t.Error("renewal revived the original code")
	}
}

// Two rows sharing a code is copy-paste, not bad luck, and left alone it
// grants a live holder the other row's scope while surviving link rm.
func TestDedupeCodes_ClearsEverySharingRow(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	entries := []Terms{
		{Label: "ana", Code: "SHARED", Scope: []string{ScopeFiles}, Expires: &past},
		{Label: "ben", Code: "SHARED", Scope: []string{ScopeShell}},
		{Label: "cleo", Code: "OWN", Scope: []string{ScopeFiles}},
	}
	out, conflicts := DedupeCodes(entries, "IDENTITY")

	if out[0].Code != "" || out[1].Code != "" {
		t.Errorf("a sharing row kept the code: %q %q", out[0].Code, out[1].Code)
	}
	if out[2].Code != "OWN" {
		t.Error("an unrelated row lost its code")
	}
	if len(conflicts) != 1 || len(conflicts[0].Labels) != 2 {
		t.Fatalf("conflicts = %+v, want one naming both rows", conflicts)
	}
	if conflicts[0].Labels[0] != "ana" || conflicts[0].Labels[1] != "ben" {
		t.Errorf("labels = %v, want both, sorted", conflicts[0].Labels)
	}
	if conflicts[0].Reserved {
		t.Error("reported as an identity-code conflict")
	}
}

// The identity is the one incumbent that is known, so it keeps its code
// and the row yields.
func TestDedupeCodes_IdentityCodeIsReserved(t *testing.T) {
	out, conflicts := DedupeCodes([]Terms{{Label: "sneaky", Code: "IDENTITY"}}, "IDENTITY")
	if out[0].Code != "" {
		t.Error("a row using the device's own code kept it")
	}
	if len(conflicts) != 1 || !conflicts[0].Reserved {
		t.Fatalf("conflicts = %+v, want one marked reserved", conflicts)
	}
}

func TestDedupeCodes_LeavesADistinctTableAlone(t *testing.T) {
	entries := []Terms{{Label: "a", Code: "A"}, {Label: "b", Code: "B"}, {Label: "c"}}
	out, conflicts := DedupeCodes(entries, "IDENTITY")
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %+v on a clean table", conflicts)
	}
	if out[0].Code != "A" || out[1].Code != "B" || out[2].Code != "" {
		t.Errorf("codes changed: %+v", out)
	}
}

// Dedup then retire then mint: the shared code must not survive on any
// row, and each row must come away with its own.
func TestDedupeThenRetireThenMint(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	entries := []Terms{
		{Label: "ana", Code: "SHARED", Expires: &past},
		{Label: "ben", Code: "SHARED"},
	}
	n := 0
	gen := func() (string, error) { n++; return fmt.Sprintf("NEW%d", n), nil }

	out, _ := DedupeCodes(entries, "IDENTITY")
	out, _ = RetireExpired(out, time.Now())
	out, _, err := Mint(out, time.Now(), gen)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Code != "" {
		t.Errorf("expired ana ended up with code %q; she should have none", out[0].Code)
	}
	if out[1].Code == "SHARED" || out[1].Code == "" {
		t.Errorf("ben's code = %q, want a freshly minted one", out[1].Code)
	}
}
