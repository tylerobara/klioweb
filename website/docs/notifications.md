---
sidebar_position: 6
---

# Notifications

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
