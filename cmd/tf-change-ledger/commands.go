package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/moveeeax/tf-change-ledger/internal/ledger"
	"github.com/moveeeax/tf-change-ledger/internal/plan"
	"github.com/moveeeax/tf-change-ledger/internal/policy"
	"github.com/moveeeax/tf-change-ledger/internal/render"
)

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func loadPlan(path string) (plan.ChangeSet, error) {
	if path == "" {
		return plan.ChangeSet{}, errors.New("-plan is required (produce it with: terraform show -json tfplan > plan.json)")
	}
	if path == "-" {
		return plan.Parse(os.Stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return plan.ChangeSet{}, fmt.Errorf("open plan: %w", err)
	}
	defer f.Close()
	return plan.Parse(f)
}

// loadPolicy reads the policy file. A missing file is only an error when the
// path was given explicitly: the default location is allowed to be absent, in
// which case nothing is protected.
func loadPolicy(path string, explicit bool) (*policy.Policy, error) {
	if path == "" {
		return policy.Default(), nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return policy.Default(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("open policy: %w", err)
	}
	defer f.Close()
	return policy.Load(f)
}

// loadAcks reads acknowledgements from a JSON file and/or repeated
// -ack "actor:body" flags.
//
// The JSON shape is a list of {"actor": "...", "body": "..."} which is what a
// caller gets from `gh api repos/O/R/issues/N/comments --jq` — no GitHub
// credentials are needed by this tool itself.
func loadAcks(path string, inline stringList) ([]policy.Acknowledgement, error) {
	var acks []policy.Acknowledgement
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read acknowledgements: %w", err)
		}
		if err := json.Unmarshal(b, &acks); err != nil {
			return nil, fmt.Errorf("parse acknowledgements (expected a JSON list of {actor, body}): %w", err)
		}
	}
	for _, raw := range inline {
		actor, body, ok := strings.Cut(raw, ":")
		if !ok {
			return nil, fmt.Errorf("-ack %q is not in the form actor:body", raw)
		}
		acks = append(acks, policy.Acknowledgement{Actor: strings.TrimSpace(actor), Body: strings.TrimSpace(body)})
	}
	return acks, nil
}

func changeish(cs plan.ChangeSet) []policy.Changeish {
	out := make([]policy.Changeish, 0, len(cs.Changes))
	for _, c := range cs.Changes {
		out = append(out, policy.Changeish{Address: c.Address, Type: c.Type, Action: string(c.Action)})
	}
	return out
}

func runSummarize(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ExitOnError)
	planPath := fs.String("plan", "", "path to `terraform show -json` output, or - for stdin")
	cfgPath := fs.String("config", policy.DefaultFilename, "path to the policy file")
	out := fs.String("out", "-", "write the comment here, or - for stdout")
	format := fs.String("format", "markdown", "markdown or json")
	dir := fs.String("dir", "", "Terraform working directory, shown in the comment footer")
	commit := fs.String("commit", "", "head commit, shown in the comment footer")
	maxRows := fs.Int("max-rows", 25, "maximum resources listed per section")
	ackFile := fs.String("ack-file", "", "JSON list of {actor, body} pull request comments")
	var acksInline stringList
	fs.Var(&acksInline, "ack", "acknowledgement as actor:body (repeatable)")
	author := fs.String("author", "", "pull request author, who may not acknowledge their own change")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cs, err := loadPlan(*planPath)
	if err != nil {
		return err
	}
	pol, err := loadPolicy(*cfgPath, isExplicit(fs, "config"))
	if err != nil {
		return err
	}
	acks, err := loadAcks(*ackFile, acksInline)
	if err != nil {
		return err
	}
	dec := pol.Gate(changeish(cs), *author, acks)

	var body string
	if *format == "json" {
		b, err := json.MarshalIndent(struct {
			Plan     plan.ChangeSet  `json:"plan"`
			Counts   plan.Counts     `json:"counts"`
			Decision policy.Decision `json:"decision"`
		}{cs, cs.Counts(), dec}, "", "  ")
		if err != nil {
			return err
		}
		body = string(b) + "\n"
	} else {
		body = render.Comment(cs, dec, render.Options{Dir: *dir, Commit: *commit, MaxRows: *maxRows})
	}
	return writeOut(*out, body)
}

