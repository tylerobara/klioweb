---
sidebar_position: 2
---

# Install

```sh
helm install klioweb oci://ghcr.io/tylerobara/charts/klioweb -n storage \
  --set ingress.enabled=true --set ingress.host=klio.example.com
```

The chart creates a cluster-wide read-only role (`get`/`list` on CNPG clusters, backups, scheduled backups,
Klio servers/plugin configurations, and pods). Use `httpRoute.*` instead of `ingress.*` for Gateway API.
