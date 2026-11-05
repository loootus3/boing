# Fork: soonest-reset routing and local usage page

This fork adds two things on top of upstream CLIProxyAPI.

## `routing.strategy: soonest-reset`

Among the credentials that are available for a request (same priority tiers and cooldowns as
`fill-first`), the proxy spends the one whose **weekly** subscription window resets first, so
quota that is about to expire is used before quota that would carry on for longer.

- Credentials with any window at 100% (or `limit_reached`) are tried last.
- Credentials without usage data yet sit between the two groups.
- Usage data comes from `internal/subsusage`, which polls adaptively (see `/usage` below):
  - Claude: `GET https://api.anthropic.com/api/oauth/usage` (`five_hour`, `seven_day`)
  - Codex: `GET https://chatgpt.com/backend-api/wham/usage` (`rate_limit.primary_window`, `secondary_window`)

```yaml
routing:
  strategy: "soonest-reset"
```

Requests mirror the official management panel (Cli-Proxy-API-Management-Center): same URLs,
`anthropic-beta: oauth-2025-04-20` for Claude, `Chatgpt-Account-Id` (from the auth file or the
`id_token` claims) for Codex, and the plan from `/api/oauth/profile` for Claude.

## `/usage` page

`http://127.0.0.1:8317/usage` shows only Claude accounts with the used percentage and reset
time of each window, and has a `+ Claude` button that runs the OAuth login in the browser.
Codex is hidden on the page for now (the tracker still polls Codex credentials and the
soonest-reset selector still uses them).

The page and its `/usage/api/*` endpoints need no management key, but they only answer
requests from the loopback interface with a loopback `Host`, reject cross-site requests, and
require an `X-Usage-UI: 1` header on state-changing calls.

The page styling is adapted from T3 Code (MIT, Copyright (c) 2026 T3 Tools Inc.).
`/usage?demo=1` renders fake accounts for a quick look.

Usage limits are fetched one account at a time (2 s apart): accounts in use (a proxied request
in the last 15 minutes, or the one the selector spends next) every 120 s, idle ones every 10 minutes
(an account that has no windows yet after a transient failure, i.e. a 429 or a network error, is retried every 120 s too;
401, missing-token and other HTTP errors keep the 10 minute interval),
and a newly added account immediately. A round stops at the first HTTP 429, leaving the remaining
accounts untouched until the next tick. The page polls every 15 s while the tab is visible;
there is no manual refresh button. Anthropic rate-limits the usage endpoint per account: on an
HTTP 429 the tracker stops querying that account for 2 minutes, doubling on each further 429
(4, 8) up to 15 minutes, and clears the backoff on the first successful fetch. During the
backoff the last good windows (kept for up to 30 minutes) are shown with a muted note and the
time of the next attempt. The Claude plan is read from `/api/oauth/profile` at most every 6 hours
per account. The "Uso" section sums each provider's weekly usage across
its accounts.

The page, top to bottom:

- Hero: weekly capacity summed over the accounts, the account in use now (5h and weekly
  rings), and the runway ("Fôlego"): how long the capacity lasts at the recent burn rate,
  counting weekly resets.
- Next 7 days: projection of the summed weekly capacity, with the refill at each reset and
  the stretches with no limit left.
- Accounts: one card per account with the 5h, weekly and per-model windows, forecast,
  sparkline and the mode selector.
- Activity: tokens or requests per hour or day, stacked by account, with totals split into
  fresh input, cache read, cache write and output.
- Heatmap: average activity per weekday and hour over the last 4 weeks.

## Per-account usage mode

Each account has a mode, set from the `/usage` page and stored in its auth file:

| mode     | `usage_mode` | `priority` | effect                                              |
|----------|--------------|------------|-----------------------------------------------------|
| Normal   | `normal`     | `0`        | spent by soonest weekly reset                       |
| Por último | `last`     | `-100`     | only when no normal account is available            |
| Não usar | `off`        | `-1000`    | never spent by `soonest-reset`; usage still shown   |

With `routing.session-affinity: true`, a session already bound to an account keeps it until the
affinity TTL expires, even after the account is set to `off`. Other strategies treat `off` only
as the lowest priority tier.

## LAN access

`/usage` accepts requests from loopback, private (RFC 1918 / ULA) and link-local addresses, and
only for local Host names (IP literals, `localhost`, the machine hostname, `.local`, `.lan`,
`.home`, `.internal`, `.home.arpa`). Anyone on the LAN can use the page, including connecting
accounts. Do not put it behind a reverse proxy or tunnel exposed to the internet.

Logins started from another computer finish by pasting the final `localhost` redirect URL into
the page, which forwards it to the management OAuth callback.

## Usage history

Proxied Claude/Codex requests and tokens are counted per credential per hour through the
`sdk/cliproxy/usage` plugin hook (independent of `usage-statistics-enabled`), and remaining-%
samples are taken on every quota poll. Both are kept in `<auth-dir>/usage-history.dat`
(31 days of hourly stats, 8 days of quota samples) and served at
`/usage/api/history?range=24h|7d|30d` for the page's chart, totals and per-account sparklines.

Token accounting (history format v2): input is stored as fresh input, cache reads and cache
writes separately. Claude reports input without cache, so only the cache read count is a read
and cache creation is counted as a write; Codex input includes the cached subset, which is
subtracted. Files without `"v":2` are migrated once on load (cache reads were counted twice
before, and Codex input included the cached part). Older data stays approximate: requests that
only wrote the cache may still show those tokens as reads.

The heatmap and the day buckets use the browser's time zone (`tz` query parameter of the
history endpoint, an IANA name; invalid or missing falls back to the server's zone).