func runGate(args []string) error {
	fs := flag.NewFlagSet("gate", flag.ExitOnError)
	planPath := fs.String("plan", "", "path to `terraform show -json` output, or - for stdin")
	cfgPath := fs.String("config", policy.DefaultFilename, "path to the policy file")
	author := fs.String("author", "", "pull request author, who may not acknowledge their own change")
	ackFile := fs.String("ack-file", "", "JSON list of {actor, body} pull request comments")
	format := fs.String("format", "text", "text or json")
	var acksInline stringList
	fs.Var(&acksInline, "ack", "acknowledgement as actor:body (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cs, err := loadPlan(*planPath)
	if err != nil {
		return err
	}
	pol, err := loadPolicy(*cfgPath, isExplicit(fs, "config"))
	if err != nil {
		return err
	}
	acks, err := loadAcks(*ackFile, acksInline)
	if err != nil {
		return err
	}
	dec := pol.Gate(changeish(cs), *author, acks)

	if *format == "json" {
		b, err := json.MarshalIndent(dec, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
	} else {
		for _, h := range dec.Protected {
			fmt.Printf("protected  %-8s %s  (rule %s)\n", h.Action, h.Address, h.Rule)
		}
		fmt.Println(dec.Reason)
	}
	if dec.Blocked {
		// The reason has already gone to stdout; do not repeat it on stderr.
		return blockedError{msg: "gate blocked"}
	}
	return nil
}

func runRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	planPath := fs.String("plan", "", "path to `terraform show -json` output, or - for stdin")
	cfgPath := fs.String("config", policy.DefaultFilename, "path to the policy file")
	ledgerPath := fs.String("ledger", "ledger.jsonl", "append-only ledger file")
	repo := fs.String("repo", "", "owner/repo the change was merged into (required)")
	pr := fs.Int("pr", 0, "pull request number (required)")
	commit := fs.String("commit", "", "merge commit SHA (required)")
	author := fs.String("author", "", "pull request author (required)")
	ackFile := fs.String("ack-file", "", "JSON list of {actor, body} pull request comments")
	all := fs.Bool("all", false, "record every change, not only destroys and replacements")
	at := fs.String("at", "", "merge timestamp in RFC3339; defaults to now")
	quiet := fs.Bool("quiet", false, "suppress the per-entry log")
	var approvers stringList
	fs.Var(&approvers, "approver", "GitHub login that approved the pull request (repeatable)")
	var acksInline stringList
	fs.Var(&acksInline, "ack", "acknowledgement as actor:body (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for name, v := range map[string]string{"-repo": *repo, "-commit": *commit, "-author": *author} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if *pr <= 0 {
		return errors.New("-pr is required and must be positive")
	}

	recordedAt := time.Now().UTC()
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return fmt.Errorf("parse -at: %w", err)
		}
		recordedAt = t.UTC()
	}

	cs, err := loadPlan(*planPath)
	if err != nil {
		return err
	}
	pol, err := loadPolicy(*cfgPath, isExplicit(fs, "config"))
	if err != nil {
		return err
	}
	acks, err := loadAcks(*ackFile, acksInline)
	if err != nil {
		return err
	}
	dec := pol.Gate(changeish(cs), *author, acks)
	hits := map[string]policy.Hit{}
	for _, h := range dec.Protected {
		hits[h.Address] = h
	}

	// Approvers on the entry are the union of pull request reviewers and the
	// people who left the acknowledgement, deduplicated. Both are evidence.
	people := map[string]string{}
	for _, a := range approvers {
		people[strings.ToLower(a)] = a
	}
	for _, a := range dec.Approvers {
		people[strings.ToLower(a)] = a
	}
	names := make([]string, 0, len(people))
	for _, v := range people {
		names = append(names, v)
	}
	sort.Strings(names)

	var entries []ledger.Entry
	for _, c := range cs.Changes {
		if !*all && !c.Action.Destructive() {
			continue
		}
		h, protected := hits[c.Address]
		entries = append(entries, ledger.Entry{
			RecordedAt:   recordedAt,
			Repo:         *repo,
			PR:           *pr,
			Commit:       *commit,
			Author:       *author,
			Approvers:    names,
			Address:      c.Address,
			Type:         c.Type,
			Action:       string(c.Action),
			Protected:    protected,
			ProtectedBy:  h.Rule,
			Acknowledged: protected && !dec.Blocked,
		})
	}
	if len(entries) == 0 {
		if !*quiet {
			fmt.Println("nothing to record: the plan contains no destructive change")
		}
		return nil
	}

	store := ledger.Open(*ledgerPath)
	stored, err := store.Append(entries...)
	if err != nil {
		return err
	}
	if !*quiet {
		for _, e := range stored {
			fmt.Printf("recorded #%d %-8s %s  approvers=%s  hash=%s\n",
				e.Seq, e.Action, e.Address, orNone(strings.Join(e.Approvers, ",")), e.Hash[:12])
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	ledgerPath := fs.String("ledger", "ledger.jsonl", "append-only ledger file")
	from := fs.String("from", "", "inclusive start date, YYYY-MM-DD or RFC3339")
	to := fs.String("to", "", "inclusive end date, YYYY-MM-DD or RFC3339")
	repo := fs.String("repo", "", "filter by owner/repo")
	action := fs.String("action", "", "filter by action: destroy, replace, update, create")
	protectedOnly := fs.Bool("protected-only", false, "only changes caught by a protected rule")
	destructiveOnly := fs.Bool("destructive-only", false, "only destroys and replacements")
	format := fs.String("format", "csv", "csv or json")
	out := fs.String("out", "-", "write here, or - for stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store := ledger.Open(*ledgerPath)
	entries, err := store.All()
	if err != nil {
		return err
	}
	f := ledger.Filter{Repo: *repo, Action: *action, Protected: *protectedOnly, OnlyDest: *destructiveOnly}
	if f.From, err = parseBound(*from, false); err != nil {
		return fmt.Errorf("parse -from: %w", err)
	}
	if f.To, err = parseBound(*to, true); err != nil {
		return fmt.Errorf("parse -to: %w", err)
	}
	rows := f.Apply(entries)

	var body strings.Builder
	if *format == "json" {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		body.Write(b)
		body.WriteByte('\n')
	} else {
		if err := ledger.WriteCSV(&body, rows); err != nil {
			return err
		}
	}
	return writeOut(*out, body.String())
}

// parseBound accepts a date or a full timestamp. A bare date used as an upper
// bound is widened to the end of that day, so -to 2026-03-31 includes the 31st.
func parseBound(s string, endOfDay bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither YYYY-MM-DD nor RFC3339", s)
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Second)
	}
	return t.UTC(), nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	ledgerPath := fs.String("ledger", "ledger.jsonl", "append-only ledger file")
	var expectHeads stringList
	fs.Var(&expectHeads, "expect-head", "full hash of a previously published head the chain must still reach (repeatable)")
	seqMin := fs.Int("expect-seq-min", 0, "fail when the chain holds fewer entries than this")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *seqMin < 0 {
		return errors.New("-expect-seq-min must not be negative")
	}
	for _, h := range expectHeads {
		if !ledger.IsFullHash(strings.ToLower(strings.TrimSpace(h))) {
			return fmt.Errorf("-expect-head %q is not a full 64-character hash (print one with: tf-change-ledger head)", h)
		}
	}
	store := ledger.Open(*ledgerPath)
	entries, err := store.All()
	if err != nil {
		return err
	}
	if err := ledger.VerifyChain(entries); err != nil {
		return blockedError{msg: "ledger verification failed: " + err.Error()}
	}
	anchors := ledger.Anchors{Hashes: expectHeads, SeqMin: *seqMin}
	if err := ledger.CheckAnchors(entries, anchors); err != nil {
		return blockedError{msg: "ledger verification failed: " + err.Error()}
	}
	head := ledger.HeadOf(entries)
	msg := fmt.Sprintf("ok: %d entries, chain intact, head %s", len(entries), head.Hash[:12])
	if n := len(expectHeads); n > 0 {
		msg += fmt.Sprintf(", %d anchored head(s) reached", n)
	}
	fmt.Println(msg)
	return nil
}

