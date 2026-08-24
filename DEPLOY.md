# Deployment Guide — URL Shortener

Code → image → Kubernetes, step by step. Read once before starting;
each phase says what to expect so you can verify as you go.

> ⚠️ **Company-cluster rule.** Deploying to the shared dev clusters
> (`tke-nn-id-dev`, `tke-rumah123-dev`) touches shared infrastructure.
> Follow RULES.md: `kubectl apply/delete` are mutating — get approval
> before running them there. The smoothest first run is on a **local
> cluster you own** (kind/k3s on the home server — see
> `learning/home-lab-plan.md`).

---

## Phase 0 — Prerequisites

- [ ] A Kubernetes cluster you may deploy to + a working `kubectl` context
- [ ] A container registry you can push to (self-hosted / GitLab Container Registry)
- [ ] A GitLab project where you can push code and set CI/CD variables

Check your context:

```bash
kubectl config get-contexts        # what exists
kubectl config use-context NAME    # pick one (local cluster recommended)
kubectl get nodes                  # proves the connection works
```

---

## Phase A — GitLab repository + CI

### A1. Create the repository

Create a new project on your GitLab instance, then push this code:

```bash
cd url-shortener
git init && git add -A && git commit -m "URL shortener learning project"
git branch -M main
git remote add origin git@gitlab.com:your-org/url-shortener.git
git push -u origin main
```

### A2. Add CI/CD variables

GitLab project → Settings → CI/CD → Variables:

| Variable | Example value | Required for |
|---|---|---|
| `REGISTRY_URL` | `registry.example.com/url-shortener` | Docker build + push |
| `REGISTRY_USER` | your registry user | Only if registry needs auth |
| `REGISTRY_PASSWORD` | your registry token/password | Only if registry needs auth |

Until `REGISTRY_URL` is set, the pipeline skips the build stage
automatically — lint and test still run on every push.

### A3. Understand the pipeline

```text
push / merge request
   │
   ├─ backend-lint:   gofmt check → go vet
   ├─ frontend-lint:  npm ci → eslint
   ├─ backend-test:   go test -v
   ├─ frontend-build: npm run build (verifies the build compiles)
   │
   └─ docker-build (main branch only, needs REGISTRY_URL):
        buildx (linux/amd64 + linux/arm64) → push
        tags: <commit-sha> + latest
```

The `docker-build` job uses GitLab's Docker-in-Docker (`dind`) service
with buildx's `docker-container` driver. If your GitLab runner is
self-hosted, make sure it has `--privileged` mode enabled for the
`dind` service — otherwise Docker-in-Docker won't work.

### A4. Watch the pipeline

GitLab project → CI/CD → Pipelines. The first run should show:

```text
backend-lint   ✔  gofmt, vet
frontend-lint  ✔  eslint
backend-test   ✔  tests pass
frontend-build ✔  vite build
docker-build   ✔  image pushed   (main branch only, REGISTRY_URL set)
```

If `REGISTRY_URL` is not yet configured, `docker-build` shows as
**skipped** (not failed). That's the expected behavior.

---

## Phase B — Kubernetes deployment

### B1. Create the Postgres secret

```bash
cd k8s
cp 01-postgres-secret.example.yaml 01-postgres-secret.yaml
# edit the copy: set a random POSTGRES_PASSWORD
kubectl apply -f 01-postgres-secret.yaml
```

> `.gitignore` blocks `*-secret.yaml` — the real password stays local.

### B1b. (Optional) Create the Google SSO secret

