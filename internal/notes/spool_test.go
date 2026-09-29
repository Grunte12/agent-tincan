package notes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvanhorn/agent-tincan/internal/envelope"
)

func TestSpoolRoundTrip(t *testing.T) {
	sp := &Spool{Dir: filepath.Join(t.TempDir(), "spool")}
	e := SpoolEntry{
		Request:   envelope.Request{ID: "abc123", From: "grokbot", TraceID: "t1", Body: "note: {}"},
		Note:      Request{Op: OpAdd, Title: "Tent", Body: "blue", Tags: []string{"camping"}},
		SpooledAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := sp.Put(e); err != nil {
		t.Fatal(err)
	}
	got, ok, err := sp.Get("abc123")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if got.Request.ID != "abc123" || got.Note.Title != "Tent" || got.Request.TraceID != "t1" || !got.SpooledAt.Equal(e.SpooledAt) {
		t.Fatalf("got %+v", got)
	}
	info, err := os.Stat(filepath.Join(sp.Dir, "abc123.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	if dinfo, _ := os.Stat(sp.Dir); dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dinfo.Mode().Perm())
	}

	e.Attempts = 3
	if err := sp.Put(e); err != nil {
		t.Fatal(err)
	}
	all, err := sp.List()
	if err != nil || len(all) != 1 || all[0].Attempts != 3 {
		t.Fatalf("List = %+v, %v", all, err)
	}
	ents, _ := os.ReadDir(sp.Dir)
	if len(ents) != 1 {
		t.Fatalf("spool dir holds %d files, want no temp files left", len(ents))
	}
	if err := sp.Remove("abc123"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := sp.Get("abc123"); ok || err != nil {
		t.Fatalf("after Remove: %v, %v", ok, err)
	}
	if err := sp.Remove("abc123"); err != nil {
		t.Fatalf("removing a missing entry: %v", err)
	}
}

func TestSpoolRejectsUnsafeIDs(t *testing.T) {
	sp := &Spool{Dir: t.TempDir()}
	for _, id := range []string{"", "../x", "a/b", ".hidden", strings.Repeat("a", 200)} {
		if err := sp.Put(SpoolEntry{Request: envelope.Request{ID: id}}); err == nil {
			t.Errorf("Put(%q) succeeded", id)
		}
	}
}

// A corrupt entry is reported but does not hide the good ones.
func TestSpoolListSkipsCorruptEntries(t *testing.T) {
	sp := &Spool{Dir: t.TempDir()}
	if err := sp.Put(SpoolEntry{Request: envelope.Request{ID: "good1"}, Note: Request{Op: OpAdd, Title: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp.Dir, "bad1.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp.Dir, ".tmp-leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := sp.List()
	if err == nil || !strings.Contains(err.Error(), "bad1") {
		t.Fatalf("err = %v, want the corrupt entry named", err)
	}
	if len(all) != 1 || all[0].Request.ID != "good1" {
		t.Fatalf("List = %+v", all)
	}
}

func TestSpoolListOfMissingDirIsEmpty(t *testing.T) {
	sp := &Spool{Dir: filepath.Join(t.TempDir(), "none")}
	all, err := sp.List()
	if err != nil || len(all) != 0 {
		t.Fatalf("List = %+v, %v", all, err)
	}
}
