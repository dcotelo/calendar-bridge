# Deploying on Kubernetes

Worth it if you already run a cluster. **Not worth standing one up for this** —
calendar-bridge is a single small process; a laptop or a Raspberry Pi is a
better fit. See [choose your deployment](README.md).

Manifests are in [`deploy/k8s/`](../../deploy/k8s/).

---

## Prerequisites

- A cluster and `kubectl`.
- OAuth credentials per account, and **tokens created on a machine with a
  browser**. The `auth` flow is interactive, so run it locally and load the
  resulting token files into a Secret.

---

## 1. Create the Secret

Run `calendar-bridge auth` locally first, then write a `config.yaml` using the
**absolute in-container paths** the manifests mount to:

```yaml
accounts:
  - name: personal
    credentials_file: /app/config/personal-credentials.json
    token_file: /app/config/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /app/config/work-acme-credentials.json
    token_file: /app/config/work-acme-token.json
    calendar_id: primary

poll_interval: 5m
lookahead_days: 30
block_title: "Busy (calendar-bridge)"

metrics:
  enabled: true
  listen_addr: "0.0.0.0:9090"
```

Note the flat layout — everything lands in `/app/config/` because a Secret has
no subdirectories.

```bash
kubectl create namespace calendar-bridge

kubectl -n calendar-bridge create secret generic calendar-bridge-config \
  --from-file=config.yaml=./config.yaml \
  --from-file=personal-credentials.json=./secrets/personal-credentials.json \
  --from-file=personal-token.json=./secrets/personal-token.json \
  --from-file=work-acme-credentials.json=./secrets/work-acme-credentials.json \
  --from-file=work-acme-token.json=./secrets/work-acme-token.json
```

**A Secret, never a ConfigMap.** Token files are live credentials. Note that a
Kubernetes Secret is base64-encoded, not encrypted, unless you have enabled
encryption at rest — check that before deciding this is more secure than a file
on a laptop.

## 2. Apply

```bash
kubectl apply -k deploy/k8s
kubectl -n calendar-bridge rollout status deploy/calendar-bridge
```

## 3. Verify

```bash
kubectl -n calendar-bridge get pods
kubectl -n calendar-bridge logs -l app.kubernetes.io/name=calendar-bridge --tail=30

kubectl -n calendar-bridge port-forward svc/calendar-bridge-metrics 9090:9090 &
curl -s http://127.0.0.1:9090/readyz
curl -s http://127.0.0.1:9090/metrics | grep last_success
```

**You should now see** `sync.pass.complete` in the logs with `ok=true`, `/readyz`
returning `ok`, and `Busy (calendar-bridge)` blocks on your calendars.

---

## The one thing that needs a decision: token persistence

This is the part the manifests cannot decide for you.

Kubernetes mounts a Secret **read-only**, and there is no way to make it
writable. calendar-bridge re-persists OAuth tokens when Google refreshes them
(roughly hourly) or rotates them. Against a read-only mount those writes fail —
non-fatally; they are logged and the pass continues — but the cache never
updates.

The shipped manifests handle this with an **init container** that copies the
Secret into an `emptyDir` the main container can write. Token refreshes then
persist for the pod's lifetime.

**The catch:** the `emptyDir` dies with the pod. So a *rotated refresh token* is
lost on restart.

| Your OAuth consent screen | What happens | What to do |
|---|---|---|
| **In production** | Google does not rotate refresh tokens. The stored one stays valid indefinitely. | The shipped manifests are fine. |
| **Testing** | Refresh tokens rotate, and expire after 7 days. A pod restart loses the rotated token and the account breaks. | Use a PersistentVolumeClaim, or — much better — move the consent screen to production status. |

**Set your consent screen to "In production".** For a personal app used only by
you it needs no verification review; you click through an "unverified app"
interstitial during `auth`. It removes the weekly re-authorization treadmill
everywhere, not just on Kubernetes.

### The PVC variant

If you must stay in Testing status, replace the `config` `emptyDir` with a
claim, and run the init container only when the volume is empty:

```yaml
volumes:
  - name: config
    persistentVolumeClaim:
      claimName: calendar-bridge-tokens
```

```yaml
# in the init container
args:
  - |
    set -eu
    # Do not clobber tokens already refreshed into the volume.
    for f in /secret/*; do
      [ -e "/app/config/$(basename "$f")" ] || cp "$f" /app/config/
    done
    cp /secret/config.yaml /app/config/config.yaml   # config always wins
    chmod 600 /app/config/*
```

Use a `ReadWriteOnce` claim; there is only ever one replica.

---

## CronJob variant

If you would rather not keep a pod resident,
[`deploy/k8s/cronjob.yaml`](../../deploy/k8s/cronjob.yaml) runs `sync-once` on a
schedule. It is an **alternative** to the Deployment, not an addition — running
both doubles your API usage.

