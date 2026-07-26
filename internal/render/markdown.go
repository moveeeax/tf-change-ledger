// Package render turns a parsed change set and a gate decision into the sticky
// pull request comment.
package render

import (
	"fmt"
	"sort"
	"strings"

	"github.com/moveeeax/tf-change-ledger/internal/plan"
	"github.com/moveeeax/tf-change-ledger/internal/policy"
)

// Marker identifies the sticky comment so a later run updates it in place
// rather than adding a second one. It must stay byte-identical across releases.
const Marker = "<!-- tf-change-ledger:summary -->"

// Options control the comment header.
type Options struct {
	// Dir is the Terraform working directory the plan came from, shown when a
	// repository plans more than one root module.
	Dir string
	// Commit is the plan's head commit, shortened for display.
	Commit string
	// MaxRows caps each risk section. 0 means the default of 25.
	MaxRows int
}

// Comment renders the full sticky comment in GitHub-flavoured Markdown.
func Comment(cs plan.ChangeSet, dec policy.Decision, opts Options) string {
	if opts.MaxRows <= 0 {
		opts.MaxRows = 25
	}
	protectedBy := map[string]policy.Hit{}
	for _, h := range dec.Protected {
		protectedBy[h.Address] = h
	}

	var b strings.Builder
	b.WriteString(Marker)
	b.WriteString("\n## Terraform change summary\n\n")

	c := cs.Counts()
	if c.Total() == 0 {
		b.WriteString("No changes. Infrastructure matches the configuration.\n")
		writeFooter(&b, cs, opts)
		return b.String()
	}

	b.WriteString(headline(c) + "\n\n")
	b.WriteString("| | count |\n|---|---:|\n")
	writeCountRow(&b, "destroy", c.Destroy)
	writeCountRow(&b, "replace", c.Replace)
	writeCountRow(&b, "update", c.Update)
	writeCountRow(&b, "create", c.Create)
	b.WriteString("\n")

	// Risk order, most dangerous first. This ordering is the whole point of
	// the comment: a reviewer must see the destroys without scrolling.
	for _, section := range []struct {
		action plan.Action
		title  string
	}{
		{plan.ActionDestroy, "Destroy"},
		{plan.ActionReplace, "Replace (destroy then create)"},
		{plan.ActionUpdate, "Update in place"},
		{plan.ActionCreate, "Create"},
	} {
		rows := filter(cs.Changes, section.action)
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s — %d\n\n", section.title, len(rows))
		writeTable(&b, rows, protectedBy, opts.MaxRows)
		b.WriteString("\n")
	}

	writeGate(&b, dec)
	writeFooter(&b, cs, opts)
	return b.String()
}

func headline(c plan.Counts) string {
	switch {
	case c.Destroy > 0:
		return fmt.Sprintf("**%d resource(s) will be destroyed.** Review the destroy list before approving.", c.Destroy)
	case c.Replace > 0:
		return fmt.Sprintf("**%d resource(s) will be replaced.** A replacement destroys the existing object first.", c.Replace)
	case c.Update > 0:
		return "No destroys. Updates only."
	default:
		return "No destroys. Creates only."
	}
}

func writeCountRow(b *strings.Builder, label string, n int) {
	if n == 0 {
		return
	}
	fmt.Fprintf(b, "| %s | %d |\n", label, n)
}

func filter(changes []plan.Change, a plan.Action) []plan.Change {
	out := make([]plan.Change, 0, len(changes))
	for _, c := range changes {
		if c.Action == a {
			out = append(out, c)
		}
	}
	return out
}

func writeTable(b *strings.Builder, rows []plan.Change, protectedBy map[string]policy.Hit, max int) {
	b.WriteString("| resource | type | note |\n|---|---|---|\n")
	shown := rows
	truncated := 0
	if len(rows) > max {
		shown = rows[:max]
		truncated = len(rows) - max
	}
	for _, r := range shown {
		note := ""
		if h, ok := protectedBy[r.Address]; ok {
			note = "**protected** (" + h.Rule + ")"
			if h.Reason != "" {
				note += ": " + h.Reason
			}
		}
		if len(r.ReplacePaths) > 0 {
			forced := "forced by " + strings.Join(codeEach(r.ReplacePaths), ", ")
			note = join(note, forced)
		}
		if r.ActionReason != "" {
			note = join(note, humanReason(r.ActionReason))
		}
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", r.Address, r.Type, note)
	}
	if truncated > 0 {
		fmt.Fprintf(b, "\n_+%d more not shown._\n", truncated)
	}
}

func codeEach(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, "`"+s+"`")
	}
	return out
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// humanReason turns Terraform's action_reason enum into review English.
func humanReason(reason string) string {
	switch reason {
	case "replace_because_tainted":
		return "resource is tainted"
	case "replace_because_cannot_update":
		return "an attribute cannot be updated in place"
	case "replace_by_request":
		return "replacement was requested explicitly"
	case "delete_because_no_resource_config":
		return "the resource block was removed from the configuration"
	case "delete_because_no_module":
		return "the module call was removed from the configuration"
	case "delete_because_wrong_repetition", "delete_because_count_index", "delete_because_each_key":
		return "count/for_each no longer produces this instance"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}

func writeGate(b *strings.Builder, dec policy.Decision) {
	if len(dec.Protected) == 0 {
		return
	}
	b.WriteString("### Protected-resource gate\n\n")
	if dec.Blocked {
		fmt.Fprintf(b, "**Blocked.** %s\n\n", dec.Reason)
		fmt.Fprintf(b, "To proceed, an approver must leave a comment containing exactly this line:\n\n```\n%s\n```\n\n", dec.Required)
	} else {
		fmt.Fprintf(b, "**Acknowledged.** %s\n\n", dec.Reason)
	}
	rules := map[string][]string{}
	for _, h := range dec.Protected {
		rules[h.Rule] = append(rules[h.Rule], h.Address)
	}
	names := make([]string, 0, len(rules))
	for name := range rules {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		addrs := rules[name]
		sort.Strings(addrs)
		fmt.Fprintf(b, "- `%s`: %s\n", name, strings.Join(codeEach(addrs), ", "))
	}
	b.WriteString("\n")
}

func writeFooter(b *strings.Builder, cs plan.ChangeSet, opts Options) {
	var parts []string
	if opts.Dir != "" {
		parts = append(parts, "dir `"+opts.Dir+"`")
	}
	if opts.Commit != "" {
		parts = append(parts, "commit `"+shortCommit(opts.Commit)+"`")
	}
	if cs.TerraformVersion != "" {
		parts = append(parts, "terraform "+cs.TerraformVersion)
	}
	b.WriteString("\n---\n")
	line := "Resource addresses, types and actions only — no attribute values, variables or state leave your runner."
	if len(parts) > 0 {
		line = strings.Join(parts, " · ") + " · " + line
	}
	fmt.Fprintf(b, "<sub>%s</sub>\n", line)
}

func shortCommit(c string) string {
	if len(c) <= 8 {
		return c
	}
	return c[:8]
}
