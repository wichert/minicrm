# Vendored browser assets

These files are third-party code committed to the repository. There is no npm
step for them: `internal/web/static/embed.go` embeds `*.js`, so a file dropped
in this package is served from `/static/`.

The reason for vendoring rather than bundling is reproducibility of the exact
bytes we ship to the browser, and a reviewable diff when the pinned version
changes. Re-vendoring is deliberate: update the file, update this README, and
mention the version bump in the commit message.

## `htmx.min.js`

| field | value |
| --- | --- |
| Version | 2.0.4 |
| Source | <https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js> |
| Size | 50,917 bytes |
| SHA-256 | `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447` |

Verify a re-download with:

```bash
curl -fsSL https://unpkg.com/htmx.org@2.0.4/dist/htmx.min.js | shasum -a 256
```

### CSP note

`mainCSPBase` sets `script-src 'self'` with no `unsafe-eval`. htmx 2.x contains a
single `new Function` call used only for `hx-trigger` **filters** (`[cond]`), and
it is gated behind the `allowEval` config, which defaults to `false`. The
frontend therefore must not use trigger filters; gating that needs app state
lives in a `htmx:beforeRequest` listener instead (see `isPaused()` in
`internal/web/static/app.js`).
