// Command tf-change-ledger turns a Terraform plan into a risk-ordered review
// comment, enforces a protected-resource gate, and appends a tamper-evident
// record of who approved every destructive change.
//
// It never runs `terraform plan` and never reads state: it consumes the JSON
// your own CI already produces with `terraform show -json`.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `tf-change-ledger — a record of who approved every destructive Terraform change.

Usage:
  tf-change-ledger <command> [flags]

Commands:
  summarize   Render a risk-ordered Markdown summary of a plan
  gate        Check a plan against .tf-ledger.yml and the acknowledgements given
  record      Append the changes of a merged pull request to the ledger
  export      Export ledger entries as CSV or JSON
  verify      Re-walk the ledger hash chain and report tampering
  version     Print the version

Run "tf-change-ledger <command> -h" for the flags of a command.

Exit codes:
  0  success
  1  usage or runtime error
  2  the gate is blocked, or the ledger chain is broken
`

// exitBlocked is returned when a plan is held for acknowledgement or a ledger
// fails verification. CI can then distinguish "your change needs a human" from
// "the tool broke".
const exitBlocked = 2

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "summarize", "summarise":
		err = runSummarize(args)
	case "gate":
		err = runGate(args)
	case "record":
		err = runRecord(args)
	case "export":
		err = runExport(args)
	case "verify":
		err = runVerify(args)
	case "version", "--version", "-v":
		fmt.Println("tf-change-ledger " + version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
	if err != nil {
		if be, ok := err.(blockedError); ok {
			fmt.Fprintln(os.Stderr, be.Error())
			os.Exit(exitBlocked)
		}
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

// blockedError marks a policy outcome rather than a failure of the tool.
type blockedError struct{ msg string }

func (e blockedError) Error() string { return e.msg }
