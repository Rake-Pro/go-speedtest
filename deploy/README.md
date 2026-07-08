# deploy

Placeholder for deployment manifests. Kubernetes/GitOps wiring is handled
elsewhere. The container image built from the repo `Dockerfile` exposes port
8080 and runs the `go-speedtest` server; front it with a TLS-terminating edge
proxy (this binary never serves TLS).
