package ledger

import (
	"fmt"
	"strings"
	"time"
)

// A hash chain only proves that nobody edited history after the head was
// fixed. Whoever can write the file can also rewrite every entry and recompute
// every hash, and `verify` will call the new chain intact. The defence is to
// publish the head somewhere that person cannot rewrite (a merge commit
// trailer, a signed tag, a ticket) and later check that the chain still
// reaches it. Head is what gets published; CheckAnchors is the later check.

// Head identifies the last entry of a chain: what to publish as an anchor.
type Head struct {
	Seq  int        `json:"seq"`
	Hash string     `json:"hash"`
	At   *time.Time `json:"at"` // recorded_at of the last entry; null for an empty ledger
}

// HeadOf returns the head of entries. An empty ledger's head is seq 0 with the
// genesis hash.
func HeadOf(entries []Entry) Head {
	if len(entries) == 0 {
		return Head{Seq: 0, Hash: GenesisHash}
	}
	last := entries[len(entries)-1]
	at := last.RecordedAt.UTC()
	return Head{Seq: last.Seq, Hash: last.Hash, At: &at}
}

// Anchors are previously published facts about the chain.
type Anchors struct {
	// Hashes are entry hashes that must each still appear in the chain.
	Hashes []string
	// SeqMin is the least number of entries the chain must hold, which catches
	// truncation back past an anchor whose hash was not kept.
	SeqMin int
}

// CheckAnchors reports the first anchor the chain no longer reaches. It does
// not re-verify the chain itself; run VerifyChain first.
//
// Hashes are compared in full. A 12-character prefix is what the CLI prints
// for reading, but a prefix that short is within reach of a forger who
// controls the entry content, so it is rejected as an anchor.
func CheckAnchors(entries []Entry, a Anchors) error {
	if a.SeqMin > 0 && len(entries) < a.SeqMin {
		return fmt.Errorf("the chain has %d entries, expected at least %d (entries were removed from the end)", len(entries), a.SeqMin)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		seen[e.Hash] = true
	}
	for _, raw := range a.Hashes {
		h := strings.ToLower(strings.TrimSpace(raw))
		if !IsFullHash(h) {
			return fmt.Errorf("expected head %q is not a full 64-character SHA-256 hex hash", raw)
		}
		if h == GenesisHash {
			continue
		}
		if !seen[h] {
			return fmt.Errorf("the chain no longer reaches the anchored head %s (history was rewritten or truncated)", short(h))
		}
	}
	return nil
}

// IsFullHash reports whether h is a lower-case 64-character SHA-256 hex hash.
func IsFullHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
