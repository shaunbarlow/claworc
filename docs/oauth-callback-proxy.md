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

## Important caveat: the callback path is behind Claworc admin auth

`/connector/*` (which includes `/connector/oauth/callback`) is mounted under Claworc's own
authenticated route group, requiring both a valid Claworc session **and** the admin role:

```go
r.Group(func(r chi.Router) {
    r.Use(middleware.RequireAuth(sessionStore))
    r.Use(middleware.RequireAdmin)
    r.HandleFunc("/connector/*", handlers.ConnectorProxy)
})
```
(`control-plane/main.go:600-605`)

This means the browser tab completing the OAuth authorization must already be logged into
Claworc as an admin (valid `claworc_session` cookie) at the moment the third-party provider
redirects back — otherwise the callback request itself gets rejected with 401/403 by Claworc's
own middleware before it ever reaches OpenConnector's `/oauth/callback` handler, independent of
whether the OAuth state/PKCE parameters are correct. In practice: kick off the "Connect" flow for
a service from inside the OpenConnector dashboard reached via `/connector/` in an already
logged-in-as-admin Claworc browser session, and don't let that session expire mid-flow.

(If `config.AuthDisabled` is set on this Claworc instance, `RequireAuth` auto-resolves to the
first admin user and this isn't a practical concern — but that's a dev/test-only escape hatch,
not something to rely on in a normal deployment.)

## Summary checklist

- [ ] `connector_origin` = `https://<claworc-host>/connector` (via `PUT /api/v1/settings`, no UI field yet)
- [ ] Register `https://<claworc-host>/connector/oauth/callback` as the redirect URI in each third-party OAuth app
- [ ] Toggle the connector off/on (or hit "Update image") to push the new origin into the running container
- [ ] Complete OAuth "Connect" flows only while logged into Claworc as an admin in that browser
