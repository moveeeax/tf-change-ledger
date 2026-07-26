package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bin is the compiled CLI under test. Building it once and running it for real
// is the only way to prove the exit codes CI depends on.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tfcl-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	bin = filepath.Join(dir, "tf-change-ledger")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout string
	stderr string
	code   int
}

func run(t *testing.T, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func testdata(name string) string { return filepath.Join("..", "..", "testdata", name) }

func TestSummarizeMarkdown(t *testing.T) {
	r := run(t, "summarize", "-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"), "-author", "alice")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"## Terraform change summary", "### Destroy", "Protected-resource gate", "/ledger approve"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestSummarizeJSON(t *testing.T) {
	r := run(t, "summarize", "-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"), "-format", "json", "-author", "alice")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	var doc struct {
		Counts struct {
			Destroy int `json:"destroy"`
			Replace int `json:"replace"`
		} `json:"counts"`
		Decision struct {
			Blocked   bool `json:"blocked"`
			Protected []struct {
				Address string `json:"address"`
			} `json:"protected"`
		} `json:"decision"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if doc.Counts.Destroy != 3 || doc.Counts.Replace != 2 {
		t.Errorf("counts = %+v", doc.Counts)
	}
	if !doc.Decision.Blocked || len(doc.Decision.Protected) != 3 {
		t.Errorf("decision = %+v", doc.Decision)
	}
}

func TestGateExitCodes(t *testing.T) {
	blocked := run(t, "gate", "-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"), "-author", "alice")
	if blocked.code != exitBlocked {
		t.Fatalf("blocked gate exited %d, want %d (%s)", blocked.code, exitBlocked, blocked.stderr)
	}

	released := run(t, "gate", "-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"),
		"-author", "alice", "-ack", "bob:/ledger approve")
	if released.code != 0 {
		t.Fatalf("acknowledged gate exited %d: %s", released.code, released.stderr)
	}

	additive := run(t, "gate", "-plan", testdata("plan-additive.json"), "-config", testdata(".tf-ledger.yml"), "-author", "alice")
	if additive.code != 0 {
		t.Fatalf("additive plan exited %d: %s", additive.code, additive.stderr)
	}
}

func TestGateReadsAcksFromAFile(t *testing.T) {
	dir := t.TempDir()
	acks := filepath.Join(dir, "acks.json")
	body := `[{"actor":"bob","body":"I checked the snapshot\n/ledger approve"}]`
	if err := os.WriteFile(acks, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := run(t, "gate", "-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"),
		"-author", "alice", "-ack-file", acks, "-format", "json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, `"bob"`) {
		t.Errorf("approver not reported:\n%s", r.stdout)
	}
}

func TestRecordExportVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	led := filepath.Join(dir, "ledger.jsonl")

	rec := run(t, "record",
		"-plan", testdata("plan-destructive.json"),
		"-config", testdata(".tf-ledger.yml"),
		"-ledger", led,
		"-repo", "acme/platform", "-pr", "42", "-commit", "0bada55deadbeef",
		"-author", "alice", "-approver", "carol", "-ack", "bob:/ledger approve",
		"-at", "2026-03-14T10:00:00Z")
	if rec.code != 0 {
		t.Fatalf("record exited %d: %s", rec.code, rec.stderr)
	}
	// 3 destroys + 2 replacements, creates and updates excluded by default.
	if n := strings.Count(rec.stdout, "recorded #"); n != 5 {
		t.Fatalf("recorded %d entries, want 5:\n%s", n, rec.stdout)
	}

	if v := run(t, "verify", "-ledger", led); v.code != 0 {
		t.Fatalf("verify exited %d: %s", v.code, v.stderr)
	}

	exp := run(t, "export", "-ledger", led, "-from", "2026-03-01", "-to", "2026-03-31")
	if exp.code != 0 {
		t.Fatalf("export exited %d: %s", exp.code, exp.stderr)
	}
	rows, err := csv.NewReader(strings.NewReader(exp.stdout)).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("got %d CSV rows including the header, want 6", len(rows))
	}
	header := strings.Join(rows[0], ",")
	for _, col := range []string{"repo", "pr", "address", "action", "author", "approvers", "acknowledged", "entry_hash"} {
		if !strings.Contains(header, col) {
			t.Errorf("CSV header missing %q: %s", col, header)
		}
	}
	body := exp.stdout
	if !strings.Contains(body, "bob carol") {
		t.Error("approvers should merge reviewers and acknowledgers")
	}
	for _, secret := range []string{"this-value-must-never-be-parsed", "ami-000"} {
		if strings.Contains(body, secret) {
			t.Errorf("export leaked %q", secret)
		}
	}

	// Out-of-range dates return only the header.
	empty := run(t, "export", "-ledger", led, "-from", "2026-04-01")
	if strings.Count(strings.TrimSpace(empty.stdout), "\n") != 0 {
		t.Errorf("date filter did not exclude everything:\n%s", empty.stdout)
	}

	// A second record call appends rather than replacing.
	rec2 := run(t, "record",
		"-plan", testdata("plan-additive.json"),
		"-config", testdata(".tf-ledger.yml"),
		"-ledger", led, "-all",
		"-repo", "acme/platform", "-pr", "43", "-commit", "feedface",
		"-author", "alice", "-at", "2026-03-15T10:00:00Z")
	if rec2.code != 0 {
		t.Fatalf("second record exited %d: %s", rec2.code, rec2.stderr)
	}
	all := run(t, "export", "-ledger", led, "-format", "json")
	var entries []struct {
		Seq int `json:"seq"`
		PR  int `json:"pr"`
	}
	if err := json.Unmarshal([]byte(all.stdout), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 7 || entries[6].Seq != 7 || entries[6].PR != 43 {
		t.Fatalf("append did not extend the chain: %+v", entries)
	}
	if v := run(t, "verify", "-ledger", led); v.code != 0 {
		t.Fatalf("verify after append exited %d: %s", v.code, v.stderr)
	}
}

func TestVerifyFailsOnATamperedLedger(t *testing.T) {
	dir := t.TempDir()
	led := filepath.Join(dir, "ledger.jsonl")
	if r := run(t, "record",
		"-plan", testdata("plan-destructive.json"), "-config", testdata(".tf-ledger.yml"),
		"-ledger", led, "-repo", "acme/platform", "-pr", "42", "-commit", "0bada55",
		"-author", "alice", "-quiet"); r.code != 0 {
		t.Fatalf("record exited %d: %s", r.code, r.stderr)
	}
	raw, err := os.ReadFile(led)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"author":"alice"`, `"author":"mallory"`, 1)
	if tampered == string(raw) {
		t.Fatal("could not tamper with the fixture")
	}
	if err := os.WriteFile(led, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	v := run(t, "verify", "-ledger", led)
	if v.code != exitBlocked {
		t.Fatalf("verify exited %d, want %d", v.code, exitBlocked)
	}
	if !strings.Contains(v.stderr, "verification failed") {
		t.Errorf("stderr = %q", v.stderr)
	}
}

func TestRecordSkipsPlansWithNoDestructiveChange(t *testing.T) {
	dir := t.TempDir()
	led := filepath.Join(dir, "ledger.jsonl")
	r := run(t, "record", "-plan", testdata("plan-additive.json"), "-config", testdata(".tf-ledger.yml"),
		"-ledger", led, "-repo", "acme/platform", "-pr", "44", "-commit", "abc123", "-author", "alice")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "nothing to record") {
		t.Errorf("stdout = %q", r.stdout)
	}
	if _, err := os.Stat(led); !os.IsNotExist(err) {
		t.Error("an empty record must not create a ledger file")
	}
}

