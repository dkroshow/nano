package remoteauth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dkroshow/nano/internal/httpapi"
)

const testRemoteHost = "nano.example.ts.net:7690"

// A local page must not be embedded by another website.
func TestLocalGuardFrameProtection(t *testing.T) {
	guard := GuardLocal(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	page := httptest.NewRecorder()
	guard.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://localhost:7690/", nil))
	if page.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("local page lacks frame protection")
	}
}

func testRemoteAuthenticator(t *testing.T) (*Authenticator, http.Handler) {
	t.Helper()
	store := testRemoteAuthStore(t)
	auth := New(store, testRemoteHost)
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/api/private", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "private") })
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "nano") })
	return auth, auth.Guard(mux)
}

func remoteRequest(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "https://"+testRemoteHost+target, body)
	r.Host = testRemoteHost
	return r
}

func formRequest(target string, values url.Values) *http.Request {
	r := remoteRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://"+testRemoteHost)
	return r
}

func TestRemoteAuthGuardFailsClosed(t *testing.T) {
	_, guarded := testRemoteAuthenticator(t)

	wrongHost := remoteRequest(http.MethodGet, "/", nil)
	wrongHost.Host = "other.example.ts.net:7690"
	wrong := httptest.NewRecorder()
	guarded.ServeHTTP(wrong, wrongHost)
	if wrong.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wrong host status = %d", wrong.Code)
	}

	document := remoteRequest(http.MethodGet, "/threads", nil)
	document.Header.Set("Accept", "text/html")
	doc := httptest.NewRecorder()
	guarded.ServeHTTP(doc, document)
	if doc.Code != http.StatusSeeOther || !strings.HasPrefix(doc.Header().Get("Location"), "/auth/login?") {
		t.Fatalf("document response = %d, location %q", doc.Code, doc.Header().Get("Location"))
	}

	api := httptest.NewRecorder()
	guarded.ServeHTTP(api, remoteRequest(http.MethodGet, "/api/private", nil))
	var apiError httpapi.ErrorEnvelope
	if err := json.Unmarshal(api.Body.Bytes(), &apiError); err != nil || api.Code != http.StatusUnauthorized || apiError.Error.Code != "authentication_required" {
		t.Fatalf("api response = %d %q", api.Code, api.Body.String())
	}
	status := httptest.NewRecorder()
	guarded.ServeHTTP(status, remoteRequest(http.MethodGet, "/auth/status", nil))
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status response = %d %q", status.Code, status.Body.String())
	}

	health := httptest.NewRecorder()
	guarded.ServeHTTP(health, remoteRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("health response = %d %q", health.Code, health.Body.String())
	}
}

