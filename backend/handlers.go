package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// webFS embeds the built React frontend INTO the Go binary.
// In dev this directory only holds a placeholder; CI copies the real
// build output here before `go build`. Result: ONE image serves both
// the API and the website.
//
//go:embed all:web/dist
var webFS embed.FS

// Handlers bundles the HTTP handlers with their dependencies.
type Handlers struct {
	store           Store
	logger          *slog.Logger
	google          GoogleConfig
	maxLinksPerUser int
}

func NewHandlers(store Store, logger *slog.Logger, google GoogleConfig, maxLinksPerUser int) *Handlers {
	return &Handlers{store: store, logger: logger, google: google, maxLinksPerUser: maxLinksPerUser}
}

// Routes wires URL patterns to handlers. Since Go 1.22 the standard
// library supports "METHOD /path/{param}" patterns — no router library
// needed for an API this size.
func (h *Handlers) Routes() http.Handler {
	mux := http.NewServeMux()

	// Kubernetes probes — see DEPLOY.md for how these map to the
	// livenessProbe / readinessProbe in the Deployment.
	mux.HandleFunc("GET /api/healthz", h.handleHealthz)
	mux.HandleFunc("GET /api/readyz", h.handleReadyz)

	// API
	mux.HandleFunc("POST /api/shorten", h.handleShorten)
	mux.HandleFunc("GET /api/links/{code}", h.handleStats)
	mux.HandleFunc("DELETE /api/links/{code}", h.handleDeleteLink)
		mux.HandleFunc("PATCH /api/links/{code}", h.handleUpdateLink)

	// Auth (Google SSO) — see auth.go for the flow explanation.
	mux.HandleFunc("GET /auth/google/login", h.handleGoogleLogin)
	mux.HandleFunc("GET /auth/google/callback", h.handleGoogleCallback)
	mux.HandleFunc("GET /api/auth/config", h.handleAuthConfig)
	mux.HandleFunc("GET /api/me", h.handleMe)
	mux.HandleFunc("GET /api/me/links", h.handleMyLinks)
	mux.HandleFunc("POST /api/logout", h.handleLogout)

	// Redirect endpoint — the whole point of the app.
	mux.HandleFunc("GET /s/{code}", h.handleRedirect)

	// Everything else: the React single-page app.
	mux.Handle("/", h.spaHandler())

	return logRequests(mux, h.logger)
}

// ---------------------------------------------------------------------
// Probe endpoints
// ---------------------------------------------------------------------

