package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// MemStore — a fake Store for tests. No database needed.
// ---------------------------------------------------------------------

type MemStore struct {
	mu        sync.Mutex
	links     map[string]Link
	users     map[string]UserInfo
	pingError error // set this to simulate a broken database
}

func NewMemStore() *MemStore {
	return &MemStore{links: make(map[string]Link), users: make(map[string]UserInfo)}
}

func (m *MemStore) Save(_ context.Context, code, url, title string, owner *string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.links[code]; exists {
		return ErrDuplicate
	}
	t := title
	m.links[code] = Link{Code: code, URL: url, Title: &t, CreatedAt: time.Now(), Owner: owner}
	return nil
}

func (m *MemStore) Get(_ context.Context, code string) (Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return Link{}, ErrNotFound
	}
	return l, nil
}

func (m *MemStore) IncrementClicks(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return ErrNotFound
	}
	l.Clicks++
	m.links[code] = l
	return nil
}

func (m *MemStore) UpsertUser(_ context.Context, u UserInfo) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[u.Sub] = u
	return nil
}

func (m *MemStore) ListLinksByOwner(_ context.Context, sub string) ([]Link, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Link
	for _, l := range m.links {
		if l.Owner != nil && *l.Owner == sub {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m *MemStore) Ping(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pingError
}

func (m *MemStore) CountLinksByOwner(_ context.Context, sub string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, l := range m.links {
		if l.Owner != nil && *l.Owner == sub {
			count++
		}
	}
	return count, nil
}

func (m *MemStore) DeleteLink(_ context.Context, code, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return ErrNotFound
	}
	if l.Owner == nil || *l.Owner != owner {
		return ErrNotFound
	}
	delete(m.links, code)
	return nil
}

func (m *MemStore) UpdateLink(_ context.Context, code, owner, title, url string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return ErrNotFound
	}
	if l.Owner == nil || *l.Owner != owner {
		return ErrNotFound
	}
	if title != "" {
		l.Title = &title
	}
	if url != "" {
		l.URL = url
	}
	m.links[code] = l
	return nil
}

// ---------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------

// testGoogleConfig returns an ENABLED config for auth tests.
func testGoogleConfig() GoogleConfig {
	return GoogleConfig{
		ClientID:       "test-client-id",
		ClientSecret:   "test-secret",
		BaseURL:        "http://localhost:8080",
		SessionSecret:  []byte("test-session-secret"),
		SessionMaxAge:  time.Hour,
		RedirectURI:    "http://localhost:8080/auth/google/callback",
		GoogleLoginURL: "https://accounts.google.com/o/oauth2/v2/auth?client_id=test-client-id&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fauth%2Fgoogle%2Fcallback&response_type=code&scope=openid+email+profile",
	}
}

// testMaxLinks is the per-user quota used in tests. Set high enough
// that existing tests never hit it.
const testMaxLinks = 100

func testServer(store Store, google GoogleConfig) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandlers(store, logger, google, testMaxLinks).Routes()
}

// discardLogger keeps test output clean.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// signSessionCookie mints a real session cookie the same way the real
// handler does — lets tests act as a signed-in user.
func signSessionCookie(google GoogleConfig, info UserInfo) *http.Cookie {
	h := NewHandlers(NewMemStore(), discardLogger(), google, testMaxLinks)
	rec := httptest.NewRecorder()
	if err := h.setSessionCookie(rec, info); err != nil {
		panic(err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" {
			return c
		}
	}
	panic("no session cookie issued")
}

func doPost(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------
// Probe tests
// ---------------------------------------------------------------------

func TestHealthzAlwaysOK(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/api/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: want 200, got %d", rec.Code)
	}
}

func TestReadyzOKWhenDatabaseReachable(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/api/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz: want 200, got %d", rec.Code)
	}
}

