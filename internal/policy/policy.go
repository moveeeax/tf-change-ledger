// Package policy loads `.tf-ledger.yml` and decides whether a change set may
// proceed, or must be held until a human acknowledges it in writing.
package policy

import (
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultAcknowledgement is used when a repository does not set one.
const DefaultAcknowledgement = "/ledger approve"

// DefaultFilename is the conventional location of the policy inside a repo.
const DefaultFilename = ".tf-ledger.yml"

// Rule marks a set of resource types or addresses as protected.
//
// A rule matches a change when (a) at least one Types or Addresses glob matches
// and (b) the change's action is in Actions. Empty Actions defaults to the
// destructive actions, which is what the gate exists for.
type Rule struct {
	Name      string   `yaml:"name"`
	Types     []string `yaml:"types"`
	Addresses []string `yaml:"addresses"`
	Actions   []string `yaml:"actions"`
	Reason    string   `yaml:"reason"`
}

// Policy is the parsed `.tf-ledger.yml`.
type Policy struct {
	Version int `yaml:"version"`
	// Acknowledgement is the exact string an approver must comment to release
	// the gate. Matching is case-insensitive and whitespace-trimmed, but is
	// otherwise literal: no fuzzy matching, no "lgtm".
	Acknowledgement string `yaml:"acknowledgement"`
	// SelfApproval allows the pull request author to acknowledge their own
	// destructive change. Off by default, because an audit trail where the
	// author signs their own work is not an audit trail.
	SelfApproval bool   `yaml:"self_approval"`
	Protected    []Rule `yaml:"protected"`
	// Exempt address globs are never gated, whatever the protected rules say.
	// Use it for sandbox modules.
	Exempt []string `yaml:"exempt"`
}

// Default returns the policy used when a repository has no `.tf-ledger.yml`:
// nothing is protected, so nothing is gated.
func Default() *Policy {
	return &Policy{Version: 1, Acknowledgement: DefaultAcknowledgement}
}

// Load parses a policy document and validates it.
func Load(r io.Reader) (*Policy, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read policy: %w", err)
	}
	p := &Policy{}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil && err != io.EOF {
		return nil, fmt.Errorf("parse policy: %w", err)
	}
	if p.Version == 0 {
		p.Version = 1
	}
	if p.Version != 1 {
		return nil, fmt.Errorf("unsupported policy version %d (this build understands version 1)", p.Version)
	}
	if strings.TrimSpace(p.Acknowledgement) == "" {
		p.Acknowledgement = DefaultAcknowledgement
	}
	for i, r := range p.Protected {
		if len(r.Types) == 0 && len(r.Addresses) == 0 {
			return nil, fmt.Errorf("protected rule %d (%q) matches nothing: set types or addresses", i, r.Name)
		}
		for _, a := range r.Actions {
			if !knownAction(a) {
				return nil, fmt.Errorf("protected rule %d (%q): unknown action %q", i, r.Name, a)
			}
		}
	}
	return p, nil
}

func knownAction(a string) bool {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "create", "update", "delete", "destroy", "replace", "read":
		return true
	}
	return false
}

// normaliseAction maps the spellings accepted in YAML onto plan actions.
// "delete" is accepted as a synonym of "destroy" because that is the word
// Terraform itself prints.
func normaliseAction(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	if a == "delete" {
		return "destroy"
	}
	return a
}

