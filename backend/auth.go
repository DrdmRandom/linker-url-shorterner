package main

// This file implements the OAuth 2.0 Authorization Code Flow with Google.
//
// The dance, step by step:
//
//   1. Browser: GET /auth/google/login
//      We invent a random "state", save it in a cookie, and redirect
//      the browser to Google's consent page (with the state attached).
//
//   2. Google: user picks an account and clicks Allow.
//
//   3. Google redirects the browser BACK to us:
//      GET /auth/google/callback?code=XYZ&state=ABC
//
//   4. We check state matches the cookie (CSRF protection!), then
//      exchange the one-time code for tokens — server-to-server,
//      using the client SECRET. The browser never sees the secret.
//
//   5. We ask Google's userinfo endpoint who the user is, upsert them
//      into our users table, and issue OUR OWN session (JWT cookie).
//      From here on, Google is out of the picture.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ---------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------

// GoogleConfig holds the OAuth client settings, read from the
// environment. In Kubernetes these come from a Secret.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	// BaseURL is the public address of THIS app, e.g.
	// http://localhost:8080 — it builds the redirect URI, which must
	// EXACTLY match what is registered in the Google console.
	BaseURL        string
	SessionSecret  []byte
	SessionMaxAge  time.Duration
	RedirectURI    string // computed: BaseURL + /auth/google/callback
	GoogleLoginURL string // computed
}

func GoogleConfigFromEnv() GoogleConfig {
	base := envOr("PUBLIC_BASE_URL", "http://localhost:8080")
	redirect := base + "/auth/google/callback"

	q := url.Values{}
	q.Set("client_id", os.Getenv("GOOGLE_CLIENT_ID"))
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	// state is added per-request in handleGoogleLogin.

	return GoogleConfig{
		ClientID:       os.Getenv("GOOGLE_CLIENT_ID"),
		ClientSecret:   os.Getenv("GOOGLE_CLIENT_SECRET"),
		BaseURL:        base,
		SessionSecret:  []byte(envOr("SESSION_SECRET", "insecure-dev-secret")),
		SessionMaxAge:  7 * 24 * time.Hour,
		RedirectURI:    redirect,
		GoogleLoginURL: "https://accounts.google.com/o/oauth2/v2/auth?" + q.Encode(),
	}
}

// Enabled reports whether SSO is configured at all. The app still
// works without it — shortening stays possible anonymously.
func (g GoogleConfig) Enabled() bool {
	return g.ClientID != "" && g.ClientSecret != ""
}

// ---------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------

// handleGoogleLogin is step 1: send the user to Google.
func (h *Handlers) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.google.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "Google SSO is not configured (set GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET)",
		})
		return
	}

	state, err := randomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// The state cookie lets the callback verify that THIS browser
	// started the login — that is what blocks CSRF: an attacker cannot
	// forge a callback whose state matches the victim's cookie.
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/auth",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600, // state is only valid for 10 minutes
	})

	u, _ := url.Parse(h.google.GoogleLoginURL)
	q := u.Query()
	q.Set("state", state)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// handleGoogleCallback is steps 3-5: Google sends the user back here.
