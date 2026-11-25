---
sidebar_position: 7
---

# Development

```sh
go run .   # uses in-cluster config, falling back to your current kubeconfig context
```

CI (`.github/workflows/ci.yaml`) publishes the image to `ghcr.io/<owner>/klioweb` and the chart to
`oci://ghcr.io/<owner>/charts/klioweb`. Pushes to `main` publish `edge`; tags `vX.Y.Z` publish releases.
The image reports its version (set with `-ldflags "-X main.version=..."`) to logged-in users.

These docs are built with Docusaurus from `website/` and published to GitHub Pages by
`.github/workflows/docs.yaml`. To preview locally: `cd website && npm ci && npm start`.