// The HTTP boundary must grant only a locally configured, exact proxy identity,
// without letting that grant bypass Host checks or write provenance.
func TestRemoteOwnerLoginBypassPreservesTrustBoundary(t *testing.T) {
	auth, guarded := testRemoteAuthenticator(t)
	if err := auth.store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	const owner = "owner@example.com"
	request := remoteRequest(http.MethodGet, "/", nil)
	request.Header.Set("Tailscale-User-Login", owner)
	before := httptest.NewRecorder()
	guarded.ServeHTTP(before, request)
	if before.Code != http.StatusSeeOther {
		t.Fatalf("empty allow-list granted access: %d", before.Code)
	}

	mux := http.NewServeMux()
	auth.RegisterRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "nano") })
	local := GuardLocal(mux)
	values := url.Values{"allowed_logins": {owner}}
	configure := func(origin, host string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7690/security/allowed-logins", strings.NewReader(values.Encode()))
		r.Host = host
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", origin)
		return r
	}
	for _, tc := range []struct {
		origin, host string
		want         int
	}{
		{"https://evil.example", "127.0.0.1:7690", http.StatusForbidden},
		{"http://evil.example", "evil.example", http.StatusMisdirectedRequest},
		{"http://127.0.0.1:7690", "127.0.0.1:7690", http.StatusSeeOther},
	} {
		response := httptest.NewRecorder()
		local.ServeHTTP(response, configure(tc.origin, tc.host))
		if response.Code != tc.want {
			t.Fatalf("configure %s %s: %d %s", tc.host, tc.origin, response.Code, response.Body.String())
		}
	}

	for _, tc := range []struct {
		name   string
		logins []string
		want   int
	}{
		{"owner", []string{owner}, http.StatusOK},
		{"other user", []string{"other@example.com"}, http.StatusSeeOther},
		{"unknown user", []string{"unknown@example.com"}, http.StatusSeeOther},
		{"missing identity", nil, http.StatusSeeOther},
		{"empty identity", []string{""}, http.StatusSeeOther},
		{"prefix", []string{"owner@"}, http.StatusSeeOther},
		{"different case", []string{"OWNER@example.com"}, http.StatusSeeOther},
		{"multiple identities", []string{owner, "other@example.com"}, http.StatusSeeOther},
		{"merged identities", []string{owner + ", other@example.com"}, http.StatusSeeOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := remoteRequest(http.MethodGet, "/", nil)
			if tc.logins != nil {
				r.Header["Tailscale-User-Login"] = tc.logins
			}
			response := httptest.NewRecorder()
			guarded.ServeHTTP(response, r)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d", response.Code, tc.want)
			}
			if tc.want == http.StatusSeeOther && !strings.HasPrefix(response.Header().Get("Location"), "/auth/login?") {
				t.Fatalf("password redirect missing: %s", response.Header().Get("Location"))
			}
			if len(response.Result().Cookies()) != 0 {
				t.Fatal("identity access minted a browser cookie")
			}
		})
	}
	for _, tc := range []struct {
		name, method, origin, host, path string
		want                             int
	}{
		{"missing Origin", http.MethodPost, "", testRemoteHost, "/api/private", http.StatusForbidden},
		{"cross-site Origin", http.MethodPost, "https://evil.example", testRemoteHost, "/api/private", http.StatusForbidden},
		{"same Origin", http.MethodPost, "https://" + testRemoteHost, testRemoteHost, "/api/private", http.StatusOK},
		{"wrong Host", http.MethodGet, "", "evil.example", "/", http.StatusMisdirectedRequest},
		{"remote cannot configure", http.MethodPost, "https://" + testRemoteHost, testRemoteHost, "/security/allowed-logins", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := remoteRequest(tc.method, tc.path, nil)
			r.Host = tc.host
			r.Header.Set("Tailscale-User-Login", owner)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			response := httptest.NewRecorder()
			guarded.ServeHTTP(response, r)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
	page := httptest.NewRecorder()
	local.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7690/security", nil))
	if !strings.Contains(page.Body.String(), owner) {
		t.Fatal("local security page does not show configured login")
	}
	loopback := httptest.NewRecorder()
	local.ServeHTTP(loopback, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7690/", nil))
	if loopback.Code != http.StatusOK {
		t.Fatalf("loopback now needs authentication: %d", loopback.Code)
	}
	if valid, err := auth.store.verifyPassword("correct horse battery staple"); err != nil || !valid {
		t.Fatal("login configuration changed password")
	}
	// A grant must not erase an existing browser's cookie-bound identity:
	// recovery and logout still need the validated session context.
	token, _, err := auth.store.createSession("test browser", owner)
	if err != nil {
		t.Fatal(err)
	}
	statusReq := remoteRequest(http.MethodGet, "/auth/status", nil)
	statusReq.Header.Set("Tailscale-User-Login", owner)
	statusReq.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: token})
	status := httptest.NewRecorder()
	guarded.ServeHTTP(status, statusReq)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"authenticated":true`) {
		t.Fatalf("allowed login lost existing cookie session: %d %s", status.Code, status.Body.String())
	}
	logoutReq := formRequest("/auth/logout", nil)
	logoutReq.Header.Set("Tailscale-User-Login", owner)
	logoutReq.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: token})
	logout := httptest.NewRecorder()
	guarded.ServeHTTP(logout, logoutReq)
	if _, valid, err := auth.store.validateSession(token); err != nil || valid {
		t.Fatalf("allowed login logout did not revoke cookie session: valid=%v err=%v", valid, err)
	}
}

func TestRemoteLoginCookieSurvivesReloadAndProtectsWrites(t *testing.T) {
	auth, guarded := testRemoteAuthenticator(t)
	if err := auth.store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}

	loginReq := formRequest("/auth/login", url.Values{
		"password": {"correct horse battery staple"},
		"next":     {"/threads?view=working"},
	})
	loginReq.Header.Set("User-Agent", "Mozilla/5.0 (iPhone)")
	loginReq.Header.Set("Tailscale-User-Login", "owner@example.com")
	login := httptest.NewRecorder()
	guarded.ServeHTTP(login, loginReq)
	if login.Code != http.StatusSeeOther || login.Header().Get("Location") != "/threads?view=working" {
		t.Fatalf("login response = %d, location %q, body %q", login.Code, login.Header().Get("Location"), login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %#v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != remoteSessionCookie || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge < 29*24*60*60 {
		t.Fatalf("unsafe session cookie = %#v", cookie)
	}

	// A hard reload is another ordinary document request. The persistent,
	// HttpOnly cookie must be sufficient; no frontend state participates.
	reloadReq := remoteRequest(http.MethodGet, "/", nil)
	reloadReq.Header.Set("Accept", "text/html")
	reloadReq.AddCookie(cookie)
	reload := httptest.NewRecorder()
	guarded.ServeHTTP(reload, reloadReq)
	if reload.Code != http.StatusOK || reload.Body.String() != "nano" {
		t.Fatalf("reload response = %d %q", reload.Code, reload.Body.String())
	}

	statusReq := remoteRequest(http.MethodGet, "/auth/status", nil)
	statusReq.AddCookie(cookie)
	status := httptest.NewRecorder()
	guarded.ServeHTTP(status, statusReq)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"remote":true`) || !strings.Contains(status.Body.String(), `"authenticated":true`) {
		t.Fatalf("remote status = %d %q", status.Code, status.Body.String())
	}

	writeReq := remoteRequest(http.MethodPost, "/api/private", nil)
	writeReq.AddCookie(cookie)
	writeWithoutOrigin := httptest.NewRecorder()
	guarded.ServeHTTP(writeWithoutOrigin, writeReq)
	if writeWithoutOrigin.Code != http.StatusForbidden {
		t.Fatalf("write without Origin status = %d", writeWithoutOrigin.Code)
	}

	writeReq = remoteRequest(http.MethodPost, "/api/private", nil)
	writeReq.AddCookie(cookie)
	writeReq.Header.Set("Origin", "https://"+testRemoteHost)
	write := httptest.NewRecorder()
	guarded.ServeHTTP(write, writeReq)
	if write.Code != http.StatusOK {
		t.Fatalf("authenticated API response = %d %q", write.Code, write.Body.String())
	}

	logoutReq := formRequest("/auth/logout", nil)
	logoutReq.AddCookie(cookie)
	logout := httptest.NewRecorder()
	guarded.ServeHTTP(logout, logoutReq)
	if logout.Code != http.StatusSeeOther || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout response = %d, cookies %#v", logout.Code, logout.Result().Cookies())
	}
}

