# tf-change-ledger

A tamper-evident record of **who approved every destructive Terraform change**.

Reviewers get a risk-ordered plan summary. Protected resources cannot merge until
somebody acknowledges them in writing. Every destroy and replacement is then
appended to a hash-chained ledger you can hand to an auditor as CSV.

The tool never runs `terraform plan`, never reads state, and never sees your
variable values. It consumes the plan JSON your own CI already produces and keeps
**only resource addresses, types and actions** — a constraint enforced by the
parser's type definitions and covered by a test.

---

## Status

Early. Everything in this repository works end to end and is tested:

| | |
|---|---|
| Plan JSON parser, risk ordering, replacement detection | done |
| `.tf-ledger.yml` protected-resource gate | done |
| Sticky pull request comment (Markdown) | done |
| Append-only, hash-chained ledger + `verify` | done |
| CSV / JSON export with date, repo and action filters | done |
| GitHub App, hosted API, Postgres, org dashboard, billing | not here — see [Not built yet](#not-built-yet) |

## Install

```sh
go install github.com/moveeeax/tf-change-ledger/cmd/tf-change-ledger@latest
```

Go 1.24 or newer. No runtime dependencies.

## Usage example

Runs against the fixtures committed in `testdata/`, so pasting this into a clone
produces exactly the output shown.

```sh
git clone https://github.com/moveeeax/tf-change-ledger.git
cd tf-change-ledger
go build -o tf-change-ledger ./cmd/tf-change-ledger
export TFCL="$PWD/tf-change-ledger" REPO="$PWD"

# a scratch directory holding the example plan and policy
mkdir -p /tmp/tfcl-demo && cd /tmp/tfcl-demo
cp "$REPO/testdata/plan-destructive.json" plan.json
cp "$REPO/testdata/.tf-ledger.yml" .
```

**1. Summarise the plan.** Destroys first, always.

```sh
"$TFCL" summarize -plan plan.json -author alice -dir envs/prod
```

```markdown
## Terraform change summary

**3 resource(s) will be destroyed.** Review the destroy list before approving.

| | count |
|---|---:|
| destroy | 3 |
| replace | 2 |
| update | 1 |
| create | 1 |

### Destroy — 3

| resource | type | note |
|---|---|---|
| `aws_iam_role.legacy` | `aws_iam_role` | **protected** (identity): IAM removals have taken production down before; the resource block was removed from the configuration |
| `aws_s3_bucket.logs` | `aws_s3_bucket` | **protected** (buckets-and-state): Object storage deletions are not recoverable once versioning lapses |
| `module.sandbox.aws_db_instance.scratch` | `aws_db_instance` |  |

### Replace (destroy then create) — 2

| resource | type | note |
|---|---|---|
| `aws_instance.worker` | `aws_instance` | resource is tainted |
| `module.data.aws_db_instance.primary` | `aws_db_instance` | **protected** (databases): Destroying an RDS instance loses data that backups may not cover; forced by `availability_zone`, `engine_version`; an attribute cannot be updated in place |
...
```

The comment is prefixed with `<!-- tf-change-ledger:summary -->` so a later run
updates it in place instead of adding a second one.

**2. Gate the merge.** Exit code `2` means "a human has to say something", as
distinct from `1`, which means the tool broke.

```sh
"$TFCL" gate -plan plan.json -author alice; echo "exit=$?"
```

```
protected  destroy  aws_iam_role.legacy  (rule identity)
protected  destroy  aws_s3_bucket.logs  (rule buckets-and-state)
protected  replace  module.data.aws_db_instance.primary  (rule databases)
3 protected change(s) await acknowledgement; an approver other than @alice must comment "/ledger approve"
gate blocked
exit=2
```

`module.sandbox.aws_db_instance.scratch` is a destroy of a protected type, but the
sandbox module is exempt in the policy, so it is not gated.

Now with an acknowledgement from somebody who is not the author:

```sh
"$TFCL" gate -plan plan.json -author alice -ack "bob:/ledger approve"; echo "exit=$?"
```

```
protected  destroy  aws_iam_role.legacy  (rule identity)
protected  destroy  aws_s3_bucket.logs  (rule buckets-and-state)
protected  replace  module.data.aws_db_instance.primary  (rule databases)
3 protected change(s) acknowledged by bob
exit=0
```

**3. Record on merge.** One entry per destroyed or replaced resource.

```sh
"$TFCL" record -plan plan.json -ledger ledger.jsonl \
  -repo acme/platform -pr 128 -commit 4f1c0de9c0ffee \
  -author alice -approver carol -ack "bob:/ledger approve"
```

```
recorded #1 destroy  aws_iam_role.legacy  approvers=bob,carol  hash=b91686d6b3bd
recorded #2 destroy  aws_s3_bucket.logs  approvers=bob,carol  hash=c96989204615
recorded #3 destroy  module.sandbox.aws_db_instance.scratch  approvers=bob,carol  hash=f2ca46e11fd6
recorded #4 replace  aws_instance.worker  approvers=bob,carol  hash=ebd37d00b106
recorded #5 replace  module.data.aws_db_instance.primary  approvers=bob,carol  hash=220cf000d09f
```

(Hashes cover the timestamp, so yours will differ unless you pass `-at`.)

**4. Export for the auditor.**

```sh
"$TFCL" export -ledger ledger.jsonl -protected-only
```

```csv
seq,recorded_at,repo,pr,commit,address,resource_type,action,author,approvers,protected,protected_by,acknowledged,entry_hash
1,2026-07-26T01:36:27Z,acme/platform,128,4f1c0de9c0ffee,aws_iam_role.legacy,aws_iam_role,destroy,alice,bob carol,true,identity,true,b91686d6…
2,2026-07-26T01:36:27Z,acme/platform,128,4f1c0de9c0ffee,aws_s3_bucket.logs,aws_s3_bucket,destroy,alice,bob carol,true,buckets-and-state,true,c9698920…
5,2026-07-26T01:36:27Z,acme/platform,128,4f1c0de9c0ffee,module.data.aws_db_instance.primary,aws_db_instance,replace,alice,bob carol,true,databases,true,220cf000…
```

Add `-from 2026-01-01 -to 2026-03-31` for a quarter, `-repo` for one repository,
`-destructive-only` to drop everything but destroys and replacements.

**5. Prove nobody edited it.**

```sh
"$TFCL" verify -ledger ledger.jsonl

# now tamper with history and try again (GNU sed: drop the '' after -i)
sed -i '' 's/"author":"alice"/"author":"mallory"/' ledger.jsonl
"$TFCL" verify -ledger ledger.jsonl; echo "exit=$?"
```

```
ok: 5 entries, chain intact, head 220cf000d09f
ledger verification failed: entry 1 (aws_iam_role.legacy): content hash a7ade29f0d96 does not match recorded b91686d6b3bd (the entry was edited)
exit=2
```

An entry's hash covers every recorded fact — repo, PR, commit, author, approvers,
address, action, timestamp — plus the hash of the entry before it. Editing a row
in place, deleting one, or reordering the file all break the chain, and `verify`
names the entry where it broke. There is no update and no delete: correcting a
mistake means appending, exactly as in a paper ledger.

## Producing the inputs

The plan JSON is whatever your pipeline already has:

```sh
terraform plan -out=tfplan
terraform show -json tfplan > plan.json
```

Acknowledgements are a JSON list of `{actor, body}`, one `gh` call away. `gh` uses
your credentials; this tool needs none of its own:

```sh
gh api "repos/$REPO/issues/$PR_NUMBER/comments" \
  --jq '[.[] | {actor: .user.login, body: .body}]' > acks.json

tf-change-ledger gate -plan plan.json -author "$PR_AUTHOR" -ack-file acks.json
```

## Configuration

`.tf-ledger.yml` at the root of the Terraform repository. If the file is absent,
nothing is protected and nothing is gated.

```yaml
version: 1

# The exact line an approver must comment. Matched case-insensitively on a line
# of its own; quoted text (a "> " reply) never counts.
acknowledgement: "/ledger approve"

# Leave false so the author of a destructive change cannot sign it off alone.
self_approval: false

protected:
  - name: databases
    reason: "Destroying an RDS instance loses data that backups may not cover"
    types: [aws_db_instance, aws_rds_cluster]
    actions: [delete, replace]      # default is [delete, replace]

  - name: buckets
    types: [aws_s3_bucket, google_storage_bucket]
    actions: [delete]

  - name: prod-network
    addresses: ["module.prod.aws_vpc.*"]

exempt:
  - module.sandbox.*
```

Globs are shell-style over the dotted address or type. A pattern with no `*` is an
exact match, so `aws_db_instance` does **not** catch `aws_db_instance_role`.
Unknown keys and unknown actions are rejected at load time rather than silently
ignored — a typo in a protection rule is a security bug.

## Commands

| command | what it does | exit codes |
|---|---|---|
| `summarize` | Risk-ordered Markdown comment, or `-format json` | 0, 1 |
| `gate` | Evaluate `.tf-ledger.yml` against the plan and the acknowledgements | 0, 1, **2 = blocked** |
| `record` | Append the merged pull request's destructive changes to the ledger | 0, 1 |
| `export` | CSV or JSON, filtered by date range, repo, action, protection | 0, 1 |
| `verify` | Re-walk the hash chain | 0, 1, **2 = broken** |

Run `tf-change-ledger <command> -h` for the flags of a command.

## What never leaves your runner

`internal/plan` declares no fields for `change.before`, `change.after`,
`variables`, `prior_state` or `configuration`. `encoding/json` drops what is not
declared, so attribute values are not filtered out later — they are never decoded
at all. `testdata/plan-destructive.json` carries a canary value and
`TestNoAttributeValuesSurviveParsing` fails if it ever reaches a change set.

## Not built yet

Each of these needs an account, a credential or a paid service, so none of it is in
this repository:

- **GitHub App** — webhook receiver, JWT → installation token, check runs, sticky
  comment upsert. The CLI is deliberately the whole of the decision logic, so the
  App is transport over these packages.
- **Hosted API and Postgres ledger** — `internal/ledger` is the reference
  implementation. The Postgres table takes the same `Entry` shape and the same
  chaining, so the two can be recomputed and compared.
- **Org dashboard** and its CSV download button.
- **Paddle checkout** and org-scoped licence checks.

## Development

```sh
go test ./...
go vet ./...
gofmt -l .
```

## Licence

Not decided yet. Until a `LICENSE` file exists in this repository, all rights are
reserved.
