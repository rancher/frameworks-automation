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
   state (linked PRs, Slack thread `ts`, supersede chain) in a fenced metadata
   block in the body.
4. Posts to Slack on every state transition (PR opened, CI red, approved,
   merged, op complete) — replies stay in-thread via the persisted `ts`.

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
  slack/              # transition pings
  reconcile/          # the 4-pass loop tying the above together
dependencies.yaml     # the DAG + paired/independent classification
```

## Configuration

Repository secrets:

| Secret | Purpose |
|--------|---------|
| `GH_BOT_TOKEN` | PAT (or App token) with `repo`, `issues` on every managed repo. Used to open PRs, manage tracker issues, query PR state. |
| `SLACK_BOT_TOKEN` | Slack App token with `chat:write`. Used to post + reply in the notification channel. |
| `SLACK_CHANNEL_ID` | Channel ID (not name) where notifications land. |

For the pilot a PAT is acceptable. Switch to a GitHub App before expanding
beyond steve.

## Pilot scope

- **Pilot 1**: steve → rancher (paired). See [`dependencies.yaml`](dependencies.yaml).
- **Pilot 2**: wrangler → rancher + steve (independent on `main`, notify-only
  on `release/*`).