func TestRemotePasswordChangeRequiresCurrentPasswordAndRevokesSessions(t *testing.T) {
	auth, guarded := testRemoteAuthenticator(t)
	if err := auth.store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	token, session, err := auth.store.createSession("Mozilla/5.0 (Macintosh)", "owner@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: remoteSessionCookie, Value: token}

	wrong := formRequest("/security/password", url.Values{
		"current_password": {"not the current password"},
		"password":         {"another strong remote password"},
		"confirm_password": {"another strong remote password"},
	})
	wrong.AddCookie(cookie)
	wrongResponse := httptest.NewRecorder()
	guarded.ServeHTTP(wrongResponse, wrong)
	if wrongResponse.Code != http.StatusBadRequest || !strings.Contains(wrongResponse.Body.String(), "current password did not match") {
		t.Fatalf("wrong-current response = %d %q", wrongResponse.Code, wrongResponse.Body.String())
	}
	if _, valid, err := auth.store.validateSession(token); err != nil || !valid {
		t.Fatalf("session %s removed after rejected change: valid %v, err %v", session.ID, valid, err)
	}

	change := formRequest("/security/password", url.Values{
		"current_password": {"correct horse battery staple"},
		"password":         {"another strong remote password"},
		"confirm_password": {"another strong remote password"},
	})
	change.AddCookie(cookie)
	changed := httptest.NewRecorder()
	guarded.ServeHTTP(changed, change)
	if changed.Code != http.StatusSeeOther || changed.Header().Get("Location") != "/auth/login?changed=1" {
		t.Fatalf("change response = %d, location %q", changed.Code, changed.Header().Get("Location"))
	}
	if _, valid, err := auth.store.validateSession(token); err != nil || valid {
		t.Fatalf("old session valid after rotation = %v, err %v", valid, err)
	}
	if valid, err := auth.store.verifyPassword("another strong remote password"); err != nil || !valid {
		t.Fatalf("new password valid = %v, err %v", valid, err)
	}
}

func TestRemoteLoginRejectsCrossSiteAndRateLimitsFailures(t *testing.T) {
	auth, guarded := testRemoteAuthenticator(t)
	if err := auth.store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	crossSite := formRequest("/auth/login", url.Values{"password": {"correct horse battery staple"}})
	crossSite.Header.Set("Origin", "https://evil.example")
	crossSite.Header.Set("Sec-Fetch-Site", "same-origin")
	rejected := httptest.NewRecorder()
	guarded.ServeHTTP(rejected, crossSite)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("cross-site status = %d", rejected.Code)
	}

	for i := 0; i < 5; i++ {
		attempt := formRequest("/auth/login", url.Values{"password": {"wrong password"}})
		attempt.Header.Set("Tailscale-User-Login", "owner@example.com")
		response := httptest.NewRecorder()
		guarded.ServeHTTP(response, attempt)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i+1, response.Code)
		}
	}
	blocked := formRequest("/auth/login", url.Values{"password": {"correct horse battery staple"}})
	blocked.Header.Set("Tailscale-User-Login", "owner@example.com")
	response := httptest.NewRecorder()
	guarded.ServeHTTP(response, blocked)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("blocked response = %d, retry %q", response.Code, response.Header().Get("Retry-After"))
	}
}

