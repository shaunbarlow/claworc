# OpenConnector OAuth callback under the Claworc-proxied path

Verified against the checked-out source (`~/.openclaw/workspace/projects/claworc` @ `de35796`,
`~/.openclaw/workspace/projects/open-connector` @ `9933a753`) on 2026-08-27.

## What to set

Set the `connector_origin` Claworc setting to Claworc's own public origin **plus the
`/connector` mount prefix**:

```
connector_origin = https://<your-claworc-host>/connector
```

(A trailing slash is fine — `OAuthClientConfigService` strips it: `oauth-client-config-service.ts:78`.)

There is currently **no field for this in the Settings UI** (`SettingsPage.tsx`'s "Managed
OpenConnector" card only exposes the enable toggle, image, and a dashboard link — grepped, no
`connector_origin` input anywhere in `frontend/src`). Set it directly via the API:

```bash
curl -sS -X PUT https://<your-claworc-host>/api/v1/settings \
  -H 'Content-Type: application/json' \
  --cookie "claworc_session=<your-admin-session-cookie>" \
  -d '{"connector_origin": "https://<your-claworc-host>/connector"}'
```

`connector_origin` is a plain (unencrypted) setting handled by the generic string-setting loop in
`UpdateSettings` (`settings.go:46`, `settings.go:417`) — no special validation, just stored as-is.

## Why this value

1. Claworc mounts the managed OpenConnector's web console + admin API at `/connector/*` on its
   own origin via `ConnectorProxy`, stripping that prefix before forwarding to the container
   (`control-plane/internal/handlers/connector.go:462-463`, wired at
   `control-plane/main.go:605`).
2. `connector_origin` becomes `OOMOL_CONNECT_ORIGIN` in the connector container's env
   (`connectorprov.go:108-109`).
3. OpenConnector's `OAuthClientConfigService.expectedRedirectUri()` builds the redirect URI as
   `${OOMOL_CONNECT_ORIGIN}/oauth/callback` — the callback path itself (`/oauth/callback`) is a
   **fixed constant**, not configurable (`oauth-client-config-service.ts:69,142-145`). The route
   is registered at exactly that path server-side too (`connect-server.ts:212`).
4. So the redirect URI OpenConnector generates, and the URL you must register with each
   third-party OAuth app, is:

   ```
   https://<your-claworc-host>/connector/oauth/callback
   ```

   If `connector_origin` is left unset/empty, it defaults to `http://localhost:<port>` inside the
   container (`connectorprov.go:63-65` — env only set `if cfg.Origin != ""`), which is unreachable
   from a real OAuth provider redirect. OAuth flows through the managed/proxied deployment will
   simply not complete until this is set.

## Making the change take effect

Like `connector_image`, saving `connector_origin` alone does **not** trigger a live redeploy —
`UpdateSettings` only auto re-applies the connector workload when `connector_enabled` itself
flips (`settings.go:262`). To push the new `OOMOL_CONNECT_ORIGIN` into the running container,
after saving the setting do one of:

- Toggle "Managed OpenConnector: Enabled" off then on in Settings, or
- Click "Update image" in that same card (`UpdateConnectorImage` re-resolves the full config,
  including origin, and force-recreates the container: `connector.go:365-387`), or
- Restart the control plane (`BootApplyConnector` re-applies on boot if `connector_enabled` is
  true).

## Fixed: the callback path no longer requires a Claworc admin session

**As of `main.go`'s `/connector/oauth/callback` route (added 2026-08-27), this is no longer an
issue** — see the section below for details. The rest of this section is kept for history/context
on why the exemption exists.

Previously, `/connector/*` (which includes `/connector/oauth/callback`) was mounted only under
Claworc's authenticated admin route group:

```go
r.Group(func(r chi.Router) {
    r.Use(middleware.RequireAuth(sessionStore))
    r.Use(middleware.RequireAdmin)
    r.HandleFunc("/connector/*", handlers.ConnectorProxy)
})
```

That meant the browser tab completing the OAuth authorization had to already be logged into
Claworc as an admin (valid `claworc_session` cookie) at the moment the third-party provider
redirected back — otherwise the callback request itself got rejected with 401/403 by Claworc's
own middleware before it ever reached OpenConnector's `/oauth/callback` handler, independent of
whether the OAuth state/PKCE parameters were correct. Third-party OAuth redirects are top-level
navigations and don't reliably carry the admin's session cookie (private/incognito windows,
same-site edge cases, a session that expired mid-flow), so this could fail even when the admin
believed they were still logged in.

## The fix

`control-plane/main.go` now registers an explicit unauthenticated route for the callback path,
registered as a sibling of (not nested inside) the admin-gated wildcard group:

```go
r.Get("/connector/oauth/callback", handlers.ConnectorProxy)
```

This is safe because:

1. **Routing precision**: chi resolves routes via a radix tree keyed on path segments, not
   registration order or middleware nesting. The static segment `/connector/oauth/callback`
   always wins over the wildcard `/connector/*` for that exact path, so every other path under
   `/connector/` (the dashboard, the admin API) still requires an authenticated admin session.
   See `control-plane/connector_oauth_callback_route_test.go` for a standing regression test of
   this precedence.
2. **OpenConnector already treats `/oauth/callback` as a public path** —
   `src/server/api/auth.ts`'s `isPublicPath` explicitly exempts it from local bearer-token/admin
   auth, since the whole point of an OAuth redirect is that the browser hitting it isn't
   otherwise authenticated to the app receiving it.
3. **Safety instead comes from the OAuth state itself**: `handlers.ConnectorProxy` still injects
   the connector's own admin bearer token onto every proxied request (including this one), but
   more importantly, OpenConnector's `OAuthFlowService.completeAuthorization` requires a `state`
   value that was minted by that same admin's prior authenticated `POST
   /api/oauth/authorizations` call, is single-use (`states.take` consumes it), and expires after
   `stateMaxAgeMs` (default 15 minutes). An unauthenticated caller who doesn't already hold a
   live `state`+`code` pair from the real provider redirect gets rejected by OpenConnector itself,
   not by Claworc's proxy — exempting the route from Claworc's own session check doesn't create a
   new way to complete someone else's OAuth flow or reach any other connector functionality.

## Summary checklist

- [ ] `connector_origin` = `https://<claworc-host>/connector` (via `PUT /api/v1/settings`, no UI field yet — or use the Settings UI field added in `bc70565`)
- [ ] Register `https://<claworc-host>/connector/oauth/callback` as the redirect URI in each third-party OAuth app
- [ ] Toggle the connector off/on (or hit "Update image") to push the new origin into the running container
- [ ] Kick off "Connect" flows from an authenticated admin session (still required to *start* the flow via `POST /api/oauth/authorizations`); the callback redirect itself no longer needs that session to still be valid
