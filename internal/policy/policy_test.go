package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) *Policy {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", ".tf-ledger.yml"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	p, err := Load(f)
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}
	return p
}

// planChanges mirrors the fixture plan, so the policy tests exercise the same
// resources the parser tests do without importing the plan package.
func planChanges() []Changeish {
	return []Changeish{
		{Address: "aws_iam_role.legacy", Type: "aws_iam_role", Action: "destroy"},
		{Address: "aws_s3_bucket.logs", Type: "aws_s3_bucket", Action: "destroy"},
		{Address: "module.sandbox.aws_db_instance.scratch", Type: "aws_db_instance", Action: "destroy"},
		{Address: "aws_instance.worker", Type: "aws_instance", Action: "replace"},
		{Address: "module.data.aws_db_instance.primary", Type: "aws_db_instance", Action: "replace"},
		{Address: "aws_security_group.web", Type: "aws_security_group", Action: "update"},
		{Address: "aws_s3_bucket.assets", Type: "aws_s3_bucket", Action: "create"},
	}
}

func TestLoadFixture(t *testing.T) {
	p := loadFixture(t)
	if p.Acknowledgement != "/ledger approve" {
		t.Errorf("acknowledgement = %q", p.Acknowledgement)
	}
	if p.SelfApproval {
		t.Error("self_approval should default to false in the fixture")
	}
	if len(p.Protected) != 3 {
		t.Fatalf("got %d rules, want 3", len(p.Protected))
	}
}