func (h *Handlers) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	// --- CSRF check: state param must equal the state cookie ---------
	stateCookie, err := r.Cookie("oauth_state")
	stateParam := r.URL.Query().Get("state")
	http.SetCookie(w, &http.Cookie{ // always consume the cookie
		Name: "oauth_state", Value: "", Path: "/auth", MaxAge: -1,
	})
	if err != nil || stateParam == "" || stateParam != stateCookie.Value {
		http.Error(w, "login rejected: state mismatch (possible CSRF)", http.StatusBadRequest)
		return
	}

	// Google returns an error param if the user denied consent.
	if apiErr := r.URL.Query().Get("error"); apiErr != "" {
		http.Error(w, "Google returned an error: "+apiErr, http.StatusUnauthorized)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	// --- Exchange the one-time code for tokens ------------------------
	tok, err := h.google.exchangeCode(r.Context(), code, h.google.RedirectURI)
	if err != nil {
		h.logger.Error("token exchange failed", "error", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}

	// --- Ask Google who the user is -----------------------------------
	info, err := fetchUserInfo(r.Context(), tok.AccessToken)
	if err != nil {
		h.logger.Error("userinfo failed", "error", err)
		http.Error(w, "could not fetch user info", http.StatusBadGateway)
		return
	}

	// --- Upsert into our own users table -------------------------------
	if err := h.store.UpsertUser(r.Context(), info); err != nil {
		h.logger.Error("upsert user failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// --- Issue OUR session (JWT cookie) --------------------------------
	if err := h.setSessionCookie(w, info); err != nil {
		h.logger.Error("create session failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("user signed in", "email", info.Email)
	// Back to the SPA — the frontend will call /api/me.
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleMe returns the signed-in user (from the JWT cookie) or 401.
func (h *Handlers) handleMe(w http.ResponseWriter, r *http.Request) {
	info, ok := h.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleMyLinks lists only the links owned by the signed-in user —
// the ownership column finally becomes visible.
func (h *Handlers) handleMyLinks(w http.ResponseWriter, r *http.Request) {
	info, ok := h.userFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	links, err := h.store.ListLinksByOwner(r.Context(), info.Sub)
	if err != nil {
		h.logger.Error("list my links failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, links)
}

// handleLogout deletes the session cookie. Stateless JWT means there is
// nothing else to clean up server-side.
func (h *Handlers) handleLogout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleAuthConfig tells the frontend whether SSO is enabled and what
// the per-user quota is. No secrets here — just config flags.
func (h *Handlers) handleAuthConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":         h.google.Enabled(),
		"maxLinksPerUser": h.maxLinksPerUser,
	})
}

// handleDeleteLink removes a link — but only if the signed-in user
// owns it. This prevents one user from deleting another user's links.
func (h *Handlers) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
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

	if err := h.store.DeleteLink(r.Context(), code, info.Sub); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "link not found or not owned by you",
			})
			return
		}
		h.logger.Error("delete link failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ---------------------------------------------------------------------
// JWT session helpers
// ---------------------------------------------------------------------

// setSessionCookie signs a JWT with the user's identity and stores it
// in an HttpOnly cookie — JavaScript cannot read it, which is the point.
func (h *Handlers) setSessionCookie(w http.ResponseWriter, info UserInfo) error {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":     info.Sub,
		"email":   info.Email,
		"name":    info.Name,
		"picture": info.Picture,
		"iat":     now.Unix(),
		"exp":     now.Add(h.google.SessionMaxAge).Unix(),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(h.google.SessionSecret)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.google.SessionMaxAge.Seconds()),
		// Set Secure:true in production behind HTTPS. Plain localhost
		// development uses http://, where Secure cookies would be dropped.
	})
	return nil
}

// userFromRequest validates the session cookie and returns the user.
func (h *Handlers) userFromRequest(r *http.Request) (UserInfo, bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return UserInfo{}, false
	}
	token, err := jwt.Parse(c.Value, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return h.google.SessionSecret, nil
	})
	if err != nil || !token.Valid {
		return UserInfo{}, false
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return UserInfo{}, false
	}
	info := UserInfo{
		Sub:     claimString(claims, "sub"),
		Email:   claimString(claims, "email"),
		Name:    claimString(claims, "name"),
		Picture: claimString(claims, "picture"),
	}
	return info, info.Sub != ""
}

func claimString(claims jwt.MapClaims, key string) string {
	s, _ := claims[key].(string)
	return s
}

// ---------------------------------------------------------------------
// Google API calls (server-to-server)
// ---------------------------------------------------------------------

// googleTokenResponse is the subset of the token endpoint reply we use.
type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// exchangeCode trades the one-time authorization code for tokens.
// This request carries the client SECRET — which is why it must happen
// in the backend, never in the browser.
func (g GoogleConfig) exchangeCode(ctx context.Context, code, redirectURI string) (*googleTokenResponse, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", g.ClientID)
	form.Set("client_secret", g.ClientSecret)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token", nil)
	if err != nil {
		return nil, err
	}
	req.URL.RawQuery = form.Encode()
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok googleTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access_token")
	}
	return &tok, nil
}

// UserInfo is who Google told us about the user. `Sub` is Google's
// stable unique ID for the account — it never changes, so it is the
// correct ownership key (emails can change; sub does not).
type UserInfo struct {
	Sub     string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture"`
}

// fetchUserInfo calls Google's userinfo endpoint with the access token.
func fetchUserInfo(ctx context.Context, accessToken string) (UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return UserInfo{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return UserInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return UserInfo{}, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}
	var info UserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return UserInfo{}, err
	}
	if info.Sub == "" {
		return UserInfo{}, errors.New("userinfo returned no subject")
	}
	return info, nil
}

// randomHex returns n random bytes as hex — used for the OAuth state.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
