# URL Shortener — Kubernetes Learning Project

A small but complete web app built for one purpose: **learning how a real
workload travels from code to Kubernetes to GitOps.**

It is deliberately simple. The complexity lives where the learning is:
probes, manifests, CI on self-hosted runners, and (later) FluxCD.

## What it does

```text
Browser                React UI (shorten a URL, get QR code)
   │
   ▼
Go API  ──POST /api/shorten──►  PostgreSQL (links table)
   │
   ▼
GET /s/{code}  ──302 redirect──►  original URL
```

- Shorten a long URL → get a 6-character code (`/s/abc123`)
- QR code of the short URL, generated client-side
- Click counter per link
- Optional **Google sign-in** — each link records its owner, and signed-in
  users get a "My links" list. Without credentials configured, the app
  still works anonymously.

## Architecture

```text
┌────────────────────── container image ──────────────────────┐
│  Go binary (single process)                                 │
│    ├─ /api/shorten        create short link                 │
│    ├─ /s/{code}           redirect                          │
│    ├─ /auth/google/*      OAuth login + callback            │
│    ├─ /api/me, /api/me/links, /api/logout   session/user   │
│    ├─ /api/healthz        liveness probe endpoint           │
│    ├─ /api/readyz         readiness probe endpoint          │
│    └─ /                   serves embedded React build       │
└──────────────────────────────┬──────────────────────────────┘
                               │
                        PostgreSQL (StatefulSet + PVC)
```

One image serves both API and frontend (the React build is embedded into
the Go binary). That keeps the Kubernetes footprint small: 1 Deployment,
1 Service — plus Postgres.

## Repository layout

```text
url-shortener/
├── backend/               Go API
│   ├── main.go            server start, graceful shutdown
│   ├── handlers.go        endpoints + probes
│   ├── auth.go            Google OAuth code flow + JWT session
│   ├── store.go           Postgres layer + auto-migration
│   ├── handlers_test.go   tests (no DB needed — fake store)
│   └── web/dist/          where CI copies the React build
├── frontend/              React (Vite) single-page app
├── k8s/                   Kubernetes manifests (apply in number order)
├── .gitlab-ci.yml         CI: lint → test → buildx → push
├── .env.example           copy to .env for local SSO credentials
├── Dockerfile             multi-stage build
├── docker-compose.yaml    local full-stack testing
├── DEPLOY.md              ← how to deploy on Kubernetes
└── README.md              this file
```

## The probes (core learning topic)

| Probe | Endpoint | Question it answers | On failure |
|---|---|---|---|
| startup | `/api/readyz` | Did the app finish starting (DB connected)? | Kill pod after 60s |
| readiness | `/api/readyz` | Can it serve traffic **right now**? | Remove from Service (no restart) |
| liveness | `/api/healthz` | Is the process alive? | Restart container |

Read the comments in `backend/handlers.go` and `k8s/05-api-deployment.yaml`
— they explain *why* liveness must NOT check the database.

## Google SSO (optional)

Sign-in uses the **OAuth 2.0 authorization code flow**, handled entirely
by the backend (`backend/auth.go`). The browser never sees the client
secret.

```text
Browser ──GET /auth/google/login──► Go API
   │                                  │ random "state" saved as a cookie
   ▼                                  ▼
Google consent page  ◄──302──  redirect with client_id + state
   │ user clicks Allow
   ▼
Go API ◄──302── /auth/google/callback?code=…&state=…
   │  1. state matches cookie? (CSRF check)
   │  2. exchange code → tokens (carries the CLIENT SECRET)
   │  3. ask Google's userinfo endpoint who this is
   │  4. upsert into users table, link ownership = Google "sub"
   │  5. issue our own session: JWT in an HttpOnly cookie
   ▼
Frontend reads /api/me → shows avatar + "My links"
```

Configuration (all via environment variables):

| Variable | Required for SSO | Meaning |
|---|---|---|
| `GOOGLE_CLIENT_ID` | yes | OAuth Client ID from Google Cloud Console |
| `GOOGLE_CLIENT_SECRET` | yes | Client secret — backend only, never in the frontend |
| `SESSION_SECRET` | yes | Signs the session JWT (`openssl rand -hex 32`) |
| `PUBLIC_BASE_URL` | yes | Public app URL; builds the redirect URI |

Setup in Google Cloud Console → APIs & Services → Credentials →
Create OAuth Client ID (**Web application**):

- **Authorized JavaScript origins**: `http://localhost:8080` (origin only)
- **Authorized redirect URIs**: `http://localhost:8080/auth/google/callback`
- OAuth consent screen: External, Publishing status **Testing**, add your
  Gmail as a test user.

For local Docker testing: `cp .env.example .env`, fill in the values,
`docker compose up --build`. Leave the two Google variables empty and
everything else keeps working — the sign-in button simply hides itself.

## Local development

Prerequisites: Go ≥ 1.24, Node ≥ 20, Docker (for Postgres).

**1. Start Postgres** (throwaway container for dev):

```bash
docker run --name shortener-pg -e POSTGRES_USER=shortener \
  -e POSTGRES_PASSWORD=dev-password -e POSTGRES_DB=shortener \
  -p 5432:5432 -d postgres:16-alpine
```

**2. Start the backend** (auto-creates the `links` table):

```bash
cd backend
POSTGRES_PASSWORD=dev-password go run .
# → "database ready, schema ensured" ... "server starting" port=8080
```

**3. Start the frontend** (proxies `/api` and `/s` to :8080):

```bash
cd frontend
npm install
npm run dev
# → open http://localhost:5173
```

**4. Try it with curl:**

```bash
curl -X POST localhost:8080/api/shorten \
  -H 'Content-Type: application/json' \
  -d '{"url":"https://kubernetes.io/docs"}'
# {"code":"abc123","short_url":"/s/abc123",...}

curl -i localhost:8080/s/abc123     # → HTTP 302, Location: https://kubernetes.io/docs
curl localhost:8080/api/readyz      # → {"status":"ready"}
```

**Run checks:**

```bash
cd backend  && go test ./...        # unit tests (no DB needed)
cd frontend && npm run lint && npm run build
```

## CI pipeline

`.gitlab-ci.yml` — three stages:

```text
push / merge request
   │
   ├─ lint:     gofmt → go vet → eslint
   ├─ test:     go test → vite build
   │
   └─ build (main branch only, requires REGISTRY_URL variable):
        docker buildx (multi-arch) → push
        tags: <commit-sha> always, + latest on main
```

The build stage is **skipped** until you set `REGISTRY_URL` in GitLab
CI/CD variables — lint and test still run on every push. Full story
in `DEPLOY.md`.

## Cleanup (when done practicing)

```bash
docker rm -f shortener-pg                       # local dev postgres
kubectl delete namespace url-shortener          # everything in-cluster
```

## Next steps in the learning path

1. **Kustomize**: convert `k8s/` into base + overlays (dev/prod)
2. **FluxCD**: deploy from a Git repo instead of `kubectl apply`
3. **Image automation**: Flux updates the image tag when CI pushes
4. **HPA**: autoscale the API on CPU


---

#### Testing CI PR number 1

#### Testing CI Commit main Number 0

---