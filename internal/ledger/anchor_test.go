package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

func chain(t *testing.T, n int) []Entry {
	t.Helper()
	s := newStore(t)
	for i := 0; i < n; i++ {
		if _, err := s.Append(sample("aws_db_instance.primary", "destroy")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.All()
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestHeadOf(t *testing.T) {
	empty := HeadOf(nil)
	if empty.Seq != 0 || empty.Hash != GenesisHash || empty.At != nil {
		t.Errorf("empty head = %+v", empty)
	}

	entries := chain(t, 3)
	h := HeadOf(entries)
	if h.Seq != 3 || h.Hash != entries[2].Hash || h.At == nil || !h.At.Equal(entries[2].RecordedAt) {
		t.Errorf("head = %+v", h)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"seq":3`, `"hash":"` + entries[2].Hash + `"`, `"at":"2026-03-14T10:00:00Z"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("json %s missing %s", b, key)
		}
	}
}

func TestCheckAnchorsAcceptsAGrowingChain(t *testing.T) {
	entries := chain(t, 4)
	anchored := entries[1].Hash
	if err := CheckAnchors(entries, Anchors{Hashes: []string{anchored, strings.ToUpper(entries[3].Hash)}, SeqMin: 2}); err != nil {
		t.Fatalf("an older anchor on a longer chain should pass: %v", err)
	}
	if err := CheckAnchors(entries, Anchors{Hashes: []string{GenesisHash}}); err != nil {
		t.Fatalf("the genesis hash anchors every chain: %v", err)
	}
}

func TestCheckAnchorsCatchesARecomputedChain(t *testing.T) {
	original := chain(t, 3)
	anchored := original[2].Hash

	// The editor rewrites entry 1 and recomputes the whole chain, which
	// VerifyChain cannot tell apart from an honest one.
	forged := append([]Entry(nil), original...)
	forged[0].Author = "mallory"
	prev := GenesisHash
	for i := range forged {
		forged[i].PrevHash = prev
		forged[i].Hash = forged[i].ComputeHash()
		prev = forged[i].Hash
	}
	if err := VerifyChain(forged); err != nil {
		t.Fatalf("the forged chain should verify on its own: %v", err)
	}
	err := CheckAnchors(forged, Anchors{Hashes: []string{anchored}})
	if err == nil || !strings.Contains(err.Error(), "no longer reaches") {
		t.Fatalf("err = %v, want the anchor to be missed", err)
	}
}

func TestCheckAnchorsCatchesTruncation(t *testing.T) {
	entries := chain(t, 3)
	err := CheckAnchors(entries[:2], Anchors{SeqMin: 3})
	if err == nil || !strings.Contains(err.Error(), "at least 3") {
		t.Fatalf("err = %v", err)
	}
	if err := CheckAnchors(entries[:2], Anchors{Hashes: []string{entries[2].Hash}}); err == nil {
		t.Fatal("dropping the anchored entry should fail")
	}
}

func TestCheckAnchorsRejectsShortHashes(t *testing.T) {
	entries := chain(t, 1)
	for _, bad := range []string{entries[0].Hash[:12], "xyz", entries[0].Hash + "00"} {
		if err := CheckAnchors(entries, Anchors{Hashes: []string{bad}}); err == nil {
			t.Errorf("anchor %q should be rejected", bad)
		}
	}
}
