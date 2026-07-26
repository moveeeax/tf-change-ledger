package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moveeeax/tf-change-ledger/internal/plan"
	"github.com/moveeeax/tf-change-ledger/internal/policy"
)

func fixtures(t *testing.T, planName string) (plan.ChangeSet, *policy.Policy) {
	t.Helper()
	root := filepath.Join("..", "..", "testdata")
	pf, err := os.Open(filepath.Join(root, planName))
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Close()
	cs, err := plan.Parse(pf)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := os.Open(filepath.Join(root, ".tf-ledger.yml"))
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close()
	pol, err := policy.Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	return cs, pol
}

func changeish(cs plan.ChangeSet) []policy.Changeish {
	out := make([]policy.Changeish, 0, len(cs.Changes))
	for _, c := range cs.Changes {
		out = append(out, policy.Changeish{Address: c.Address, Type: c.Type, Action: string(c.Action)})
	}
	return out
}

func TestCommentOrdersDestroysFirst(t *testing.T) {
	cs, pol := fixtures(t, "plan-destructive.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{Dir: "envs/prod", Commit: "0bada55deadbeef"})

	if !strings.HasPrefix(out, Marker) {
		t.Fatal("comment must start with the sticky marker")
	}
	idx := func(s string) int {
		i := strings.Index(out, s)
		if i < 0 {
			t.Fatalf("section %q missing from comment:\n%s", s, out)
		}
		return i
	}
	destroy, replace, update, create := idx("### Destroy"), idx("### Replace"), idx("### Update in place"), idx("### Create")
	if !(destroy < replace && replace < update && update < create) {
		t.Fatalf("sections out of risk order: destroy=%d replace=%d update=%d create=%d", destroy, replace, update, create)
	}
	if !strings.Contains(out, "**3 resource(s) will be destroyed.**") {
		t.Error("headline should lead with the destroy count")
	}
	if !strings.Contains(out, "dir `envs/prod`") || !strings.Contains(out, "commit `0bada55d`") {
		t.Error("footer should carry dir and short commit")
	}
	if !strings.Contains(out, "terraform 1.9.5") {
		t.Error("footer should carry the terraform version")
	}
}

func TestCommentShowsTheBlockedGate(t *testing.T) {
	cs, pol := fixtures(t, "plan-destructive.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{})

	if !strings.Contains(out, "### Protected-resource gate") {
		t.Fatal("gate section missing")
	}
	if !strings.Contains(out, "**Blocked.**") {
		t.Error("gate should read as blocked")
	}
	if !strings.Contains(out, "/ledger approve") {
		t.Error("comment must tell the reviewer the exact string to type")
	}
	for _, rule := range []string{"databases", "buckets-and-state", "identity"} {
		if !strings.Contains(out, "`"+rule+"`") {
			t.Errorf("rule %q not named in the gate section", rule)
		}
	}
	if strings.Contains(out, "module.sandbox.aws_db_instance.scratch`, ") {
		t.Error("exempt address should not appear in the gate list")
	}
}

func TestCommentShowsTheReleasedGate(t *testing.T) {
	cs, pol := fixtures(t, "plan-destructive.json")
	dec := pol.Gate(changeish(cs), "alice", []policy.Acknowledgement{{Actor: "bob", Body: "/ledger approve"}})
	out := Comment(cs, dec, Options{})
	if !strings.Contains(out, "**Acknowledged.**") || !strings.Contains(out, "bob") {
		t.Fatalf("released gate not rendered:\n%s", out)
	}
}

func TestCommentAnnotatesReplacementCause(t *testing.T) {
	cs, pol := fixtures(t, "plan-destructive.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{})
	if !strings.Contains(out, "forced by `availability_zone`, `engine_version`") {
		t.Error("replacement should name the attributes that forced it")
	}
	if !strings.Contains(out, "an attribute cannot be updated in place") {
		t.Error("action_reason should be rendered in English")
	}
	if !strings.Contains(out, "the resource block was removed from the configuration") {
		t.Error("delete_because_no_resource_config should be rendered in English")
	}
	if !strings.Contains(out, "resource is tainted") {
		t.Error("replace_because_tainted should be rendered in English")
	}
}

func TestCommentNeverLeaksAttributeValues(t *testing.T) {
	cs, pol := fixtures(t, "plan-destructive.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{})
	for _, secret := range []string{"this-value-must-never-be-parsed", "ami-000", "example-logs", "14.7", "15.4"} {
		if strings.Contains(out, secret) {
			t.Errorf("comment leaked attribute value %q", secret)
		}
	}
}

func TestCommentForAnAdditivePlanHasNoGate(t *testing.T) {
	cs, pol := fixtures(t, "plan-additive.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{})
	if strings.Contains(out, "Protected-resource gate") {
		t.Error("an additive plan should not render a gate section")
	}
	if !strings.Contains(out, "No destroys. Creates only.") {
		t.Errorf("unexpected headline:\n%s", out)
	}
	if strings.Contains(out, "### Destroy") {
		t.Error("empty sections should be omitted")
	}
}

func TestCommentForAnEmptyPlan(t *testing.T) {
	cs, pol := fixtures(t, "plan-empty.json")
	out := Comment(cs, pol.Gate(changeish(cs), "alice", nil), Options{})
	if !strings.Contains(out, "No changes.") {
		t.Errorf("unexpected body:\n%s", out)
	}
}

func TestCommentTruncatesLongSections(t *testing.T) {
	var cs plan.ChangeSet
	for i := 0; i < 30; i++ {
		cs.Changes = append(cs.Changes, plan.Change{
			Address: "aws_instance.n" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Type:    "aws_instance",
			Action:  plan.ActionDestroy,
		})
	}
	out := Comment(cs, policy.Decision{}, Options{MaxRows: 5})
	if !strings.Contains(out, "_+25 more not shown._") {
		t.Errorf("truncation notice missing:\n%s", out)
	}
}