// Match reports whether the rule covers a change with the given address, type
// and action.
func (r Rule) Match(address, resourceType, action string) bool {
	actions := r.Actions
	if len(actions) == 0 {
		actions = []string{"destroy", "replace"}
	}
	found := false
	for _, a := range actions {
		if normaliseAction(a) == action {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for _, g := range r.Types {
		if globMatch(g, resourceType) {
			return true
		}
	}
	for _, g := range r.Addresses {
		if globMatch(g, address) {
			return true
		}
	}
	return false
}

// globMatch applies shell-style globbing to a Terraform address or type.
//
// Terraform addresses are dot-separated and contain no slashes, so `*` in
// path.Match matches across the whole remainder of the address:
// `module.sandbox.*` matches `module.sandbox.aws_s3_bucket.tmp`. A pattern with
// no metacharacter is an exact match, so `aws_db_instance` does not
// accidentally match `aws_db_instance_role`.
func globMatch(pattern, s string) bool {
	if pattern == "" {
		return false
	}
	ok, err := path.Match(pattern, s)
	if err != nil {
		// A malformed pattern must never silently match everything.
		return false
	}
	return ok
}

// Exempted reports whether an address is excluded from gating.
func (p *Policy) Exempted(address string) bool {
	for _, g := range p.Exempt {
		if globMatch(g, address) {
			return true
		}
	}
	return false
}

// Hit is one change caught by one protected rule.
type Hit struct {
	Address string `json:"address"`
	Type    string `json:"type"`
	Action  string `json:"action"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason,omitempty"`
}

// Changeish is the subset of plan.Change the policy needs. Keeping the
// dependency this narrow means policy has no import of the plan package and can
// be exercised without building a whole plan document.
type Changeish struct {
	Address string
	Type    string
	Action  string
}

// Evaluate returns every change caught by a protected rule, ordered by address.
// A change is reported once, against the first rule that catches it.
func (p *Policy) Evaluate(changes []Changeish) []Hit {
	var hits []Hit
	for _, c := range changes {
		if p.Exempted(c.Address) {
			continue
		}
		for i, r := range p.Protected {
			if !r.Match(c.Address, c.Type, c.Action) {
				continue
			}
			name := r.Name
			if name == "" {
				name = fmt.Sprintf("rule[%d]", i)
			}
			hits = append(hits, Hit{
				Address: c.Address,
				Type:    c.Type,
				Action:  c.Action,
				Rule:    name,
				Reason:  r.Reason,
			})
			break
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Address < hits[j].Address })
	return hits
}

// Acknowledgement is a comment left on the pull request.
type Acknowledgement struct {
	Actor string `json:"actor"`
	Body  string `json:"body"`
}

// Decision is the outcome of the gate for one pull request.
type Decision struct {
	Blocked bool `json:"blocked"`
	// Protected are the changes a rule caught.
	Protected []Hit `json:"protected,omitempty"`
	// Approvers are the actors whose comment matched the acknowledgement
	// string and who were allowed to give it.
	Approvers []string `json:"approvers,omitempty"`
	// Reason explains the decision in one line, for a check-run summary.
	Reason string `json:"reason"`
	// Required is the exact string an approver has to comment.
	Required string `json:"required_acknowledgement"`
}

// Gate decides whether a pull request may merge.
//
// The rule is deliberately blunt: if any protected change is present, at least
// one acknowledgement matching the configured string must have been left by
// somebody other than the author (unless self_approval is enabled).
func (p *Policy) Gate(changes []Changeish, author string, acks []Acknowledgement) Decision {
	d := Decision{Required: p.Acknowledgement, Protected: p.Evaluate(changes)}
	if len(d.Protected) == 0 {
		d.Reason = "no protected resources touched"
		return d
	}

	want := strings.ToLower(strings.TrimSpace(p.Acknowledgement))
	seen := map[string]bool{}
	for _, a := range acks {
		if !matchesAck(a.Body, want) {
			continue
		}
		if !p.SelfApproval && strings.EqualFold(a.Actor, author) {
			continue
		}
		if a.Actor == "" || seen[strings.ToLower(a.Actor)] {
			continue
		}
		seen[strings.ToLower(a.Actor)] = true
		d.Approvers = append(d.Approvers, a.Actor)
	}
	sort.Strings(d.Approvers)

	if len(d.Approvers) == 0 {
		d.Blocked = true
		d.Reason = fmt.Sprintf("%d protected change(s) await acknowledgement; an approver other than %s must comment %q",
			len(d.Protected), displayAuthor(author), p.Acknowledgement)
		if p.SelfApproval {
			d.Reason = fmt.Sprintf("%d protected change(s) await acknowledgement; comment %q to proceed",
				len(d.Protected), p.Acknowledgement)
		}
		return d
	}
	d.Reason = fmt.Sprintf("%d protected change(s) acknowledged by %s",
		len(d.Protected), strings.Join(d.Approvers, ", "))
	return d
}

func displayAuthor(author string) string {
	if strings.TrimSpace(author) == "" {
		return "the plan author"
	}
	return "@" + author
}

// matchesAck reports whether a comment body contains the acknowledgement on a
// line of its own, ignoring Markdown blockquotes. Requiring a whole,
// unquoted line stops a reply that quotes the phrase ("> /ledger approve — are
// you sure about this?") from releasing the gate by accident.
func matchesAck(body, want string) bool {
	if want == "" {
		return false
	}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, ">") {
			continue // quoted text never counts as an acknowledgement
		}
		if strings.EqualFold(line, want) {
			return true
		}
	}
	return false
}