func TestRecordRequiresProvenance(t *testing.T) {
	dir := t.TempDir()
	r := run(t, "record", "-plan", testdata("plan-destructive.json"),
		"-ledger", filepath.Join(dir, "l.jsonl"), "-repo", "acme/platform", "-author", "alice", "-commit", "abc")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1", r.code)
	}
	if !strings.Contains(r.stderr, "-pr is required") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

func TestMissingPolicyFileIsPermissiveOnlyAtTheDefaultPath(t *testing.T) {
	// Explicit path that does not exist: an error, because the user asked for it.
	r := run(t, "gate", "-plan", testdata("plan-destructive.json"), "-config", filepath.Join(t.TempDir(), "nope.yml"))
	if r.code != 1 {
		t.Fatalf("exit %d, want 1: %s", r.code, r.stderr)
	}
	// Default path absent: nothing is protected, so the gate passes.
	cmd := exec.Command(bin, "gate", "-plan", "plan.json")
	cmd.Dir = t.TempDir()
	data, err := os.ReadFile(testdata("plan-destructive.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmd.Dir, "plan.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gate without a policy failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "no protected resources touched") {
		t.Errorf("output = %q", out)
	}
}

func TestUnknownCommand(t *testing.T) {
	r := run(t, "frobnicate")
	if r.code != 1 || !strings.Contains(r.stderr, "unknown command") {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
}

func TestVersion(t *testing.T) {
	r := run(t, "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "tf-change-ledger ") {
		t.Fatalf("exit %d, stdout %q", r.code, r.stdout)
	}
}