// runHead prints the head of a verified chain. Publishing it somewhere the
// ledger's editors cannot rewrite is what turns "the chain is intact" into
// "the chain is the one we saw then"; see ledger.CheckAnchors.
func runHead(args []string) error {
	fs := flag.NewFlagSet("head", flag.ExitOnError)
	ledgerPath := fs.String("ledger", "ledger.jsonl", "append-only ledger file")
	format := fs.String("format", "text", "text (seq hash at) or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *format != "text" && *format != "json" {
		return fmt.Errorf("-format %q: want text or json", *format)
	}
	entries, err := ledger.Open(*ledgerPath).All()
	if err != nil {
		return err
	}
	// A head is only worth anchoring if the chain behind it holds.
	if err := ledger.VerifyChain(entries); err != nil {
		return blockedError{msg: "ledger verification failed: " + err.Error()}
	}
	head := ledger.HeadOf(entries)
	if *format == "json" {
		b, err := json.Marshal(head)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	at := "-"
	if head.At != nil {
		at = head.At.Format(time.RFC3339)
	}
	fmt.Printf("%d %s %s\n", head.Seq, head.Hash, at)
	return nil
}

func writeOut(path, body string) error {
	if path == "" || path == "-" {
		_, err := os.Stdout.WriteString(body)
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

// isExplicit reports whether a flag was actually passed, as opposed to left at
// its default.
func isExplicit(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}
