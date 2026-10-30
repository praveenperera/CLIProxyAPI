# Fork maintenance

This fork tracks the latest release of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI). Releases are tags on upstream `main`, such as `v8.0.53`. The fork adds:

- Claude between-tools thinking ([docs/claude-between-tools.md](docs/claude-between-tools.md))
- Quota-aware chat routing with always-on session affinity, quota weights, ordered priority tiers and native Claude history preservation ([docs/quota-aware-chat-affinity.md](docs/quota-aware-chat-affinity.md))
- Same-account retries for brief upstream errors (`routing.transient-retries`)
- Quota history and per-session prompt cache usage reports in the v8 management API ([docs/management-api-v8.md](docs/management-api-v8.md))

The fork is kept as a short stack of commits on top of upstream and rebased, not merged, so the fork's changes stay readable as one diff against upstream.

## Keep upstream files nearly untouched

Every line the fork changes in a file that also exists upstream can conflict on the next rebase. New files never conflict. So:

- Put fork logic in fork-owned files in the same package. Leave a one-line hook in the upstream file: a call, a field, a route.
- Never reindent upstream code. Instead of `if fork { ... } else { <upstream> }`, return early into a fork function at the top so the upstream body stays byte-identical. The Claude executor's native-history path (`claude_executor_native*.go`) is the example.
- Never make gofmt realign upstream lines. Put fork fields in their own block after a blank line, embed a fork-owned struct, or assign after a composite literal.
- Put fork tests in fork-owned `_test.go` files, not appended to upstream test files.
- Prefer keeping upstream config fields and overriding their value in one place over deleting them. For example, `routing.session-affinity` is forced on in `internal/config/fork.go`, so upstream code and tests that use the field still apply unchanged.
- Some changes have to edit upstream logic in place, such as ordered routing across priority tiers or the transient-retry flow in the stream conductor. Keep those edits as small as the behavior allows.

`scripts/fork-footprint.sh` lists the fork's changes in upstream-owned files, largest first. It exits 1 if any of those changes are whitespace only. Run it before committing fork work, and check that its totals did not grow without a reason.

## Syncing with upstream

The fork follows upstream releases, not every commit on upstream `main`, so it only moves to code upstream has shipped.

```bash
git fetch origin --tags
tag=$(gh release view --repo router-for-me/CLIProxyAPI --json tagName -q .tagName)
git branch -f backup/master-pre-sync master   # cheap undo point
git rebase "$tag"                             # rerere remembers past resolutions
gofmt -l $(git diff --name-only --diff-filter=AM "$tag" -- '*.go')
go build ./... && go vet ./... && go test ./...
scripts/fork-footprint.sh "$tag"
git push --force-with-lease fork master
```

When a test fails after a rebase, run it on a plain worktree of the release tag before blaming the fork. Upstream sometimes ships failing or timing-sensitive tests.

When resolving a conflict, keep both sides when both sides only add lines. When upstream changes code next to a fork hook, take upstream's version and re-add the hook. Do not carry an old copy of upstream code forward.

Keep the stack at a few commits. Fold fixes to fork features into the existing commits (`git commit --fixup` then `git rebase -i --autosquash "$tag"`), because every extra commit is another possible rebase stop.
