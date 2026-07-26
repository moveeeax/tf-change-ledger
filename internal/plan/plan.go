// Package plan parses the JSON representation of a Terraform plan
// (`terraform show -json tfplan`) into a minimal, privacy-preserving change set.
//
// Privacy is a hard requirement of this tool, not a nicety: the structs below
// deliberately declare no fields for `change.before`, `change.after`,
// `change.after_unknown`, `variables`, `prior_state` or `configuration`.
// encoding/json drops unknown object members, so attribute values, secrets and
// state contents are never materialised in memory and can never be forwarded to
// a ledger endpoint. Only resource addresses, types, names and actions survive
// parsing.
package plan

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Action is the effect a plan has on a single resource, collapsed from the
// Terraform action list into one value.
type Action string

const (
	// ActionNoOp means the resource is unchanged.
	ActionNoOp Action = "no-op"
	// ActionCreate means the resource will be created.
	ActionCreate Action = "create"
	// ActionRead means a data source will be read during apply.
	ActionRead Action = "read"
	// ActionUpdate means the resource will be updated in place.
	ActionUpdate Action = "update"
	// ActionReplace means the resource will be destroyed and recreated.
	ActionReplace Action = "replace"
	// ActionDestroy means the resource will be destroyed.
	ActionDestroy Action = "destroy"
)

// Destructive reports whether the action removes an existing object, and so
// carries a risk of irreversible data loss.
func (a Action) Destructive() bool {
	return a == ActionDestroy || a == ActionReplace
}

// rank orders actions by how much attention they deserve in a review.
// Lower is more urgent.
func (a Action) rank() int {
	switch a {
	case ActionDestroy:
		return 0
	case ActionReplace:
		return 1
	case ActionUpdate:
		return 2
	case ActionCreate:
		return 3
	case ActionRead:
		return 4
	default:
		return 5
	}
}

// Change is a single resource change, stripped of every attribute value.
type Change struct {
	Address      string `json:"address"`
	Module       string `json:"module,omitempty"`
	Type         string `json:"type"`
	Name         string `json:"name"`
	Provider     string `json:"provider,omitempty"`
	Action       Action `json:"action"`
	ActionReason string `json:"action_reason,omitempty"`
	// ReplacePaths holds the top-level attribute *names* that forced a
	// replacement. Names only — never values.
	ReplacePaths []string `json:"replace_paths,omitempty"`
}

// ChangeSet is the parsed result of one plan file.
type ChangeSet struct {
	TerraformVersion string   `json:"terraform_version,omitempty"`
	FormatVersion    string   `json:"format_version,omitempty"`
	Changes          []Change `json:"changes"`
}

// Counts summarises a change set by action.
type Counts struct {
	Create  int `json:"create"`
	Update  int `json:"update"`
	Replace int `json:"replace"`
	Destroy int `json:"destroy"`
	Read    int `json:"read"`
	NoOp    int `json:"no_op"`
}

// Total returns the number of resources that actually change.
// no-op and data reads are excluded: they never appear in a review.
func (c Counts) Total() int {
	return c.Create + c.Update + c.Replace + c.Destroy
}

// Counts tallies the change set by action.
func (cs ChangeSet) Counts() Counts {
	var out Counts
	for _, ch := range cs.Changes {
		switch ch.Action {
		case ActionCreate:
			out.Create++
		case ActionUpdate:
			out.Update++
		case ActionReplace:
			out.Replace++
		case ActionDestroy:
			out.Destroy++
		case ActionRead:
			out.Read++
		default:
			out.NoOp++
		}
	}
	return out
}

// Destructive returns only the changes that destroy or replace an object,
// preserving the risk ordering of the change set.
func (cs ChangeSet) Destructive() []Change {
	out := make([]Change, 0, len(cs.Changes))
	for _, ch := range cs.Changes {
		if ch.Action.Destructive() {
			out = append(out, ch)
		}
	}
	return out
}

