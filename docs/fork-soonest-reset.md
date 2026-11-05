# Fork: soonest-reset routing and local usage page

This fork adds two things on top of upstream CLIProxyAPI.

## `routing.strategy: soonest-reset`

Among the credentials that are available for a request (same priority tiers and cooldowns as
`fill-first`), the proxy spends the one whose **weekly** subscription window resets first, so
quota that is about to expire is used before quota that would carry on for longer.

- Credentials with any window at 100% (or `limit_reached`) are tried last.
- Credentials without usage data yet sit between the two groups.
- Usage data comes from `internal/subsusage`, which polls every 60 seconds:
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

`http://127.0.0.1:8317/usage` shows only Claude (first) and Codex accounts with the used
percentage and reset time of each window, marks the account that will be spent next, and has
`+ Claude` / `+ Codex` buttons that run the OAuth login in the browser.

The page and its `/usage/api/*` endpoints need no management key, but they only answer
requests from the loopback interface with a loopback `Host`, reject cross-site requests, and
require an `X-Usage-UI: 1` header on state-changing calls.

The page styling is adapted from T3 Code (MIT, Copyright (c) 2026 T3 Tools Inc.).
`/usage?demo=1` renders fake accounts for a quick look.

Usage limits are fetched every 60 s and the page polls every 15 s while the tab is visible;
there is no manual refresh button. The "Uso" section sums each provider's weekly usage across
its accounts.

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