```bash
kubectl delete -n calendar-bridge deploy/calendar-bridge
kubectl apply -f deploy/k8s/cronjob.yaml -n calendar-bridge
```

Costs: no metrics endpoint to scrape between runs, no push-notification support,
and minute-granularity scheduling. `concurrencyPolicy: Forbid` stops passes
stacking.

---

## What the manifests already do

- `replicas: 1` with `strategy: Recreate`. Two replicas would double API usage
  for no benefit; every operation is idempotent, so the second does the same
  work and writes nothing extra.
- Pod Security Standard **restricted** enforced at the namespace.
- `runAsNonRoot`, UID 65532, `readOnlyRootFilesystem`, all capabilities dropped,
  `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`.
- `automountServiceAccountToken: false` — it never talks to the API server.
- Memory requests and limits; **no CPU limit**, deliberately: this is a bursty,
  mostly-idle workload and a CPU limit only adds throttling latency to a pass.
- `livenessProbe` on `/healthz`, `readinessProbe` on `/readyz`. Liveness
  deliberately ignores sync health — an instance that cannot reach Google should
  keep retrying with backoff, not be killed and restarted.
- A ClusterIP metrics Service. The endpoint is read-only and unauthenticated;
  do not expose it outside the cluster.

## Scraping metrics

With the Prometheus Operator:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: calendar-bridge
  namespace: calendar-bridge
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: calendar-bridge
  endpoints:
    - port: metrics
      interval: 60s
```

Alerting rules: [OBSERVABILITY.md](../OBSERVABILITY.md#alerting).

---

## Upgrading

```bash
# 1. Note the running version.
kubectl -n calendar-bridge exec deploy/calendar-bridge -- /app/calendar-bridge version

# 2. Preview with the new image — writes nothing.
kubectl -n calendar-bridge run cb-dryrun --rm -it --restart=Never \
  --image=ghcr.io/dcotelo/calendar-bridge:v0.2.0 \
  --overrides='{"spec":{"containers":[{"name":"cb-dryrun","image":"ghcr.io/dcotelo/calendar-bridge:v0.2.0","args":["sync-once","-config","/app/config/config.yaml","-dry-run"],"volumeMounts":[{"name":"c","mountPath":"/app/config"}]}],"volumes":[{"name":"c","secret":{"secretName":"calendar-bridge-config"}}]}}'

# 3. Roll it.
kubectl -n calendar-bridge set image deploy/calendar-bridge calendar-bridge=ghcr.io/dcotelo/calendar-bridge:v0.2.0
kubectl -n calendar-bridge rollout status deploy/calendar-bridge
```

Read [UPGRADING.md](../UPGRADING.md) first. Pin an exact tag in
`kustomization.yaml` rather than `latest`.

## Rolling back

```bash
kubectl -n calendar-bridge rollout undo deploy/calendar-bridge
kubectl -n calendar-bridge rollout status deploy/calendar-bridge
```

There is no state to migrate.

## Uninstalling cleanly

```bash
# 1. Remove the workload.
kubectl delete -k deploy/k8s
# or, if you used the CronJob:
kubectl delete -f deploy/k8s/cronjob.yaml -n calendar-bridge

# 2. Revoke access, so the tokens are dead even if the Secret is backed up
#    somewhere: https://myaccount.google.com/permissions

# 3. Remove the Secret and namespace.
kubectl -n calendar-bridge delete secret calendar-bridge-config
kubectl delete namespace calendar-bridge
```

**4. Remove the busy blocks it created.** Manual. Search each calendar for your
`block_title` and delete the results.

Check whether your etcd backups still contain the Secret.

---

## Failure modes specific to this target

| Symptom | Cause and fix |
|---|---|
| Pod `CrashLoopBackOff`, exit 3 | Config error. `kubectl logs`. Usually a path in `config.yaml` that does not match where the Secret mounted. |
| Pod `CrashLoopBackOff`, exit 4 | An account needs authorizing. The `auth` flow needs a browser — run it locally and update the Secret. |
| Init container fails | The Secret does not exist, or a `--from-file` key is missing. `kubectl describe pod`. |
| `readOnlyRootFilesystem` errors | Something is writing outside `/app/config` and `/tmp`. Please report it. |
| Warnings about a token write on every refresh | The config volume is not writable — the init container did not run, or you mounted the Secret directly. |
| Account breaks weekly | Refresh-token rotation plus an `emptyDir`. See [token persistence](#the-one-thing-that-needs-a-decision-token-persistence). |
| Two pods briefly during a rollout | `strategy` is not `Recreate`. Harmless but wasteful. |
| Readiness flaps | `metrics.ready_max_age` is shorter than `poll_interval`. It defaults to 3× and should stay above it. |
| Pod evicted under memory pressure | Raise the memory limit. 128Mi is generous for a handful of accounts, but a very large calendar in a 30-day window uses more. |
