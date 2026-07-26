package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, name string) ChangeSet {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	cs, err := Parse(f)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return cs
}

func TestParseOrdersByRisk(t *testing.T) {
	cs := load(t, "plan-destructive.json")

	got := make([]string, 0, len(cs.Changes))
	for _, c := range cs.Changes {
		got = append(got, string(c.Action)+" "+c.Address)
	}
	want := []string{
		// destroys first, alphabetically within the action
		"destroy aws_iam_role.legacy",
		"destroy aws_s3_bucket.logs",
		"destroy module.sandbox.aws_db_instance.scratch",
		// then replacements
		"replace aws_instance.worker",
		"replace module.data.aws_db_instance.primary",
		// then updates
		"update aws_security_group.web",
		// then creates
		"create aws_s3_bucket.assets",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("position %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseDropsNoOpAndDataSources(t *testing.T) {
	cs := load(t, "plan-destructive.json")
	for _, c := range cs.Changes {
		if strings.HasPrefix(c.Address, "data.") {
			t.Errorf("data source %s should not be reported", c.Address)
		}
		if c.Action == ActionNoOp {
			t.Errorf("no-op %s should not be reported", c.Address)
		}
	}
	if c := cs.Counts(); c.NoOp != 0 || c.Read != 0 {
		t.Errorf("counts should exclude no-op and read, got %+v", c)
	}
}

func TestCreateBeforeDestroyIsAReplacement(t *testing.T) {
	// aws_instance.worker uses ["create","delete"], which Terraform emits for
	// create_before_destroy. It is still a replacement.
	cs := load(t, "plan-destructive.json")
	for _, c := range cs.Changes {
		if c.Address == "aws_instance.worker" {
			if c.Action != ActionReplace {
				t.Fatalf("got %q, want replace", c.Action)
			}
			return
		}
	}
	t.Fatal("aws_instance.worker missing from the change set")
}

func TestCounts(t *testing.T) {
	c := load(t, "plan-destructive.json").Counts()
	if c.Destroy != 3 || c.Replace != 2 || c.Update != 1 || c.Create != 1 {
		t.Fatalf("got %+v", c)
	}
	if c.Total() != 7 {
		t.Fatalf("total = %d, want 7", c.Total())
	}
}

func TestDestructive(t *testing.T) {
	d := load(t, "plan-destructive.json").Destructive()
	if len(d) != 5 {
		t.Fatalf("got %d destructive changes, want 5", len(d))
	}
	for _, c := range d {
		if !c.Action.Destructive() {
			t.Errorf("%s has action %s", c.Address, c.Action)
		}
	}
}

func TestReplacePathsAreNamesOnly(t *testing.T) {
	cs := load(t, "plan-destructive.json")
	for _, c := range cs.Changes {
		if c.Address != "module.data.aws_db_instance.primary" {
			continue
		}
		want := []string{"availability_zone", "engine_version"}
		if len(c.ReplacePaths) != len(want) {
			t.Fatalf("got %v, want %v", c.ReplacePaths, want)
		}
		for i := range want {
			if c.ReplacePaths[i] != want[i] {
				t.Fatalf("got %v, want %v", c.ReplacePaths, want)
			}
		}
		if c.ActionReason != "replace_because_cannot_update" {
			t.Errorf("action reason = %q", c.ActionReason)
		}
		if c.Provider != "aws" {
			t.Errorf("provider = %q, want aws", c.Provider)
		}
		return
	}
	t.Fatal("database resource missing from the change set")
}

// TestNoAttributeValuesSurviveParsing is the privacy guarantee of the whole
// product: a plan containing a password must produce a change set that cannot
// possibly carry it, whatever we later serialise.
func TestNoAttributeValuesSurviveParsing(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plan-destructive.json"))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "this-value-must-never-be-parsed"
	if !strings.Contains(string(raw), secret) {
		t.Fatal("fixture no longer contains the canary value; the test is meaningless")
	}
	cs, err := ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs.Changes {
		blob := c.Address + c.Type + c.Name + c.Module + c.Provider + c.ActionReason + strings.Join(c.ReplacePaths, "")
		if strings.Contains(blob, secret) || strings.Contains(blob, "ami-000") || strings.Contains(blob, "secret") {
			t.Fatalf("attribute value leaked into change %s: %q", c.Address, blob)
		}
	}
}

func TestParseEmptyPlan(t *testing.T) {
	cs := load(t, "plan-empty.json")
	if len(cs.Changes) != 0 {
		t.Fatalf("got %d changes, want 0", len(cs.Changes))
	}
	if cs.Counts().Total() != 0 {
		t.Fatal("total should be zero")
	}
}

func TestParseRejectsNonPlanDocuments(t *testing.T) {
	for _, in := range []string{`{"hello":"world"}`, `[]`, `not json`} {
		if _, err := ParseBytes([]byte(in)); err == nil {
			t.Errorf("input %q parsed without error", in)
		}
	}
}

func TestCollapse(t *testing.T) {
	cases := map[string]Action{
		"":                     ActionNoOp,
		"no-op":                ActionNoOp,
		"create":               ActionCreate,
		"update":               ActionUpdate,
		"delete":               ActionDestroy,
		"read":                 ActionRead,
		"delete,create":        ActionReplace,
		"create,delete":        ActionReplace,
		"create,update":        ActionNoOp,
		"delete,create,delete": ActionNoOp,
	}
	for in, want := range cases {
		var actions []string
		if in != "" {
			actions = strings.Split(in, ",")
		}
		if got := collapse(actions); got != want {
			t.Errorf("collapse(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortProvider(t *testing.T) {
	if got := shortProvider("registry.terraform.io/hashicorp/aws"); got != "aws" {
		t.Errorf("got %q", got)
	}
	if got := shortProvider("aws"); got != "aws" {
		t.Errorf("got %q", got)
	}
	if got := shortProvider(""); got != "" {
		t.Errorf("got %q", got)
	}
}
