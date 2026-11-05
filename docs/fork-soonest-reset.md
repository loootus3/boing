# Fork: soonest-reset routing and local usage page

This fork adds two things on top of upstream CLIProxyAPI.

## `routing.strategy: soonest-reset`

Among the credentials that are available for a request (same priority tiers and cooldowns as
`fill-first`), the proxy spends the one whose **weekly** subscription window resets first, so
quota that is about to expire is used before quota that would carry on for longer.

- Credentials with any window at 100% (or `limit_reached`) are tried last.
- Credentials without usage data yet sit between the two groups.
- Usage data comes from `internal/subsusage`, which polls every 3 minutes:
  - Claude: `GET https://api.anthropic.com/api/oauth/usage` (`five_hour`, `seven_day`)
  - Codex: `GET https://chatgpt.com/backend-api/wham/usage` (`rate_limit.primary_window`, `secondary_window`)

```yaml
routing:
  strategy: "soonest-reset"
```

## `/usage` page

`http://127.0.0.1:8317/usage` shows only Claude (first) and Codex accounts with the used
percentage and reset time of each window, marks the account that will be spent next, and has
`+ Claude` / `+ Codex` buttons that run the OAuth login in the browser.

The page and its `/usage/api/*` endpoints need no management key, but they only answer
requests from the loopback interface with a loopback `Host`, reject cross-site requests, and
require an `X-Usage-UI: 1` header on state-changing calls.
