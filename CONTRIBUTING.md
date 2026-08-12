# Contributing to tellury

Contributions are welcome. There's no CLA and nothing to sign — open a pull request.

Rules are the most useful thing you can add. `tellury` ships three, and the interesting ones
are the thousands nobody has written yet: every team knows a way their cloud bill leaks that
no vendor dashboard catches. The rule interface exists to make those easy to write.

## Adding a rule

A rule is a Go package under `pkg/rules/<provider>/<service>/<rule_id>/` implementing the
`rules.Rule` interface. Look at `detached_disk` for the simplest complete example.

What a good rule does:

- **Prices its finding.** A finding without a defensible dollar figure is noise. Show the
  inputs — size, type, region, rate — and let the cost math be checkable.
- **Resolves to true or false.** Thresholds are explicit numbers with a stated reason, not
  tuned constants nobody can justify later.
- **Skips rather than guesses.** If the data needed isn't there, record a skip with a reason.
  `--explain-skips` should tell an operator the difference between "checked, it's fine" and
  "couldn't check". Missing data is never zero.
- **Ships a fixture.** Add a Cloud Asset Inventory JSON fixture that triggers your rule, so
  `tellury scan --fixture` reproduces the finding with no cloud account. This is how reviewers
  verify your work, and it's why offline mode exists.
- **Honours `tellury-exempt=true`** on a resource, checked before anything else.

## Other contributions

Bug reports, docs, a new cloud provider, or a fix from the "Known issues" list in the README
are all just as welcome. For anything large, open an issue first so we can agree on the shape
before you spend the time.

Use the official SDK when talking to a cloud provider — hand-rolling auth, retries, and
pagination is where the bugs live. Other dependencies just need to earn their place.

## Before opening a PR

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

All four should be clean. Add a test for anything you change, and a fixture-based test for a
new rule.

## Cutting a release

The steps below are the process, not a suggestion — every one of them has caught
something real.

1. **Write the changelog entry** for the net change against the last tag, not the churn
   within it. A feature added and removed between releases never shipped and does not
   belong in it. Say plainly when behaviour changes, and lead the Fixed section with
   anything that affects figures a user already relies on.
2. **Run every command the docs claim.** Not copied from `--help` — executed. A broken
   `graph export` example shipped once because it was written from a flag list, and a
   README quick-start went stale for two releases after the column layout changed.
3. **Check internal links and anchors.** Sections get renamed; the links to them do not.
4. `go build ./...`, `go vet ./...`, `go test ./...`.
5. **Run the live pricing token pin.** It needs real AWS credentials, so the default suite
   skips it and CI cannot:

   ```bash
   TELLURY_AWS_LIVE_PRICE_TEST=1 go test ./pkg/pricing/aws/ \
     -run TestCatalogPricer_LiveGetProductsTokenPinned -count=1 -v
   ```

   It fetches a real `GetProducts` response and asserts that every SKU token the catalogue
   derives still matches the tokens the rules query, and that all four price kinds resolve
   with `provenance=live_api` and a real region rather than `default`. A failure means AWS
   renamed an attribute; if that ships, the affected resources stop being priced.

   The first time anyone ran this it failed four of five lookups, which is how the gp3
   IOPS, throughput and static-IP defects were found. An opt-in test nobody runs is not a
   check — put it here so cutting a release is when it gets run.
6. **Reconcile one figure against a real invoice.** Every pricing defect in this project —
   four of them — was found this way and by nothing else. Two were invisible to the entire
   test suite; one had a test asserting the wrong arithmetic.
7. **Tag with an annotated message** saying what changed and who should upgrade, then push
   the branch and the tag.
8. **Watch the release workflow.** Pushing a `v*` tag runs `.github/workflows/release.yml`,
   which re-runs the tests, extracts the matching `CHANGELOG.md` section via
   `scripts/release-notes.sh`, and publishes cross-compiled archives plus `checksums.txt`
   through goreleaser. The release notes are the changelog section, not a generated commit
   list — if the section is missing the job fails rather than publishing empty notes.

   Tagging stays manual on purpose. Steps 5 and 6 need real credentials and a real invoice,
   so CI cannot run them, and a fully push-button release would quietly skip the two checks
   that have caught every pricing defect in this project.
9. **Verify the published artifact, not your working tree**: download the archive the release
   actually serves and run it.

   ```bash
   curl -sSL https://github.com/TypeOneLabs/tellury/releases/latest/download/tellury_linux_amd64.tar.gz \
     | tar xz tellury
   ./tellury version   # must report the tag, not "dev"
   ```

   Archive names carry no version so the documented URL never goes stale; the binary
   identifies itself instead. A binary reporting `dev` means the ldflags stamping broke.

## Licence

`tellury` is [Apache 2.0](LICENSE), and contributions are made under the same licence. That's
all — by opening a PR you're licensing your work under Apache 2.0, same as everything else in
the repository.