func TestReadyzFailsWhenDatabaseDown(t *testing.T) {
	store := NewMemStore()
	store.pingError = errors.New("connection refused")

	rec := doGet(t, testServer(store, testGoogleConfig()), "/api/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with broken db: want 503, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------
// Shorten tests
// ---------------------------------------------------------------------

func TestShortenCreatesLink(t *testing.T) {
	g := testGoogleConfig()
	store := NewMemStore()
	h := testServer(store, g)
	cookie := signSessionCookie(g, UserInfo{Sub: "test-user", Email: "t@t.co"})

	b, _ := json.Marshal(shortenRequest{URL: "https://example.com/some/page"})
	req := httptest.NewRequest(http.MethodPost, "/api/shorten", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("shorten: want 201, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	var resp shortenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Code) != 6 {
		t.Errorf("code length: want 6, got %d (%q)", len(resp.Code), resp.Code)
	}
	if resp.ShortURL != "/s/"+resp.Code {
		t.Errorf("short_url: want /s/%s, got %s", resp.Code, resp.ShortURL)
	}
}

func TestShortenRejectsInvalidURLs(t *testing.T) {
	g := testGoogleConfig()
	h := testServer(NewMemStore(), g)
	cookie := signSessionCookie(g, UserInfo{Sub: "test-user"})

	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"no scheme", "example.com"},
		{"wrong scheme", "ftp://example.com"},
		{"javascript scheme", "javascript:alert(1)"},
		{"no host", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(shortenRequest{URL: tc.url})
			req := httptest.NewRequest(http.MethodPost, "/api/shorten", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("url %q: want 400, got %d", tc.url, rec.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Redirect tests
// ---------------------------------------------------------------------

func TestRedirectSendsToOriginalURL(t *testing.T) {
	store := NewMemStore()
	_ = store.Save(context.Background(), "abc123", "https://example.com/target", "", nil)
	h := testServer(store, testGoogleConfig())

	rec := doGet(t, h, "/s/abc123")
	if rec.Code != http.StatusFound {
		t.Fatalf("redirect: want 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://example.com/target" {
		t.Errorf("Location: want https://example.com/target, got %q", loc)
	}

	// Click counter must have increased.
	link, err := store.Get(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if link.Clicks != 1 {
		t.Errorf("clicks: want 1, got %d", link.Clicks)
	}
}

func TestRedirectUnknownCodeIs404(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/s/zzzzzz")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestRedirectRejectsWeirdCodes(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/s/..%2Fetc%2Fpasswd")
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("weird code: want 400 or 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------
// Stats tests
// ---------------------------------------------------------------------

func TestStatsReturnsLinkData(t *testing.T) {
	store := NewMemStore()
	_ = store.Save(context.Background(), "abc123", "https://example.com", "", nil)
	h := testServer(store, testGoogleConfig())

	rec := doGet(t, h, "/api/links/abc123")
	if rec.Code != http.StatusOK {
		t.Fatalf("stats: want 200, got %d", rec.Code)
	}
	var link Link
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if link.URL != "https://example.com" {
		t.Errorf("url: want https://example.com, got %q", link.URL)
	}
}

// ---------------------------------------------------------------------
// Code generation tests
// ---------------------------------------------------------------------

func TestRandomCodeUsesSafeAlphabet(t *testing.T) {
	for i := 0; i < 1000; i++ {
		code, err := randomCode(6)
		if err != nil {
			t.Fatalf("randomCode: %v", err)
		}
		if len(code) != 6 {
			t.Fatalf("length: want 6, got %d", len(code))
		}
		for _, c := range code {
			if !bytes.ContainsRune([]byte(codeAlphabet), c) {
				t.Fatalf("character %q not in alphabet", c)
			}
		}
	}
}

func TestRandomCodeIsVaried(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		code, _ := randomCode(6)
		seen[code] = true
	}
	if len(seen) < 95 { // astronomically unlikely to collide more than 5x
		t.Errorf("expected mostly unique codes, got %d unique of 100", len(seen))
	}
}

// ---------------------------------------------------------------------
// Auth tests (Google SSO)
// ---------------------------------------------------------------------

func TestGoogleLoginDisabledReturns503(t *testing.T) {
	// Zero-value GoogleConfig = SSO not configured.
	h := testServer(NewMemStore(), GoogleConfig{})
	rec := doGet(t, h, "/auth/google/login")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when SSO unconfigured, got %d", rec.Code)
	}
}

func TestGoogleLoginRedirectsToGoogleWithState(t *testing.T) {
	h := testServer(NewMemStore(), testGoogleConfig())
	rec := doGet(t, h, "/auth/google/login")

	if rec.Code != http.StatusFound {
		t.Fatalf("want 302 redirect to Google, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	for _, want := range []string{"accounts.google.com", "client_id=test-client-id", "state="} {
		if !strings.Contains(loc, want) {
			t.Errorf("redirect Location missing %q: %s", want, loc)
		}
	}

	// A state cookie must be set so the callback can verify it.
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "oauth_state" {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("oauth_state cookie not set on login")
	}
	if !strings.Contains(loc, "state="+stateCookie.Value) {
		t.Errorf("redirect state param does not match state cookie")
	}
}

func TestGoogleCallbackRejectsStateMismatch(t *testing.T) {
	h := testServer(NewMemStore(), testGoogleConfig())
	req := httptest.NewRequest(http.MethodGet,
		"/auth/google/callback?code=somecode&state=WRONG", nil)
	req.AddCookie(&http.Cookie{Name: "oauth_state", Value: "right-value"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("state mismatch: want 400, got %d", rec.Code)
	}
}

func TestGoogleCallbackRejectsMissingStateCookie(t *testing.T) {
	h := testServer(NewMemStore(), testGoogleConfig())
	// No oauth_state cookie at all — e.g. an attacker tricking a
	// victim's browser into visiting the callback URL.
	rec := doGet(t, h, "/auth/google/callback?code=somecode&state=xyz")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing state cookie: want 400, got %d", rec.Code)
	}
}

func TestMeRequiresAuth(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/api/me")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me without cookie: want 401, got %d", rec.Code)
	}
}

func TestMyLinksRequiresAuth(t *testing.T) {
	rec := doGet(t, testServer(NewMemStore(), testGoogleConfig()), "/api/me/links")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/me/links without cookie: want 401, got %d", rec.Code)
	}
}

func TestSessionCookieRoundtrip(t *testing.T) {
	g := testGoogleConfig()
	h := testServer(NewMemStore(), g)

	info := UserInfo{Sub: "google-123", Email: "dawwi@example.com", Name: "Dawwi"}
	cookie := signSessionCookie(g, info)

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("/api/me with session: want 200, got %d", rec.Code)
	}
	var got UserInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if got.Sub != "google-123" || got.Email != "dawwi@example.com" {
		t.Errorf("user mismatch: got %+v", got)
	}
}

func TestTamperedSessionCookieIsRejected(t *testing.T) {
	g := testGoogleConfig()
	h := testServer(NewMemStore(), g)

	// Sign a cookie with a DIFFERENT secret — must be rejected.
	tampered := testGoogleConfig()
	tampered.SessionSecret = []byte("different-secret")
	cookie := signSessionCookie(tampered, UserInfo{Sub: "google-123"})

	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered JWT: want 401, got %d", rec.Code)
	}
}

func TestShortenRecordsOwnerWhenSignedIn(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	h := testServer(store, g)

	b, _ := json.Marshal(shortenRequest{URL: "https://example.com/mine"})
	req := httptest.NewRequest(http.MethodPost, "/api/shorten", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(signSessionCookie(g, UserInfo{Sub: "google-123", Email: "a@b.co"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("shorten: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp shortenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	link, err := store.Get(context.Background(), resp.Code)
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if link.Owner == nil || *link.Owner != "google-123" {
		t.Errorf("owner: want google-123, got %v", link.Owner)
	}
}

func TestShortenRequiresAuth(t *testing.T) {
	rec := doPost(t, testServer(NewMemStore(), testGoogleConfig()), "/api/shorten", shortenRequest{URL: "https://example.com/anon"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("shorten without login: want 401, got %d", rec.Code)
	}
}

func TestMyLinksReturnsOnlyOwnedLinks(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	me := "google-123"
	other := "google-999"
	_ = store.Save(context.Background(), "own123", "https://mine.com", "", &me)
	_ = store.Save(context.Background(), "oth456", "https://theirs.com", "", &other)
	_ = store.Save(context.Background(), "anon78", "https://anon.com", "", nil)

	h := testServer(store, g)
	req := httptest.NewRequest(http.MethodGet, "/api/me/links", nil)
	req.AddCookie(signSessionCookie(g, UserInfo{Sub: me}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("my links: want 200, got %d", rec.Code)
	}
	var links []Link
	if err := json.Unmarshal(rec.Body.Bytes(), &links); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(links) != 1 || links[0].Code != "own123" {
		t.Errorf("want exactly my link own123, got %+v", links)
	}
}

func TestLogoutClearsSessionCookie(t *testing.T) {
	h := testServer(NewMemStore(), testGoogleConfig())
	rec := doPost(t, h, "/api/logout", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("logout: want 200, got %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "session" && c.MaxAge >= 0 {
			t.Errorf("logout must expire the session cookie, MaxAge=%d", c.MaxAge)
			return
		}
	}
}

// ---------------------------------------------------------------------
// Quota tests
// ---------------------------------------------------------------------

func TestShortenQuotaExceeded(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	sub := "quota-user"
	// Fill the quota (testMaxLinks = 100).
	for i := 0; i < testMaxLinks; i++ {
		_ = store.Save(context.Background(), fmt.Sprintf("q%d", i), "https://example.com", "", &sub)
	}

	h := testServer(store, g)
	cookie := signSessionCookie(g, UserInfo{Sub: sub})

	b, _ := json.Marshal(shortenRequest{URL: "https://example.com/over"})
	req := httptest.NewRequest(http.MethodPost, "/api/shorten", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("quota exceeded: want 403, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestShortenSucceedsAtQuotaLimit(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	sub := "quota-user2"
	// Fill to 99 (testMaxLinks - 1), the 100th should succeed.
	for i := 0; i < testMaxLinks-1; i++ {
		_ = store.Save(context.Background(), fmt.Sprintf("ql%d", i), "https://example.com", "", &sub)
	}

	h := testServer(store, g)
	cookie := signSessionCookie(g, UserInfo{Sub: sub})

	b, _ := json.Marshal(shortenRequest{URL: "https://example.com/just-in"})
	req := httptest.NewRequest(http.MethodPost, "/api/shorten", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("at quota limit: want 201, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------
// Delete tests
// ---------------------------------------------------------------------

func TestDeleteLinkRemovesOwnedLink(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	sub := "del-user"
	_ = store.Save(context.Background(), "del123", "https://example.com", "", &sub)
	h := testServer(store, g)
	cookie := signSessionCookie(g, UserInfo{Sub: sub})

	req := httptest.NewRequest(http.MethodDelete, "/api/links/del123", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("delete owned: want 200, got %d", rec.Code)
	}
	// Verify it's gone.
	_, err := store.Get(context.Background(), "del123")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected link to be deleted, got err %v", err)
	}
}

func TestDeleteLinkNotOwnedReturns404(t *testing.T) {
	store := NewMemStore()
	g := testGoogleConfig()
	subA := "user-a"
	subB := "user-b"
	_ = store.Save(context.Background(), "del456", "https://example.com", "", &subA)
	h := testServer(store, g)
	cookie := signSessionCookie(g, UserInfo{Sub: subB})

	req := httptest.NewRequest(http.MethodDelete, "/api/links/del456", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete not owned: want 404, got %d", rec.Code)
	}
}

func TestDeleteLinkNotFoundReturns404(t *testing.T) {
	g := testGoogleConfig()
	h := testServer(NewMemStore(), g)
	cookie := signSessionCookie(g, UserInfo{Sub: "some-user"})

	req := httptest.NewRequest(http.MethodDelete, "/api/links/nonexist", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete non-existent: want 404, got %d", rec.Code)
	}
}

func TestDeleteLinkRequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/api/links/abc123", nil)
	rec := httptest.NewRecorder()
	testServer(NewMemStore(), testGoogleConfig()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("delete without auth: want 401, got %d", rec.Code)
	}
}

func TestUpdateLinkTitle(t *testing.T) {
	store := NewMemStore()
	google := testGoogleConfig()
	h := testServer(store, google)

	user := UserInfo{Sub: "test-user", Email: "test@example.com", Name: "Test"}
	cookie := signSessionCookie(google, user)

	// Create a link first
	body := bytes.NewReader([]byte(`{"url":"https://example.com","title":"old title"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/shorten", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d", rec.Code)
	}
	var created shortenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Update the title
	body2 := bytes.NewReader([]byte(`{"title":"new title"}`))
	req2 := httptest.NewRequest(http.MethodPatch, "/api/links/"+created.Code, body2)
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("update: want 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// Verify the title changed
	link, err := store.Get(context.Background(), created.Code)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if link.Title == nil || *link.Title != "new title" {
		t.Fatalf("title: want 'new title', got %v", link.Title)
	}
}

func TestUpdateLinkRequiresAuth(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/api/links/abc123",
		bytes.NewReader([]byte(`{"title":"new"}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	testServer(NewMemStore(), testGoogleConfig()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("update without auth: want 401, got %d", rec.Code)
	}
}

func TestUpdateLinkNotFound(t *testing.T) {
	store := NewMemStore()
	google := testGoogleConfig()
	h := testServer(store, google)
	user := UserInfo{Sub: "test-user", Email: "test@example.com", Name: "Test"}
	cookie := signSessionCookie(google, user)

	body := bytes.NewReader([]byte(`{"title":"new"}`))
	req := httptest.NewRequest(http.MethodPatch, "/api/links/nonexistent", body)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("update nonexistent: want 404, got %d", rec.Code)
	}
}