// handleHealthz is the LIVENESS probe: "is the process alive and not
// deadlocked?" It deliberately does NOT check the database. If the DB
// is down, killing and restarting this pod does not fix the DB — it
// only causes a crash loop. Keep liveness dumb.
func (h *Handlers) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz is the READINESS probe: "can this pod serve traffic
// right now?" It DOES check the database, because a pod that cannot
// reach its data should not receive requests. When this fails,
// Kubernetes removes the pod from the Service's endpoints (no traffic)
// but does NOT restart it — it keeps checking until it recovers.
func (h *Handlers) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := h.store.Ping(ctx); err != nil {
		h.logger.Warn("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not ready",
			"reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ---------------------------------------------------------------------
// API endpoints
// ---------------------------------------------------------------------

type shortenRequest struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type shortenResponse struct {
	Code     string `json:"code"`
	ShortURL string `json:"short_url"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Clicks   int    `json:"clicks"`
}

// handleShorten validates the URL, generates a short code, stores it.
func (h *Handlers) handleShorten(w http.ResponseWriter, r *http.Request) {
	// --- Require sign-in ---
	info, ok := h.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}

	// Limit request body size (1 MB) — cheap defense against abuse.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	cleanURL, err := validateURL(req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// --- Per-user quota check ---
	count, err := h.store.CountLinksByOwner(r.Context(), info.Sub)
	if err != nil {
		h.logger.Error("count links failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if count >= h.maxLinksPerUser {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("quota reached (%d links). Delete some to free up space.", h.maxLinksPerUser),
		})
		return
	}

	// Codes are random, so a collision is possible. On duplicate, just
	// roll a new code — up to 3 tries, then give up.
	var code string
	for attempt := 0; attempt < 3; attempt++ {
		code, err = randomCode(6)
		if err != nil {
			h.logger.Error("generate code failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		err = h.store.Save(r.Context(), code, cleanURL, req.Title, &info.Sub)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrDuplicate) {
			h.logger.Error("save link failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	if errors.Is(err, ErrDuplicate) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "could not allocate a short code, try again"})
		return
	}

	h.logger.Info("link created", "code", code, "url", cleanURL, "owner", info.Sub)
	writeJSON(w, http.StatusCreated, shortenResponse{
		Code:     code,
		ShortURL: "/s/" + code,
		URL:      cleanURL,
			Title:    req.Title,
		Clicks:   0,
	})
}

// handleRedirect resolves the code and sends a 302 to the original URL.
func (h *Handlers) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !validCodePattern(code) {
		http.Error(w, "invalid short code", http.StatusBadRequest)
		return
	}

	link, err := h.store.Get(r.Context(), code)
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "short link not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.logger.Error("lookup failed", "code", code, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Counting clicks is best-effort: if it fails, the redirect still
	// happens. Users care about the redirect, not the counter.
	if err := h.store.IncrementClicks(r.Context(), code); err != nil {
		h.logger.Warn("click count failed", "code", code, "error", err)
	}

	http.Redirect(w, r, link.URL, http.StatusFound)
}

// handleStats returns one link's data (handy for testing with curl).
func (h *Handlers) handleStats(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !validCodePattern(code) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid short code"})
		return
	}

	link, err := h.store.Get(r.Context(), code)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "link not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, link)
}

// handleUpdateLink changes the title and/or URL of a link. Only the
// owner can update their own links — the auth check enforces that.
func (h *Handlers) handleUpdateLink(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !validCodePattern(code) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid short code"})
		return
	}

	info, ok := h.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
		return
	}

	var body struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	// Validate the URL if one was provided
	if body.URL != "" {
		cleanURL, err := validateURL(body.URL)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		body.URL = cleanURL
	}

	if err := h.store.UpdateLink(r.Context(), code, info.Sub, body.Title, body.URL); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "link not found or not owned by you",
			})
			return
		}
		h.logger.Error("update link failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ---------------------------------------------------------------------
// Static frontend
// ---------------------------------------------------------------------

// spaHandler serves the embedded React build. Unknown paths fall back
// to index.html so client-side routing keeps working.
func (h *Handlers) spaHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		h.logger.Error("embedded frontend missing", "error", err)
		return http.NotFoundHandler()
	}

	// Dev guard: without a real build, show a helpful message instead
	// of a confusing 404.
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "Frontend not built. For local dev run `npm run dev` inside frontend/ (port 5173).", http.StatusNotFound)
		})
	}

	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(sub, p); err != nil {
				r.URL.Path = "/" // SPA fallback → index.html
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// validateURL accepts only absolute http(s) URLs. A URL shortener that
// accepts anything becomes an open-redirect gadget for phishing.
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("url is required")
	}
	if len(raw) > 2048 {
		return "", errors.New("url too long (max 2048 chars)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("url is not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("only http:// and https:// urls are allowed")
	}
	if u.Host == "" {
		return "", errors.New("url must include a host, e.g. https://example.com")
	}
	return u.String(), nil
}

// codeAlphabet excludes characters people confuse: l/1/I, o/0/O.
const codeAlphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"

var codePattern = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)

func validCodePattern(code string) bool {
	return codePattern.MatchString(code)
}

// randomCode generates a cryptographically random short code.
func randomCode(n int) (string, error) {
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = codeAlphabet[idx.Int64()]
	}
	return string(b), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// logRequests is tiny request-logging middleware: method, path, status,
// duration — one JSON line per request, visible via `kubectl logs`.
func logRequests(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
