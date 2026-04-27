---
name: agent-browser
description: Browser automation inside a roster space. Use when the user asks to interact with a website — navigate, fill forms, click buttons, take screenshots, extract data, scrape content, test a web app, log into a site, or automate any browser task. Triggers include "open a website", "fill out a form", "click a button", "scrape data from a page", "test this app", "log into a site", "take a screenshot of". Drives this orch's dedicated headed Chrome via CDP. Never touches the user's main browser.
allowed-tools: Bash(agent-browser:*)
hidden: true
---

# agent-browser (roster space)

Fast browser automation against this orchestrator's dedicated headed Chrome — a real Chrome window, separate from the user's main browser, with a profile name and theme color that match this space's id.

## Rules (non-negotiable)

- **Driver**: run `agent-browser <subcommand>`. The roster wrapper on PATH auto-attaches `--cdp $AGENT_BROWSER_CDP` and blocks any flag that would launch a separate browser. **Never pass `--cdp` yourself.**
- **Never** use port 9222 — that's the user's main Chrome and is off-limits to spaces.
- **Never** run `agent-browser install` (would set up Playwright; we don't use it) or `npx -y agent-browser` (bypasses the wrapper).
- If `agent-browser` reports the browser isn't alive, **stop and notify the user (or your parent) to click the globe icon in the dashboard.** Don't try to launch Chrome yourself.
- This is a real Chrome window with the user's normal fingerprint — Cloudflare/CAPTCHA challenges should behave as for a human session.

## The core loop

```bash
agent-browser open <url>        # 1. Open a page
agent-browser snapshot -i       # 2. See what's on it (interactive elements only)
agent-browser click @e3         # 3. Act on refs from the snapshot
agent-browser snapshot -i       # 4. Re-snapshot after any page change
```

Refs (`@e1`, `@e2`, …) are assigned fresh on every snapshot. They become **stale the moment the page changes** — after a click that navigates, a form submit, a dynamic re-render, a dialog opens. Always re-snapshot before the next ref interaction.

## Common verbs

```bash
agent-browser open <url>             # navigate
agent-browser back / forward / reload
agent-browser snapshot -i            # interactive elements only (preferred)
agent-browser snapshot -i -u         # include href URLs on links
agent-browser snapshot -s "#main"    # scope to a CSS selector
agent-browser click @e3              # click ref
agent-browser fill @e2 "text"        # set input value (clears first)
agent-browser type @e2 "text"        # type chars without clearing (search boxes)
agent-browser select @e1 "option"    # pick a dropdown option
agent-browser check @e1 / uncheck @e1
agent-browser press Enter            # send a key
agent-browser scroll down 500
agent-browser screenshot path.png
agent-browser get text @e1
agent-browser get url
agent-browser get title
agent-browser wait --load networkidle
agent-browser wait @e1
agent-browser wait 2000              # ms
```

Snapshot output looks like:

```
Page: Example — Log in
URL: https://example.com/login

@e1 [heading] "Log in"
@e2 [form]
  @e3 [input type="email"] placeholder="Email"
  @e4 [input type="password"] placeholder="Password"
  @e5 [button type="submit"] "Continue"
  @e6 [link] "Forgot password?"
```

For the full reference, the upstream tool serves a richer guide via `agent-browser skills get core --full` — but the roster wrapper rules above always take precedence over anything that suggests `--cdp`, `connect`, `dashboard`, or `install`.
