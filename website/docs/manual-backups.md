---
sidebar_position: 5
---

# Manual backups

With `backup.enabled=true` and `backup.namespaces=[...]` (requires OIDC), a logged-in user sees a
**Back up now** button on each Klio-enabled cluster in those namespaces. It creates a CNPG `Backup`
(`method: plugin`, `klio.cnpg.io`) named `<cluster>-klio-ui-<timestamp>`, labelled
`klioweb.obara.dev/manual-backup=true` and annotated with the user. Only one in-flight backup per
cluster is allowed. The chart grants a create-only Role on `backups` in the listed namespaces.

```yaml
backup:
  enabled: true
  namespaces: [default]
```
