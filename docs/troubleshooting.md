# Troubleshooting

Common failure modes and how to resolve them. Organised by the symptom you'll see first (event, log line, probe state) rather than by component.

## Pod won't start

### `get greydns-config: configmaps "greydns-config" is forbidden`

The pod's ServiceAccount lacks `get` on the ConfigMap. Check the `greydns-config-reader` Role binds the `greydns-sa` ServiceAccount in the pod's namespace and that both live in the same namespace as the ConfigMap.

```sh
kubectl -n <ns> describe role greydns-config-reader
kubectl -n <ns> describe rolebinding greydns-config-reader-binding
```

### `get greydns-secret: secrets "greydns-secret" is forbidden`

Same cause as above for the Secret. The same Role covers both ConfigMap and Secret resourceNames; confirm the Secret's name is exactly `greydns-secret`.

### `cloudflare: secret key "cloudflare-token" is empty or missing`

The Secret exists but the key is wrong. The Cloudflare provider reads `cloudflare-token`; re-create with:

```sh
kubectl create secret generic greydns-secret --from-literal=cloudflare-token=<token>
```

## Probes fail

### `/readyz` returns 503 forever

The pod is alive (`/healthz` is OK) but reports not ready. Readiness flips true once provider build, zone discovery, and the initial cache warm all succeed. Check the logs for one of:

- `Failed to build provider` — bad token or provider name
- `Failed to list zones` — provider API error; see provider auth below
- `initial cache refresh: ...` — provider returned an error during startup

Followers in a multi-replica setup intentionally stay ready even when they don't hold the lease; if every replica is stuck on 503, it's a startup problem, not a leadership one.

### `/readyz` flaps between 200 and 503

Usually means the lease is thrashing. See "Leader lease contention" below.

## Leader lease contention

### Leadership keeps changing hands

Every ~`RetryPeriod` (2s) a pod without the lease probes for it. If two pods see the lease as free in the same window, the loser's Update is rejected and it logs `Observed new leader`. Some churn during a rolling update is expected; sustained churn points to:

- **API server slow or overloaded** — `RenewDeadline` (10s) is tight; slow lease updates read as loss. Check `kubectl get apiservice` and apiserver logs.
- **Clock skew** — lease expiry is apiserver-authoritative, so clock skew between nodes doesn't matter, but clock skew between pods and their perception of timeouts can still cause spurious renewal failures. Verify NTP.
- **Network partition** — one replica can write to the apiserver, another can't. Only the writer will hold leadership; followers will stay in probe-green followers state.

### Leader lease exists but no pod holds it

```sh
kubectl get lease greydns-leader -o yaml
```

If `spec.holderIdentity` is empty and the lease is older than `LeaseDuration` (15s), the next probe wins. If no pod ever claims it, check:

- Pod ServiceAccount has `coordination.k8s.io/leases` `create`/`update`/`get`/`patch` (see `deployment.yaml`).
- Pod hostname is resolvable and unique (`os.Hostname()` is the identity).

## DNS records not updating

### `DuplicateDomain` event on a Service or Ingress

A record for that domain already exists in the provider, owned by a different greydns-managed resource. Possible causes:

- Two Services or Ingresses annotated with the same `greydns.io/domain` / `spec.rules[].host`. First come, first served; the loser gets the event. Pick one to remove or rename.
- A resource you deleted earlier hasn't finished cleaning up. Check the retry queue metric (`greydns_retry_queue_depth`); if non-zero, the controller is still trying to delete.
- A manually-created record in Cloudflare with the greydns owner comment. Clean it up in the Cloudflare dashboard.

### `Zone %q not managed by provider`

`greydns.io/zone` annotation doesn't match any zone the provider's token can list. Compare the annotation value against:

```sh
# from inside the cluster or with the same token locally:
curl -H "Authorization: Bearer <token>" https://api.cloudflare.com/client/v4/zones
```

Zone lookup is case-insensitive, so `Example.com` and `example.com` both work; anything else (typos, subdomains of managed zones, zones in a different Cloudflare account) will fail.

### `InvalidAnnotation` event

The Service or Ingress has a malformed greydns annotation. Detail in the event message narrows it down:

- `greydns.io/zone is empty` — missing or empty zone annotation
- `greydns.io/domain is empty` / `spec.rules[].host is empty` — no target domains
- `greydns.io/ttl: must be a positive integer, got "foo"` — TTL parse failure
- `greydns.io/<key>: override not in allowed-overrides` — the platform operator restricted which overrides Services can set; this one isn't on the allowlist

### Record created but Cloudflare reports 401/403 on update/delete

Cloudflare token scope. The token needs **Zone: Read** and **DNS: Edit** on every zone greydns manages. Missing DNS:Edit will let reads succeed but writes fail.

### Records don't disappear after deleting a Service/Ingress

Check `greydns_retry_queue_depth` metric. If it's non-zero, the controller tried and failed to delete and is retrying on the cache-refresh interval (default 60s). Logs will show `Retry delete failed` with the provider error; fix the provider (token expiry, quota) and the next tick will clear it.

If the metric is zero and the record is still there, either:

- The record isn't owned by greydns (no `[greydns]owner=...` comment). Clean up manually.
- The owner comment uses the legacy `ns/name` format and the current version expected `kind:ns/name`. Legacy records are recognised on read and rewritten on next update; a one-time `kubectl annotate` bounce forces a reconcile.

## Metrics

Scrape `GET /metrics` on port 8080 of any pod. For a leader-elected deployment, only the leader's cache/reconcile metrics are meaningful (followers don't mutate); probe-only metrics (workqueue depth, retry queue) will be zero on followers. The `greydns_cache_refresh_last_success_timestamp_seconds` gauge is the canonical "am I up-to-date" signal; alert if it stops advancing.
