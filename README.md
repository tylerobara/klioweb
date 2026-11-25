# klioWeb

Read-only web dashboard for monitoring [Klio](https://github.com/cloudnative-pg/klio) backups of CloudNativePG clusters.
It lists every CNPG cluster, which ones have the Klio plugin enabled, and the health of their backups.
There is no login; restrict access at the ingress/network layer.

## Install

```sh
helm install klioweb oci://ghcr.io/tylerobara/charts/klioweb -n storage \
  --set ingress.enabled=true --set ingress.host=klio.example.com
```

The chart creates a cluster-wide read-only role (`get`/`list` on CNPG clusters, backups, scheduled backups,
Klio servers/plugin configurations, and pods). Use `httpRoute.*` instead of `ingress.*` for Gateway API.

## Login (OIDC)

The dashboard is readable without logging in. With OIDC enabled, a **Log in** button appears top right, and
actions that change things (such as restore verification) require a logged-in user.

```yaml
oidc:
  enabled: true
  issuerUrl: https://auth.example.com/application/o/klioweb/
  clientId: klioweb
  externalUrl: https://klioweb.example.com   # callback = <externalUrl>/auth/callback
  existingSecret: klioweb-oidc               # key client-secret (or set clientSecret inline)
  allowedGroups: [klio-admins]               # optional; empty = any user your IdP authenticates
  # allowedEmails: [me@example.com]
  # scopes: [openid, profile, email]         # add the scope your IdP needs to release groups
```

Authorization-code flow with PKCE; sessions are signed cookies (`HttpOnly`, `SameSite=Lax`, `Secure` on https),
so no server-side session store is needed. If no session secret is supplied, the chart generates one and keeps it
across upgrades. State-changing endpoints must be wrapped in `Auth.RequireLogin`, which also requires an
`X-Requested-With` header as CSRF protection.

## Restore verification

A logged-in user can click **Verify** next to a completed backup. klioweb then:

1. clones the source cluster's image, storage and Postgres settings, plus its `PluginConfiguration`, into a temporary
   single-instance `Cluster` (named `<cluster>-rt-<id>`) that bootstraps from that backup's `backupId`;
2. waits for it to become healthy (default timeout 30m);
3. runs SQL checks inside the restored pod: accepts connections, promoted out of recovery, and per-database table
   count and size;
4. always deletes the temporary cluster, plugin configuration and volume, then stores the result.

Results are kept as ConfigMaps in klioweb's namespace (`keepRuns` per cluster) and shown on the **Restore tests** tab;
anyone can read them, only logged-in users can start a run. Everything klioweb creates is labelled
`klioweb.obara.dev/restore-test=true`; it refuses to delete anything without that label and sweeps leftovers at startup.

```yaml
oidc: {enabled: true, ...}      # required
restore:
  enabled: true
  namespaces: [default, media]  # only these namespaces get write access (Role + RoleBinding each)
  timeout: 30m
  keepRuns: 20
```

The Roles allow creating/deleting `clusters` and `pluginconfigurations` and `pods/exec` in the listed namespaces, so
treat a logged-in klioweb user as trusted for those namespaces. Use `oidc.allowedGroups` to limit who that is.
Keep `replicaCount: 1` (runs are tracked in-process).

## Notifications

Set `notifications.enabled=true` to be notified when a Klio backup finishes. `notifications.events` is
`failure` (default), `success` or `both`. A generic webhook and Pushover can be used together.

```yaml
notifications:
  enabled: true
  events: both
  linkUrl: https://klioweb.example.com     # optional, attached to Pushover messages
  webhook:                                  # Slack/Mattermost/Discord URLs work as-is
    url: https://hooks.example.com/abc      # stored in a chart-managed Secret
    # existingSecret: klioweb-webhook       # or your own Secret (takes precedence)
    # existingSecretKey: url
  pushover:                                 # https://pushover.net/api
    enabled: true
    appToken: ""                            # or existingSecret with keys token/user
    userKey: ""
    # existingSecret: klioweb-pushover
    # existingSecretTokenKey: token
    # existingSecretUserKey: user
    failurePriority: 1                      # -2..2; 2 = emergency (uses retry/expire)
    successPriority: -1
    # device: "", sound: "", retry: 60, expire: 3600
```

Webhook payloads are JSON (`text`, `content`, `event`, `cluster`, `namespace`, `backup`, `error`, timings).
Backups that finished before the pod started are not notified, so restarts don't replay history. Failed
deliveries are retried (per destination) unless the service rejects the request outright (HTTP 4xx).

## Development

```sh
go run .   # uses in-cluster config, falling back to your current kubeconfig context
```

CI (`.github/workflows/ci.yaml`) publishes the image to `ghcr.io/<owner>/klioweb` and the chart to
`oci://ghcr.io/<owner>/charts/klioweb`. Pushes to `main` publish `edge`; tags `vX.Y.Z` publish releases.
