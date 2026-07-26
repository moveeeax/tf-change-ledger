package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return Open(filepath.Join(t.TempDir(), "ledger.jsonl"))
}

func sample(addr, action string) Entry {
	return Entry{
		RecordedAt:   time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC),
		Repo:         "acme/platform",
		PR:           42,
		Commit:       "0bada55deadbeef",
		Author:       "alice",
		Approvers:    []string{"bob"},
		Address:      addr,
		Type:         strings.SplitN(addr, ".", 2)[0],
		Action:       action,
		Protected:    true,
		ProtectedBy:  "databases",
		Acknowledged: true,
	}
}

func TestAppendChainsEntries(t *testing.T) {
	s := newStore(t)
	stored, err := s.Append(sample("aws_db_instance.primary", "destroy"), sample("aws_s3_bucket.logs", "destroy"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 {
		t.Fatalf("got %d entries", len(stored))
	}
	if stored[0].Seq != 1 || stored[1].Seq != 2 {
		t.Fatalf("sequences = %d, %d", stored[0].Seq, stored[1].Seq)
	}
	if stored[0].PrevHash != GenesisHash {
		t.Errorf("first prev_hash = %q", stored[0].PrevHash)
	}
	if stored[1].PrevHash != stored[0].Hash {
		t.Error("second entry does not link to the first")
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestAppendIsAppendOnlyAcrossCalls(t *testing.T) {
	s := newStore(t)
	if _, err := s.Append(sample("aws_db_instance.primary", "destroy")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(sample("aws_s3_bucket.logs", "destroy")); err != nil {
		t.Fatal(err)
	}
	entries, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[1].PrevHash != entries[0].Hash {
		t.Fatal("chain broken across process boundaries")
	}
	seq, head, err := s.Head()
	if err != nil {
		t.Fatal(err)
	}
	if seq != 2 || head != entries[1].Hash {
		t.Fatalf("head = %d/%s", seq, head)
	}
}

func TestVerifyDetectsAnEditedEntry(t *testing.T) {
	s := newStore(t)
	if _, err := s.Append(sample("aws_db_instance.primary", "destroy"), sample("aws_s3_bucket.logs", "destroy")); err != nil {
		t.Fatal(err)
	}
	// Somebody edits the approver in the file to cover their tracks.
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"bob"`, `"mallory"`, 1)
	if tampered == string(raw) {
		t.Fatal("fixture did not change; the test is meaningless")
	}
	if err := os.WriteFile(s.Path(), []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	err = s.Verify()
	if err == nil {
		t.Fatal("verify accepted an edited entry")
	}
	if !strings.Contains(err.Error(), "was edited") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestVerifyDetectsADeletedEntry(t *testing.T) {
	s := newStore(t)
	if _, err := s.Append(
		sample("aws_db_instance.primary", "destroy"),
		sample("aws_s3_bucket.logs", "destroy"),
		sample("aws_iam_role.legacy", "destroy"),
	); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	// Remove the middle entry.
	kept := strings.Join([]string{lines[0], lines[2]}, "\n") + "\n"
	if err := os.WriteFile(s.Path(), []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(); err == nil {
		t.Fatal("verify accepted a ledger with a hole in it")
	}
}

func TestVerifyEmptyLedger(t *testing.T) {
	s := newStore(t)
	if err := s.Verify(); err != nil {
		t.Fatalf("an empty ledger is valid: %v", err)
	}
	entries, err := s.All()
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %v, err = %v", entries, err)
	}
}

func TestHashIgnoresApproverOrder(t *testing.T) {
	a := sample("aws_db_instance.primary", "destroy")
	a.Approvers = []string{"bob", "carol"}
	b := a
	b.Approvers = []string{"carol", "bob"}
	if a.ComputeHash() != b.ComputeHash() {
		t.Fatal("approver ordering must not change the hash")
	}
}

func TestHashCoversEveryRecordedFact(t *testing.T) {
	base := sample("aws_db_instance.primary", "destroy")
	base.Seq, base.PrevHash = 1, GenesisHash
	h := base.ComputeHash()

	mutate := map[string]func(*Entry){
		"repo":         func(e *Entry) { e.Repo = "acme/other" },
		"pr":           func(e *Entry) { e.PR = 43 },
		"commit":       func(e *Entry) { e.Commit = "cafebabe" },
		"author":       func(e *Entry) { e.Author = "mallory" },
		"approvers":    func(e *Entry) { e.Approvers = []string{"mallory"} },
		"address":      func(e *Entry) { e.Address = "aws_db_instance.replica" },
		"type":         func(e *Entry) { e.Type = "aws_s3_bucket" },
		"action":       func(e *Entry) { e.Action = "update" },
		"protected":    func(e *Entry) { e.Protected = false },
		"protected_by": func(e *Entry) { e.ProtectedBy = "none" },
		"acknowledged": func(e *Entry) { e.Acknowledged = false },
		"recorded_at":  func(e *Entry) { e.RecordedAt = e.RecordedAt.Add(time.Hour) },
		"seq":          func(e *Entry) { e.Seq = 2 },
		"prev_hash":    func(e *Entry) { e.PrevHash = strings.Repeat("f", 64) },
	}
	for field, f := range mutate {
		e := base
		e.Approvers = append([]string(nil), base.Approvers...)
		f(&e)
		if e.ComputeHash() == h {
			t.Errorf("changing %s did not change the hash", field)
		}
	}
}

func TestFilterByDateAndAction(t *testing.T) {
	entries := []Entry{
		{Seq: 1, RecordedAt: time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC), Repo: "acme/a", Action: "destroy", Protected: true},
		{Seq: 2, RecordedAt: time.Date(2026, 2, 5, 12, 0, 0, 0, time.UTC), Repo: "acme/b", Action: "replace"},
		{Seq: 3, RecordedAt: time.Date(2026, 3, 5, 12, 0, 0, 0, time.UTC), Repo: "acme/a", Action: "update"},
	}
	feb := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)

	got := Filter{From: feb, To: end}.Apply(entries)
	if len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("date filter returned %d entries", len(got))
	}
	if got := (Filter{Repo: "ACME/A"}).Apply(entries); len(got) != 2 {
		t.Fatalf("repo filter is not case-insensitive: %d", len(got))
	}
	if got := (Filter{Action: "destroy"}).Apply(entries); len(got) != 1 {
		t.Fatalf("action filter returned %d", len(got))
	}
	if got := (Filter{Protected: true}).Apply(entries); len(got) != 1 {
		t.Fatalf("protected filter returned %d", len(got))
	}
	if got := (Filter{OnlyDest: true}).Apply(entries); len(got) != 2 {
		t.Fatalf("destructive filter returned %d", len(got))
	}
}

func TestWriteCSV(t *testing.T) {
	s := newStore(t)
	stored, err := s.Append(sample("aws_db_instance.primary", "destroy"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteCSV(&b, stored); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	if lines[0] != strings.Join(csvHeader, ",") {
		t.Fatalf("header = %q", lines[0])
	}
	for _, want := range []string{"acme/platform", "42", "aws_db_instance.primary", "destroy", "alice", "bob", "true"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("row is missing %q: %s", want, lines[1])
		}
	}
}

func TestEntryRoundTripsThroughJSON(t *testing.T) {
	s := newStore(t)
	stored, err := s.Append(sample("aws_db_instance.primary", "destroy"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(stored[0])
	if err != nil {
		t.Fatal(err)
	}
	var back Entry
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ComputeHash() != stored[0].Hash {
		t.Fatal("hash does not survive a JSON round trip")
	}
}
