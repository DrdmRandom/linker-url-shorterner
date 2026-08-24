# =====================================================================
# Multi-stage build: 3 stages, each with a job
#
#   Stage 1  frontend-build : Node compiles React → static files
#   Stage 2  backend-build  : Go compiles the API → ONE static binary
#                             (the React files are EMBEDDED into it)
#   Stage 3  runtime        : tiny alpine image with ONLY the binary
#
# Why multi-stage? Build tools (node_modules, Go toolchain, ~1 GB)
# never end up in the final image. Final image ≈ 20 MB.
# =====================================================================

# ---- Stage 1: build the React frontend ----
FROM node:22-alpine AS frontend-build
WORKDIR /app/frontend
# Copy package files FIRST and install, before copying source code.
# Docker caches layers: if only .jsx files change, `npm ci` is skipped.
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

# ---- Stage 2: build the Go backend ----
FROM golang:1.24-alpine AS backend-build
WORKDIR /app
# Same caching trick: download modules before copying source.
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# Inject the built frontend where go:embed expects it.
COPY --from=frontend-build /app/frontend/dist ./web/dist
# CGO_ENABLED=0 → fully static binary, runs on any base image.
# -ldflags="-s -w" strips debug info → smaller binary.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /url-shortener .

# ---- Stage 3: runtime ----
FROM alpine:3.21
# Run as a non-root user (Kubernetes Pod Security best practice).
RUN addgroup -S app && adduser -S app -G app
COPY --from=backend-build /url-shortener /usr/local/bin/url-shortener
USER app
EXPOSE 8080
# Exec form (JSON array): the binary becomes PID 1 and receives SIGTERM
# directly, which enables the graceful shutdown in main.go.
ENTRYPOINT ["url-shortener"]
