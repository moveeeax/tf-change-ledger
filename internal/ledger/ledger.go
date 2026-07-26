// Package ledger holds the append-only record of destructive Terraform changes
// and who approved them.
//
// The record is the product. Everything else — the parser, the comment, the
// gate — exists to produce it. Two properties matter to an auditor and are
// implemented here:
//
//  1. Append-only. Entries are only ever written to the end of the file.
//  2. Tamper-evident. Each entry carries the SHA-256 of the previous entry, so
//     editing or deleting history breaks the chain and `verify` says where.
//
// The file-backed store is the reference implementation and is what the CLI
// uses. The hosted service will put the same Entry shape in Postgres with the
// same chaining, so the chain can be recomputed and compared.
package ledger

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// GenesisHash is the PrevHash of the first entry in a chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Entry is one destructive change to one resource, in one merged pull request.
//
// One entry per resource, not per pull request: an auditor asks "who approved
// deleting this database", and the answer must not require reading a diff.
type Entry struct {
	// Chain fields.
	Seq      int    `json:"seq"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`

	// Provenance.
	RecordedAt time.Time `json:"recorded_at"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Commit     string    `json:"commit"`
	Author     string    `json:"author"`
	Approvers  []string  `json:"approvers,omitempty"`

	// The change itself. Addresses, types and actions only: no attribute
	// values ever enter the ledger.
	Address      string `json:"address"`
	Type         string `json:"type"`
	Action       string `json:"action"`
	Protected    bool   `json:"protected"`
	ProtectedBy  string `json:"protected_by,omitempty"`
	Acknowledged bool   `json:"acknowledged"`
}

// payload is the canonical, order-stable serialisation that the hash covers.
// Hash itself is excluded; every other field is included, so any edit to a
// recorded fact invalidates the chain.
func (e Entry) payload() string {
	approvers := append([]string(nil), e.Approvers...)
	sort.Strings(approvers)
	return strings.Join([]string{
		fmt.Sprintf("seq=%d", e.Seq),
		"prev=" + e.PrevHash,
		"at=" + e.RecordedAt.UTC().Format(time.RFC3339Nano),
		"repo=" + e.Repo,
		fmt.Sprintf("pr=%d", e.PR),
		"commit=" + e.Commit,
		"author=" + e.Author,
		"approvers=" + strings.Join(approvers, ","),
		"address=" + e.Address,
		"type=" + e.Type,
		"action=" + e.Action,
		fmt.Sprintf("protected=%t", e.Protected),
		"protected_by=" + e.ProtectedBy,
		fmt.Sprintf("acknowledged=%t", e.Acknowledged),
	}, "\n")
}

// ComputeHash returns the chain hash for an entry.
func (e Entry) ComputeHash() string {
	sum := sha256.Sum256([]byte(e.payload()))
	return hex.EncodeToString(sum[:])
}

// Store is an append-only ledger backed by a JSON Lines file.
type Store struct {
	path string
}

// Open returns a store for the file at path. The file is created on first
// append; opening a path that does not yet exist is not an error.
func Open(path string) *Store { return &Store{path: path} }

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// All reads every entry in chain order.
func (s *Store) All() ([]Entry, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	return read(f)
}

func read(r io.Reader) ([]Entry, error) {
	var out []Entry
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("ledger line %d: %w", line, err)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return out, nil
}

// Head returns the sequence number and hash of the last entry.
func (s *Store) Head() (seq int, hash string, err error) {
	entries, err := s.All()
	if err != nil {
		return 0, "", err
	}
	if len(entries) == 0 {
		return 0, GenesisHash, nil
	}
	last := entries[len(entries)-1]
	return last.Seq, last.Hash, nil
}

// Append writes entries to the end of the ledger, filling in Seq, PrevHash and
// Hash. The passed entries are not modified; the stored entries are returned.
//
// There is deliberately no Update and no Delete. Correcting a mistake means
// appending a new entry, exactly as it does in a paper ledger.
func (s *Store) Append(entries ...Entry) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	seq, prev, err := s.Head()
	if err != nil {
		return nil, err
	}

	stored := make([]Entry, 0, len(entries))
	var buf strings.Builder
	for _, e := range entries {
		seq++
		e.Seq = seq
		e.PrevHash = prev
		if e.RecordedAt.IsZero() {
			e.RecordedAt = time.Now().UTC()
		}
		e.RecordedAt = e.RecordedAt.UTC().Truncate(time.Second)
		e.Hash = e.ComputeHash()
		prev = e.Hash

		b, err := json.Marshal(e)
		if err != nil {
			return nil, fmt.Errorf("encode entry %d: %w", e.Seq, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
		stored = append(stored, e)
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open ledger for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(buf.String()); err != nil {
		return nil, fmt.Errorf("append ledger: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("sync ledger: %w", err)
	}
	return stored, nil
}

// Verify walks the chain and returns the first problem it finds, or nil.
func (s *Store) Verify() error {
	entries, err := s.All()
	if err != nil {
		return err
	}
	return VerifyChain(entries)
}

// VerifyChain checks sequence continuity, back-links and per-entry hashes.
func VerifyChain(entries []Entry) error {
	prev := GenesisHash
	for i, e := range entries {
		if e.Seq != i+1 {
			return fmt.Errorf("entry %d: sequence is %d, expected %d (an entry was removed or reordered)", i+1, e.Seq, i+1)
		}
		if e.PrevHash != prev {
			return fmt.Errorf("entry %d (%s): prev_hash %s does not match previous entry hash %s", e.Seq, e.Address, short(e.PrevHash), short(prev))
		}
		if got := e.ComputeHash(); got != e.Hash {
			return fmt.Errorf("entry %d (%s): content hash %s does not match recorded %s (the entry was edited)", e.Seq, e.Address, short(got), short(e.Hash))
		}
		prev = e.Hash
	}
	return nil
}

func short(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// Filter narrows a set of entries for export.
type Filter struct {
	From      time.Time
	To        time.Time
	Repo      string
	Action    string
	Protected bool
	OnlyDest  bool
}

// Apply returns the entries matching the filter, in chain order.
func (f Filter) Apply(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !f.From.IsZero() && e.RecordedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && e.RecordedAt.After(f.To) {
			continue
		}
		if f.Repo != "" && !strings.EqualFold(f.Repo, e.Repo) {
			continue
		}
		if f.Action != "" && !strings.EqualFold(f.Action, e.Action) {
			continue
		}
		if f.Protected && !e.Protected {
			continue
		}
		if f.OnlyDest && e.Action != "destroy" && e.Action != "replace" {
			continue
		}
		out = append(out, e)
	}
	return out
}
