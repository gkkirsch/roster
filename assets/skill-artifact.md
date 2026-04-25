---
name: artifact
description: Build a live-rendered website, landing page, dashboard, or interactive UI inside this roster space. Use when the user asks for "a website", "a landing page", "a demo", "an app", "a UI", "a component", "a prototype", or any visual artifact they want to see render. Scaffolds a Vite + React 19 + Tailwind v4 starter via `roster artifact create`; the dashboard's artifact panel renders it live with HMR.
allowed-tools: Bash(roster artifact:*)
hidden: true
---

# artifact (roster space)

When the user asks for something visual — a website, landing page, dashboard, prototype, demo, interactive component — scaffold an **artifact**, don't paste raw JSX into chat. The artifact has its own Vite dev server; the user watches it render in the dashboard's artifact panel as you build.

## Setup (once per artifact)

```
roster artifact create <orch-id> <aid> --title "<short title>"
```

- `<orch-id>` is your own id.
- `<aid>` is short kebab-case: `landing-page`, `dash`, `pricing`.
- Creates `<your_claude_dir>/artifacts/<aid>/` with the starter wired up. Refuses if the dir already exists — that's the cue you're refining, not creating.

The starter ships with:

```
artifacts/<aid>/
├── package.json        (vite + react 19 + tailwind v4 + ts)
├── vite.config.ts
├── tsconfig.json
├── index.html
└── src/
    ├── main.tsx
    ├── App.tsx
    └── styles.css      (@import "tailwindcss")
```

## Build loop

Use the standard `Write` / `Edit` tools on files inside `artifacts/<aid>/`. Every save is HMR-pushed to the iframe — the user sees each change.

Recommended cadence:
1. Sketch a skeleton in `src/App.tsx` first (header, hero, sections, footer). Save.
2. Fill each section in turn. Save after each.
3. Split into components only when one starts repeating or growing past ~80 lines.

## Rules

- **Stack is fixed**: React 19 + Tailwind v4 + TypeScript. Don't switch frameworks. Don't add CSS preprocessors. Don't replace Tailwind.
- **No `npm install`** unless the task genuinely needs a new dep. Lean on what's already in the starter.
- **Don't recreate** if `roster artifact create` says it exists — `Edit` the existing files.
- **Don't run `npm run dev` yourself** — fleetview spawns the dev server lazily when the user opens the artifact panel. If the user reports they can't see anything, tell them to open the artifact panel from the top-right of the chat.
- **Don't open the dir in your dedicated Chrome** to "preview" — the user already sees it via the artifact panel.

## Tailwind v4 notes

The starter uses Tailwind v4's CSS-first import (`@import "tailwindcss"`). No `tailwind.config.js`. Custom colors / fonts go in `styles.css`:

```css
@import "tailwindcss";

@theme {
  --color-brand: #4dcbae;
  --font-display: "Fraunces", serif;
}
```

Then use as `bg-brand`, `font-display`, etc. in JSX.

## When NOT to use this skill

- The user asks for code they'll paste elsewhere (e.g. "give me the JSX for…") — produce it in chat instead.
- The task is fixing existing code in a real repo, not building a fresh demo.
- The user explicitly says "no live preview" or "just write to a file."
