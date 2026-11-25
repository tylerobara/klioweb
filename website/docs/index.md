---
slug: /
sidebar_position: 1
title: Overview
---

# klioWeb

Web dashboard for monitoring [Klio](https://github.com/cloudnative-pg/klio) backups of CloudNativePG clusters.
It lists every CNPG cluster, which ones have the Klio plugin enabled, and the health of their backups.

The dashboard is **read-only without logging in**; restrict access at the ingress/network layer if that matters to
you. With [OIDC login](login.md) enabled, logged-in users can also [verify restores](restore-verification.md) and
[run manual backups](manual-backups.md).

The page auto-refreshes every 15 seconds by default; pick another interval (or Off) from the **Refresh** menu in the
header. Logged-in users also see the running klioWeb version in the header.
