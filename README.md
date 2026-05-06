# release-automation

Reconciler for cross-repo dependency bumps across the Rancher repositories
(`rancher`, `steve`, `apiserver`, `norman`, `webhook`, `remotedialer-proxy`,
`wrangler`, `lasso`, `dynamiclistener`, `remotedialer`).

## What it does

Runs as a GitHub Actions workflow in this repo. Triggered two ways:

- **`repository_dispatch`** (`event_type: tag-emitted`) fired from each source
  repo's `Release` workflow when a tag is created. Reacts within ~30s.
- **`schedule`** every 15 min as the safety net for upstream lib releases we
  don't directly trigger and to poll downstream PR state.

For each new upstream release the reconciler:

1. Walks `dependencies.yaml` to find downstream repos and target branches.
2. Opens bump PRs (`go get dep@version && go mod tidy`).
3. Maintains a tracker issue per `(dep, version)` that owns the operation's
   state (linked PRs, supersede chain) in a fenced metadata block in the body.

Humans review and merge PRs. The bot only opens them.

## Layout

```
.github/workflows/
  reconciler.yml      # schedule (cron) — full sweep
  tag-emitted.yml     # repository_dispatch — scoped to one release
cmd/reconciler/       # entrypoint
internal/
  config/             # dependencies.yaml + VERSION.md parsing
  github/             # GitHub API client
  drift/              # detect new upstream releases / dep drift
  pr/                 # open + supersede bump PRs
  tracker/            # tracker-issue lifecycle
  reconcile/          # the multi-pass loop tying the above together
dependencies.yaml     # the DAG + paired/independent classification
```

## Configuration

Credentials:

The workflows pull a GitHub App's `appId` and `privateKey` from Vault
(`secret/data/github/repo/rancher/${{ github.repository }}/github/pr-actions-write-app/credentials`)
and mint a short-lived installation token via
[`actions/create-github-app-token`](https://github.com/actions/create-github-app-token).
The `pr-actions-write-app` App must be installed on every managed repo with
`Contents: write`, `Pull requests: write`, `Issues: write`, and
`Metadata: read`. The Go entrypoint reads the token from the `GH_BOT_TOKEN`
env var.

## Pilot scope

- **Pilot 1**: steve → rancher (paired). See [`dependencies.yaml`](dependencies.yaml).
- **Pilot 2**: wrangler → rancher + steve (independent on `main`, notify-only
  on `release/*`).