Skip this and the app still deploys — it just runs without sign-in
(the Deployment's `optional: true` secret refs make the pods start fine).

```bash
cp 04b-google-auth-secret.example.yaml 04b-google-auth-secret.yaml
# edit the copy: GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET,
#                SESSION_SECRET (openssl rand -hex 32)
kubectl apply -f 04b-google-auth-secret.yaml
```

Then set the redirect URI in Google Cloud Console to match your access
URL. With port-forward: `http://localhost:8080/auth/google/callback`
(and keep the ConfigMap's `PUBLIC_BASE_URL` on the same origin). With an
Ingress: `https://<ingress-host>/auth/google/callback` — and update
`PUBLIC_BASE_URL` in `04-api-configmap.yaml` BEFORE applying it.

### B2. Point the Deployment at your image

Edit `k8s/05-api-deployment.yaml`:

```yaml
image: REGISTRY_HOST/url-shortener:latest
# becomes, e.g.:
image: registry.example.internal:5000/url-shortener:abc1234
```

Use the SHA tag from Phase A4 (not `latest`) so you know exactly what runs.

### B3. Apply in order

```bash
kubectl apply -f k8s/00-namespace.yaml
kubectl apply -f k8s/01-postgres-secret.yaml
kubectl apply -f k8s/02-postgres-service.yaml
kubectl apply -f k8s/03-postgres-statefulset.yaml
kubectl apply -f k8s/04-api-configmap.yaml
# optional — Google SSO (see B1b):
# kubectl apply -f k8s/04b-google-auth-secret.yaml
kubectl apply -f k8s/05-api-deployment.yaml
kubectl apply -f k8s/06-api-service.yaml
# optional, only with an ingress controller:
# kubectl apply -f k8s/07-ingress.example.yaml
```

### B4. Watch it come up (the fun part)

```bash
kubectl get pods -n url-shortener -w
```

Expected sequence — match it against what you know about probes:

```text
postgres-0                 0/1   Pending     → PVC binding
postgres-0                 0/1   Running     → readiness pg_isready
postgres-0                 1/1   Running     ✔
url-shortener-api-xxxxx    0/1   Running     → startupProbe waiting for DB
url-shortener-api-xxxxx    1/1   Running     ✔ readyz passed
```

Then inspect what Kubernetes recorded about your probes:

```bash
kubectl describe pod -n url-shortener -l app=url-shortener-api | grep -A4 Probe
kubectl logs   -n url-shortener -l app=url-shortener-api | head
# expect: "database ready, schema ensured" then "server starting"
```

### B5. Use it

```bash
kubectl port-forward -n url-shortener svc/url-shortener-api 8080:80
```

Open http://localhost:8080 → shorten a URL → scan the QR with your
phone → the phone hits the short link → 302 → original URL. 🎉

Probe the probes themselves:

```bash
curl localhost:8080/api/healthz    # {"status":"ok"}
curl localhost:8080/api/readyz     # {"status":"ready"}
```

---

## Phase C — Experiments (where it really sinks in)

Each experiment = break something, watch Kubernetes react, explain why.

1. **Kill Postgres on purpose**: `kubectl delete pod -n url-shortener postgres-0`
   Watch the API pods go `NotReady` (readiness fails → removed from
   Service) but NOT restart (liveness doesn't check the DB).
2. **Break liveness**: set `path: /api/healthzz` in the Deployment.
   Watch CrashLoopBackOff. Fix it back.
3. **Scale**: `kubectl scale deploy/url-shortener-api -n url-shortener --replicas=3`.
   Both replicas share Postgres — that's why state lives in the DB.
4. **Redeploy a new version**: push a code change, watch CI tag a new
   SHA, update the image tag in the Deployment, `kubectl apply`, and
   watch the rolling update (`kubectl rollout status`).

---

## Troubleshooting

| Symptom | Likely cause | Check / fix |
|---|---|---|
| Pod `Pending` | No PVC / no StorageClass | `kubectl describe pod` → events; cluster needs a default StorageClass |
| Pod `ImagePullBackOff` | Wrong image name, or registry needs credentials / insecure-registry config | `kubectl describe pod` events; create `imagePullSecrets` or fix registry trust |
| API pod `CrashLoopBackOff` | Postgres unreachable / wrong password | `kubectl logs` — the retry warnings show attempts; check Secret values |
| Pod Running but `0/1` Ready | readiness failing | `kubectl exec`/port-forward then `curl /api/readyz` — read the `reason` |
| 502 via Ingress | Service selector mismatch | `kubectl get endpoints -n url-shortener` — empty means selector ≠ labels |
| Google: `redirect_uri_mismatch` | Console redirect URI ≠ `PUBLIC_BASE_URL + /auth/google/callback` | Compare the exact string (scheme + host + path) in both places |
| Google: `Error 403: access_denied` | App in Testing mode, account not a test user | Add your Gmail to the consent screen test users |
| Sign-in button missing | SSO not configured | `curl /api/auth/config` → `{"enabled":false}` means the Secret/env vars are absent |

---

## After this works: the GitOps path

Manual `kubectl apply` is step one. The full GitOps flow continues:

1. Move these manifests into a **GitOps repository**
2. **Kustomize** base + overlays per environment
3. **FluxCD** reconciles the cluster from git (no manual apply)
4. **Flux image automation** bumps the image tag when CI pushes

That is the full loop — this project becomes the payload flowing through
it.
