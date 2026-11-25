---
sidebar_position: 4
---

# Restore verification

A logged-in user can click **Verify** next to a completed backup. klioWeb then:

1. clones the source cluster's image, storage and Postgres settings, plus its `PluginConfiguration`, into a temporary
   single-instance `Cluster` (named `<cluster>-rt-<id>`) that bootstraps from that backup's `backupId`;
2. waits for it to become healthy (default timeout 30m);
3. runs SQL checks inside the restored pod: accepts connections, promoted out of recovery, and per-database table
   count and size;
4. always deletes the temporary cluster, plugin configuration and volume, then stores the result.

Results are kept as ConfigMaps in klioWeb's namespace (`keepRuns` per cluster) and shown on the **Restore tests** tab;
anyone can read them, only logged-in users can start a run. Everything klioWeb creates is labelled
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
treat a logged-in klioWeb user as trusted for those namespaces. Use `oidc.allowedGroups` to limit who that is.
Keep `replicaCount: 1` (runs are tracked in-process).