// rawPlan mirrors only the parts of the plan JSON we are willing to read.
type rawPlan struct {
	FormatVersion    string      `json:"format_version"`
	TerraformVersion string      `json:"terraform_version"`
	ResourceChanges  []rawChange `json:"resource_changes"`
}

type rawChange struct {
	Address       string `json:"address"`
	ModuleAddress string `json:"module_address"`
	Mode          string `json:"mode"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	ProviderName  string `json:"provider_name"`
	ActionReason  string `json:"action_reason"`
	Change        struct {
		Actions      []string `json:"actions"`
		ReplacePaths []any    `json:"replace_paths"`
	} `json:"change"`
}

// Parse reads a `terraform show -json` document and returns the change set,
// ordered by descending risk: destroys, then replacements, then updates, then
// creates. Within an action changes are ordered by address so that output is
// stable across runs and diffable across commits.
func Parse(r io.Reader) (ChangeSet, error) {
	dec := json.NewDecoder(r)
	var raw rawPlan
	if err := dec.Decode(&raw); err != nil {
		return ChangeSet{}, fmt.Errorf("decode plan json: %w", err)
	}
	if raw.FormatVersion == "" && raw.ResourceChanges == nil {
		return ChangeSet{}, fmt.Errorf("not a terraform plan document: no format_version and no resource_changes")
	}

	cs := ChangeSet{
		FormatVersion:    raw.FormatVersion,
		TerraformVersion: raw.TerraformVersion,
		Changes:          make([]Change, 0, len(raw.ResourceChanges)),
	}
	for _, rc := range raw.ResourceChanges {
		action := collapse(rc.Change.Actions)
		// Data sources produce noise, never risk. Drop them entirely.
		if rc.Mode == "data" {
			continue
		}
		if action == ActionNoOp {
			continue
		}
		cs.Changes = append(cs.Changes, Change{
			Address:      rc.Address,
			Module:       rc.ModuleAddress,
			Type:         rc.Type,
			Name:         rc.Name,
			Provider:     shortProvider(rc.ProviderName),
			Action:       action,
			ActionReason: rc.ActionReason,
			ReplacePaths: replacePathNames(rc.Change.ReplacePaths),
		})
	}

	sort.SliceStable(cs.Changes, func(i, j int) bool {
		a, b := cs.Changes[i], cs.Changes[j]
		if a.Action.rank() != b.Action.rank() {
			return a.Action.rank() < b.Action.rank()
		}
		return a.Address < b.Address
	})
	return cs, nil
}

// ParseBytes is Parse over an in-memory document.
func ParseBytes(b []byte) (ChangeSet, error) { return Parse(strings.NewReader(string(b))) }

// collapse folds Terraform's action list into a single Action.
//
// Terraform emits ["delete","create"] for a normal replacement and
// ["create","delete"] when the resource has create_before_destroy set. Both are
// replacements and must be treated identically.
func collapse(actions []string) Action {
	switch len(actions) {
	case 0:
		return ActionNoOp
	case 1:
		switch actions[0] {
		case "create":
			return ActionCreate
		case "update":
			return ActionUpdate
		case "delete":
			return ActionDestroy
		case "read":
			return ActionRead
		default: // "no-op" and anything unrecognised
			return ActionNoOp
		}
	case 2:
		if (actions[0] == "delete" && actions[1] == "create") ||
			(actions[0] == "create" && actions[1] == "delete") {
			return ActionReplace
		}
		return ActionNoOp
	default:
		return ActionNoOp
	}
}

// replacePathNames extracts the top-level attribute names from
// change.replace_paths, which Terraform encodes as a list of index paths such
// as [["engine_version"],["availability_zone"]]. Only the leading string of
// each path is kept; nested indices are attribute positions, not names, and are
// of no use in a review comment.
func replacePathNames(paths []any) []string {
	if len(paths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		steps, ok := p.([]any)
		if !ok || len(steps) == 0 {
			continue
		}
		name, ok := steps[0].(string)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// shortProvider turns "registry.terraform.io/hashicorp/aws" into "aws".
func shortProvider(name string) string {
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}
