---
sidebar_position: 3
---

# Login (OIDC)

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