func TestRemoteLoginAllowsMobileFormWithoutOrigin(t *testing.T) {
	auth, guarded := testRemoteAuthenticator(t)
	if err := auth.store.setPassword("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}

	for name, setFallback := range map[string]func(*http.Request){
		"fetch metadata": func(r *http.Request) {
			r.Header.Set("Sec-Fetch-Site", "same-origin")
		},
		"same-origin referer": func(r *http.Request) {
			r.Header.Set("Referer", "https://"+testRemoteHost+"/auth/login?next=%2F")
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := formRequest("/auth/login", url.Values{"password": {"correct horse battery staple"}})
			request.Header.Del("Origin")
			setFallback(request)
			response := httptest.NewRecorder()
			guarded.ServeHTTP(response, request)
			if response.Code != http.StatusSeeOther {
				t.Fatalf("mobile login response = %d %q", response.Code, response.Body.String())
			}
		})
	}

	crossSite := formRequest("/auth/login", url.Values{"password": {"correct horse battery staple"}})
	crossSite.Header.Del("Origin")
	crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
	crossSite.Header.Set("Referer", "https://"+testRemoteHost+"/auth/login")
	rejected := httptest.NewRecorder()
	guarded.ServeHTTP(rejected, crossSite)
	if rejected.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch metadata status = %d", rejected.Code)
	}
}

func TestLocalSecuritySetupRequiresLoopbackAndSameOrigin(t *testing.T) {
	auth, _ := testRemoteAuthenticator(t)
	mux := http.NewServeMux()
	auth.RegisterRoutes(mux)
	localStatus := httptest.NewRecorder()
	mux.ServeHTTP(localStatus, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7690/auth/status", nil))
	if localStatus.Code != http.StatusOK || !strings.Contains(localStatus.Body.String(), `"remote":false`) {
		t.Fatalf("local status = %d %q", localStatus.Code, localStatus.Body.String())
	}

	foreign := httptest.NewRequest(http.MethodGet, "http://evil.example/security", nil)
	foreign.Host = "evil.example"
	foreignResponse := httptest.NewRecorder()
	mux.ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusForbidden {
		t.Fatalf("foreign host status = %d", foreignResponse.Code)
	}

	values := url.Values{
		"password":         {"correct horse battery staple"},
		"confirm_password": {"correct horse battery staple"},
	}
	setup := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7690/security/password", strings.NewReader(values.Encode()))
	setup.Host = "127.0.0.1:7690"
	setup.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setup.Header.Set("Origin", "https://evil.example")
	crossSiteResponse := httptest.NewRecorder()
	mux.ServeHTTP(crossSiteResponse, setup)
	if crossSiteResponse.Code != http.StatusForbidden {
		t.Fatalf("local cross-site status = %d", crossSiteResponse.Code)
	}

	setup = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7690/security/password", strings.NewReader(values.Encode()))
	setup.Host = "127.0.0.1:7690"
	setup.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setup.Header.Set("Origin", "http://127.0.0.1:7690")
	setupResponse := httptest.NewRecorder()
	mux.ServeHTTP(setupResponse, setup)
	if setupResponse.Code != http.StatusSeeOther || setupResponse.Header().Get("Location") != "/security?changed=1" {
		t.Fatalf("setup response = %d, location %q, body %q", setupResponse.Code, setupResponse.Header().Get("Location"), setupResponse.Body.String())
	}
	valid, err := auth.store.verifyPassword("correct horse battery staple")
	if err != nil || !valid {
		t.Fatalf("configured password valid = %v, err %v", valid, err)
	}
}