func TestEvaluateMatchesTheRightResources(t *testing.T) {
	p := loadFixture(t)
	hits := p.Evaluate(planChanges())

	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.Address+"/"+h.Rule)
	}
	want := []string{
		"aws_iam_role.legacy/identity",
		"aws_s3_bucket.logs/buckets-and-state",
		"module.data.aws_db_instance.primary/databases",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExemptWins(t *testing.T) {
	p := loadFixture(t)
	// module.sandbox.aws_db_instance.scratch is a destroy of a protected type,
	// but the sandbox module is exempt.
	for _, h := range p.Evaluate(planChanges()) {
		if strings.HasPrefix(h.Address, "module.sandbox.") {
			t.Fatalf("exempt address %s was gated", h.Address)
		}
	}
	if !p.Exempted("module.sandbox.aws_s3_bucket.tmp") {
		t.Error("module.sandbox.* should match nested addresses")
	}
	if p.Exempted("module.sandboxed.aws_s3_bucket.tmp") {
		t.Error("module.sandbox.* must not match module.sandboxed.*")
	}
}

func TestCreateOfProtectedTypeIsNotGated(t *testing.T) {
	p := loadFixture(t)
	for _, h := range p.Evaluate(planChanges()) {
		if h.Address == "aws_s3_bucket.assets" {
			t.Fatal("creating a bucket must not require acknowledgement")
		}
	}
}

func TestGlobMatchIsExactWithoutMetacharacters(t *testing.T) {
	r := Rule{Types: []string{"aws_db_instance"}, Actions: []string{"delete"}}
	if r.Match("aws_db_instance_role.x", "aws_db_instance_role", "destroy") {
		t.Error("aws_db_instance must not match aws_db_instance_role")
	}
	if !r.Match("aws_db_instance.x", "aws_db_instance", "destroy") {
		t.Error("exact type should match")
	}

	g := Rule{Types: []string{"aws_s3_bucket*"}, Actions: []string{"delete"}}
	if !g.Match("aws_s3_bucket_policy.x", "aws_s3_bucket_policy", "destroy") {
		t.Error("trailing glob should match the policy sub-type")
	}
}

func TestDeleteIsASynonymOfDestroy(t *testing.T) {
	r := Rule{Types: []string{"aws_s3_bucket"}, Actions: []string{"delete"}}
	if !r.Match("aws_s3_bucket.x", "aws_s3_bucket", "destroy") {
		t.Fatal("YAML 'delete' should match the parser's 'destroy'")
	}
}

func TestRuleDefaultActionsAreDestructive(t *testing.T) {
	r := Rule{Types: []string{"aws_s3_bucket"}}
	if !r.Match("aws_s3_bucket.x", "aws_s3_bucket", "destroy") {
		t.Error("destroy should match by default")
	}
	if !r.Match("aws_s3_bucket.x", "aws_s3_bucket", "replace") {
		t.Error("replace should match by default")
	}
	if r.Match("aws_s3_bucket.x", "aws_s3_bucket", "update") {
		t.Error("update should not match by default")
	}
}

func TestGateBlocksUntilAcknowledged(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate(planChanges(), "alice", nil)
	if !d.Blocked {
		t.Fatal("gate should block with no acknowledgement")
	}
	if len(d.Protected) != 3 {
		t.Fatalf("got %d protected changes, want 3", len(d.Protected))
	}
	if !strings.Contains(d.Reason, "@alice") {
		t.Errorf("reason should name the author: %q", d.Reason)
	}
}

func TestGateReleasesOnExactAcknowledgement(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate(planChanges(), "alice", []Acknowledgement{
		{Actor: "bob", Body: "looks fine to me\n/ledger approve\n"},
	})
	if d.Blocked {
		t.Fatalf("gate should be released: %s", d.Reason)
	}
	if len(d.Approvers) != 1 || d.Approvers[0] != "bob" {
		t.Fatalf("approvers = %v", d.Approvers)
	}
}

func TestGateRejectsSelfApprovalByDefault(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate(planChanges(), "alice", []Acknowledgement{
		{Actor: "Alice", Body: "/ledger approve"},
	})
	if !d.Blocked {
		t.Fatal("the author must not be able to acknowledge their own change")
	}
	p.SelfApproval = true
	if d := p.Gate(planChanges(), "alice", []Acknowledgement{{Actor: "alice", Body: "/ledger approve"}}); d.Blocked {
		t.Fatal("self approval should be allowed when enabled")
	}
}

func TestGateIgnoresQuotedAndPartialAcknowledgements(t *testing.T) {
	p := loadFixture(t)
	for _, body := range []string{
		"> /ledger approve\nare you sure about this?",
		"I would /ledger approve if the backup existed",
		"/ledger approve later",
		"ledger approve",
		"",
	} {
		d := p.Gate(planChanges(), "alice", []Acknowledgement{{Actor: "bob", Body: body}})
		if !d.Blocked {
			t.Errorf("body %q should not release the gate", body)
		}
	}
}

func TestGateIsCaseInsensitiveOnTheAckLine(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate(planChanges(), "alice", []Acknowledgement{{Actor: "bob", Body: "  /LEDGER Approve  "}})
	if d.Blocked {
		t.Fatalf("case and surrounding whitespace should be tolerated: %s", d.Reason)
	}
}

func TestGateDeduplicatesApprovers(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate(planChanges(), "alice", []Acknowledgement{
		{Actor: "bob", Body: "/ledger approve"},
		{Actor: "Bob", Body: "/ledger approve"},
		{Actor: "carol", Body: "/ledger approve"},
	})
	if len(d.Approvers) != 2 {
		t.Fatalf("approvers = %v, want bob and carol once each", d.Approvers)
	}
}

func TestGatePassesWhenNothingProtectedIsTouched(t *testing.T) {
	p := loadFixture(t)
	d := p.Gate([]Changeish{
		{Address: "aws_sqs_queue.jobs", Type: "aws_sqs_queue", Action: "create"},
	}, "alice", nil)
	if d.Blocked {
		t.Fatal("an additive plan must never block")
	}
	if d.Reason != "no protected resources touched" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestLoadRejectsBadPolicies(t *testing.T) {
	cases := map[string]string{
		"unknown version": "version: 2\n",
		"empty rule":      "version: 1\nprotected:\n  - name: nothing\n",
		"unknown action":  "version: 1\nprotected:\n  - name: db\n    types: [aws_db_instance]\n    actions: [obliterate]\n",
		"unknown field":   "version: 1\nprotcted: []\n",
		"not yaml":        "version: [1\n",
	}
	for name, doc := range cases {
		if _, err := Load(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadEmptyDocumentIsPermissive(t *testing.T) {
	p, err := Load(strings.NewReader(""))
	if err != nil {
		t.Fatalf("empty policy should load: %v", err)
	}
	if p.Acknowledgement != DefaultAcknowledgement {
		t.Errorf("acknowledgement = %q", p.Acknowledgement)
	}
	if d := p.Gate(planChanges(), "alice", nil); d.Blocked {
		t.Fatal("a policy with no rules must not block anything")
	}
}
