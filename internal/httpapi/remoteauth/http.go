package remoteauth

// remote_auth_http.go is the remote trust boundary. Tailscale Serve points at
// this guarded listener; local agents continue to use Nano's ordinary
// 127.0.0.1 listener and never inherit browser-cookie authentication.

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dkroshow/nano/internal/httpapi"
)

const remoteSessionCookie = "__Host-nano_session"

type remoteAuthContextKey int

const (
	remoteRequestKey remoteAuthContextKey = iota
	remoteSessionKey
)

type Authenticator struct {
	store      *Store
	remoteHost string
	limiter    *remoteLoginLimiter
}

type remoteLoginLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	attempts map[string]remoteLoginWindow
}

type remoteLoginWindow struct {
	Started time.Time
	Count   int
	Blocked time.Time
}

func New(store *Store, remoteHost string) *Authenticator {
	return &Authenticator{
		store: store, remoteHost: strings.TrimSpace(strings.ToLower(remoteHost)),
		limiter: &remoteLoginLimiter{now: time.Now, attempts: make(map[string]remoteLoginWindow)},
	}
}

func (a *Authenticator) RemoteHost() string { return a.remoteHost }

func (a *Authenticator) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", a.handleLogin)
	mux.HandleFunc("/auth/logout", a.handleLogout)
	mux.HandleFunc("/auth/status", a.handleStatus)
	mux.HandleFunc("/security", a.handleSecurity)
	mux.HandleFunc("/security/password", a.handlePassword)
	mux.HandleFunc("/security/allowed-logins", a.handleAllowedLogins)
	mux.HandleFunc("/security/sessions/revoke", a.handleRevokeSession)
	mux.HandleFunc("/security/sessions/revoke-all", a.handleRevokeAllSessions)
}

func (a *Authenticator) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpapi.WriteHTTPError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	_, authenticated := CurrentSession(r)
	httpapi.WriteJSON(w, map[string]bool{
		"remote":        IsRemoteRequest(r),
		"authenticated": authenticated,
	})
}

func IsRemoteRequest(r *http.Request) bool {
	remote, _ := r.Context().Value(remoteRequestKey).(bool)
	return remote
}

func CurrentSession(r *http.Request) (Session, bool) {
	session, ok := r.Context().Value(remoteSessionKey).(Session)
	return session, ok
}

func (a *Authenticator) Guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setRemoteSecurityHeaders(w)
		if a.remoteHost == "" || !strings.EqualFold(r.Host, a.remoteHost) {
			httpapi.WriteHTTPError(w, "remote host not configured", http.StatusMisdirectedRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), remoteRequestKey, true))
		if r.URL.Path == "/healthz" {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				httpapi.WriteHTTPError(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprintln(w, "ok")
			return
		}
		if r.URL.Path == "/auth/login" {
			next.ServeHTTP(w, r)
			return
		}
		allowed := false
		cookie, err := r.Cookie(remoteSessionCookie)
		if err == nil {
			session, valid, validateErr := a.store.validateSession(cookie.Value)
			if validateErr != nil {
				httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			if valid {
				if session.RefreshCookie {
					setRemoteSessionCookie(w, cookie.Value, session.ExpiresAt)
				}
				r = r.WithContext(context.WithValue(r.Context(), remoteSessionKey, session))
				allowed = true
			}
		}
		// Serve strips client-supplied identity headers. This listener must stay
		// loopback-only, with Serve as its only untrusted ingress.
		if logins := r.Header.Values("Tailscale-User-Login"); !allowed && len(logins) == 1 {
			var err error
			allowed, err = a.store.loginAllowed(logins[0])
			if err != nil {
				httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if allowed {
			if requestChangesState(r) && !a.sameOrigin(r) {
				httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if requestWantsDocument(r) {
			nextPath := safeNextPath(r.URL.RequestURI())
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(nextPath), http.StatusSeeOther)
			return
		}
		httpapi.WriteError(w, http.StatusUnauthorized, "authentication_required", "Authentication required")
	})
}

// GuardLocal protects every route on the trusted loopback listener, including
// diagnostics and maintenance routes mounted outside the application mux.
// Requests without browser provenance remain available to local tools.
func GuardLocal(next http.Handler, allowDevOrigin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		if !isLoopbackRequestHost(r.Host) {
			httpapi.WriteHTTPError(w, "local host required", http.StatusMisdirectedRequest)
			return
		}
		if (requestChangesState(r) || strings.HasPrefix(r.URL.Path, "/debug/pprof/")) && !localRequestOriginAllowed(r, allowDevOrigin) {
			httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localRequestOriginAllowed(r *http.Request, allowDevOrigin bool) bool {
	if site := strings.TrimSpace(strings.ToLower(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
		return false
	}
	if origins, present := r.Header["Origin"]; present {
		if len(origins) != 1 {
			return false
		}
		origin := strings.TrimSpace(origins[0])
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
			return false
		}
		return requestOriginMatches(origin, localScheme(r), r.Host) || (allowDevOrigin && requestOriginMatches(origin, "http", "localhost:5173"))
	}
	if site := strings.TrimSpace(strings.ToLower(r.Header.Get("Sec-Fetch-Site"))); site != "" {
		return site == "same-origin"
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		return requestOriginMatches(referer, localScheme(r), r.Host) || (allowDevOrigin && requestOriginMatches(referer, "http", "localhost:5173"))
	}
	return true
}

// localScheme is the scheme a same-origin page on the trusted listener used:
// the loopback HTTPS listener serves the same handler as plain HTTP.
func localScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func requestChangesState(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
}

func setRemoteSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

func SetPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	// Some mobile browsers omit Origin on ordinary HTML form submissions.
	// Keep a same-origin Referer available as a secure fallback for those forms.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
}

func requestWantsDocument(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || strings.HasPrefix(r.URL.Path, "/events") {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html") || !strings.Contains(r.URL.Path, ".")
}

func safeNextPath(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	return parsed.RequestURI()
}

func setRemoteSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: remoteSessionCookie, Value: token, Path: "/", MaxAge: maxAge, Expires: expires,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearRemoteSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: remoteSessionCookie, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (l *remoteLoginLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	window := l.attempts[key]
	if now.Before(window.Blocked) {
		return false, window.Blocked.Sub(now)
	}
	if window.Started.IsZero() || now.Sub(window.Started) >= 15*time.Minute {
		delete(l.attempts, key)
	}
	return true, 0
}

func (l *remoteLoginLimiter) failed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	window := l.attempts[key]
	if window.Started.IsZero() || now.Sub(window.Started) >= 15*time.Minute {
		window = remoteLoginWindow{Started: now}
	}
	window.Count++
	if window.Count >= 5 {
		window.Blocked = now.Add(15 * time.Minute)
	}
	l.attempts[key] = window
}

func (l *remoteLoginLimiter) succeeded(key string) {
	l.mu.Lock()
	delete(l.attempts, key)
	l.mu.Unlock()
}

func (a *Authenticator) loginLimitKey(r *http.Request) string {
	login := strings.TrimSpace(strings.ToLower(r.Header.Get("Tailscale-User-Login")))
	if login == "" {
		login = "unknown-tailnet-user"
	}
	return login
}

func (a *Authenticator) sameOrigin(r *http.Request) bool {
	wantScheme := localScheme(r)
	wantHost := strings.ToLower(r.Host)
	if IsRemoteRequest(r) {
		wantScheme = "https"
		wantHost = a.remoteHost
	} else if !isLoopbackRequestHost(r.Host) {
		return false
	}
	matches := func(rawURL string) bool { return requestOriginMatches(rawURL, wantScheme, wantHost) }

	// Origin is the strongest signal. If a browser sends a concrete value, a
	// mismatch must fail even when another header claims the request is local.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && origin != "null" {
		parsed, err := url.Parse(origin)
		return err == nil && parsed.Path == "" && matches(origin)
	}
	if fetchSite := strings.TrimSpace(strings.ToLower(r.Header.Get("Sec-Fetch-Site"))); fetchSite != "" {
		return fetchSite == "same-origin"
	}
	if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
		return matches(referer)
	}
	return false
}

func requestOriginMatches(rawURL, scheme, host string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && parsed.Scheme == scheme && strings.EqualFold(parsed.Host, host)
}

func isLoopbackRequestHost(hostport string) bool {
	host := hostport
	if split, _, err := net.SplitHostPort(hostport); err == nil {
		host = split
	}
	host = strings.Trim(host, "[]")
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !IsRemoteRequest(r) {
		httpapi.WriteNotFound(w)
		return
	}
	configured, err := a.store.configured()
	if err != nil {
		httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	SetPageHeaders(w)
	next := safeNextPath(r.URL.Query().Get("next"))
	switch r.Method {
	case http.MethodGet:
		_ = remoteLoginTemplate.Execute(w, remoteLoginPage{Configured: configured, Next: next, Changed: r.URL.Query().Get("changed") == "1"})
	case http.MethodPost:
		if !configured {
			httpapi.WriteHTTPError(w, "set the Nano remote password locally first", http.StatusPreconditionFailed)
			return
		}
		if !a.sameOrigin(r) {
			httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
		if err := r.ParseForm(); err != nil {
			httpapi.WriteHTTPError(w, "invalid login", http.StatusBadRequest)
			return
		}
		next = safeNextPath(r.FormValue("next"))
		key := a.loginLimitKey(r)
		if allowed, retry := a.limiter.allow(key); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retry.Seconds()))))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = remoteLoginTemplate.Execute(w, remoteLoginPage{Configured: true, Next: next, Error: "Too many attempts. Try again in 15 minutes."})
			return
		}
		valid, verifyErr := a.store.verifyPassword(r.FormValue("password"))
		if verifyErr != nil {
			httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			a.limiter.failed(key)
			w.WriteHeader(http.StatusUnauthorized)
			_ = remoteLoginTemplate.Execute(w, remoteLoginPage{Configured: true, Next: next, Error: "That password did not match."})
			return
		}
		a.limiter.succeeded(key)
		token, session, createErr := a.store.createSession(r.UserAgent(), r.Header.Get("Tailscale-User-Login"))
		if createErr != nil {
			httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		setRemoteSessionCookie(w, token, session.ExpiresAt)
		http.Redirect(w, r, next, http.StatusSeeOther)
	default:
		w.Header().Set("Allow", "GET, POST")
		httpapi.WriteHTTPError(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !IsRemoteRequest(r) {
		httpapi.WriteNotFound(w)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpapi.WriteHTTPError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.sameOrigin(r) {
		httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
		return
	}
	if session, ok := CurrentSession(r); ok {
		_ = a.store.revokeSession(session.ID)
	}
	clearRemoteSessionCookie(w)
	http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
}

type remoteLoginPage struct {
	Configured bool
	Changed    bool
	Next       string
	Error      string
}

type remoteSecurityPage struct {
	Configured    bool
	Remote        bool
	Changed       bool
	Revoked       bool
	Error         string
	Sessions      []remoteSecuritySession
	AllowedLogins string
	LoginsChanged bool
}

type remoteSecuritySession struct {
	ID        string
	Label     string
	Login     string
	LastSeen  string
	Expires   string
	IsCurrent bool
}

func (a *Authenticator) handleSecurity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		httpapi.WriteHTTPError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !IsRemoteRequest(r) && !isLoopbackRequestHost(r.Host) {
		httpapi.WriteHTTPError(w, "local access only", http.StatusForbidden)
		return
	}
	configured, err := a.store.configured()
	if err != nil {
		httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	sessions, err := a.store.listSessions()
	if err != nil {
		httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	logins, err := a.store.allowedLogins()
	if err != nil {
		httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	current, _ := CurrentSession(r)
	page := remoteSecurityPage{
		Configured: configured, Remote: IsRemoteRequest(r),
		Changed: r.URL.Query().Get("changed") == "1", Revoked: r.URL.Query().Get("revoked") == "1",
		AllowedLogins: strings.Join(logins, "\n"), LoginsChanged: r.URL.Query().Get("logins_changed") == "1",
	}
	for _, session := range sessions {
		page.Sessions = append(page.Sessions, remoteSecuritySession{
			ID: session.ID, Label: session.browserLabel(), Login: session.TailscaleLogin,
			LastSeen: session.LastSeenAt.Local().Format("Jan 2, 3:04 PM"),
			Expires:  session.ExpiresAt.Local().Format("Jan 2, 2006"), IsCurrent: session.ID == current.ID,
		})
	}
	SetPageHeaders(w)
	_ = remoteSecurityTemplate.Execute(w, page)
}

func (a *Authenticator) handleAllowedLogins(w http.ResponseWriter, r *http.Request) {
	if IsRemoteRequest(r) || !isLoopbackRequestHost(r.Host) {
		httpapi.WriteHTTPError(w, "local access only", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpapi.WriteHTTPError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.sameOrigin(r) {
		httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		httpapi.WriteHTTPError(w, "invalid login form", http.StatusBadRequest)
		return
	}
	if err := a.store.setAllowedLogins(r.PostForm.Get("allowed_logins")); err != nil {
		a.renderSecurityError(w, r, err.Error()+".")
		return
	}
	http.Redirect(w, r, "/security?logins_changed=1", http.StatusSeeOther)
}

func (a *Authenticator) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpapi.WriteHTTPError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !IsRemoteRequest(r) && !isLoopbackRequestHost(r.Host) {
		httpapi.WriteHTTPError(w, "local access only", http.StatusForbidden)
		return
	}
	if !a.sameOrigin(r) {
		httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		httpapi.WriteHTTPError(w, "invalid password form", http.StatusBadRequest)
		return
	}
	password := r.FormValue("password")
	if password != r.FormValue("confirm_password") {
		a.renderSecurityError(w, r, "The new passwords do not match.")
		return
	}
	if IsRemoteRequest(r) {
		valid, err := a.store.verifyPassword(r.FormValue("current_password"))
		if err != nil {
			httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			a.renderSecurityError(w, r, "The current password did not match.")
			return
		}
	}
	if err := a.store.setPassword(password); err != nil {
		a.renderSecurityError(w, r, err.Error()+".")
		return
	}
	clearRemoteSessionCookie(w)
	if IsRemoteRequest(r) {
		http.Redirect(w, r, "/auth/login?changed=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/security?changed=1", http.StatusSeeOther)
}

func (a *Authenticator) renderSecurityError(w http.ResponseWriter, r *http.Request, message string) {
	configured, _ := a.store.configured()
	logins, _ := a.store.allowedLogins()
	SetPageHeaders(w)
	w.WriteHeader(http.StatusBadRequest)
	_ = remoteSecurityTemplate.Execute(w, remoteSecurityPage{Configured: configured, Remote: IsRemoteRequest(r), Error: message, AllowedLogins: strings.Join(logins, "\n")})
}

func (a *Authenticator) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpapi.WriteHTTPError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.sameOrigin(r) {
		httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		httpapi.WriteHTTPError(w, "invalid session", http.StatusBadRequest)
		return
	}
	id := r.FormValue("session_id")
	if err := a.store.revokeSession(id); err != nil {
		httpapi.WriteHTTPError(w, "invalid session", http.StatusBadRequest)
		return
	}
	if current, ok := CurrentSession(r); ok && current.ID == id {
		clearRemoteSessionCookie(w)
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/security?revoked=1", http.StatusSeeOther)
}

func (a *Authenticator) handleRevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpapi.WriteHTTPError(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.sameOrigin(r) {
		httpapi.WriteHTTPError(w, "invalid request origin", http.StatusForbidden)
		return
	}
	if err := a.store.revokeAllSessions(); err != nil {
		httpapi.WriteHTTPError(w, "remote authentication unavailable", http.StatusServiceUnavailable)
		return
	}
	clearRemoteSessionCookie(w)
	if IsRemoteRequest(r) {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/security?revoked=1", http.StatusSeeOther)
}

var remoteLoginTemplate = template.Must(template.New("remote-login").Parse(remoteAuthPageStart + `
<main class="card login-card">
  <div class="mark">N</div>
  <p class="eyebrow">CODEX NANO</p>
  <h1>Remote access</h1>
  {{if .Changed}}<div class="notice good">Password changed. Sign in again.</div>{{end}}
  {{if not .Configured}}
    <p class="lede">Remote access is locked. Set a password from <code>http://127.0.0.1:7690/security</code> on the machine that runs Nano first.</p>
  {{else}}
    <p class="lede">Enter the Nano password. This browser stays signed in for up to 30 days.</p>
    {{if .Error}}<div class="notice bad" role="alert">{{.Error}}</div>{{end}}
    <form method="post" action="/auth/login" class="stack">
      <input type="hidden" name="next" value="{{.Next}}">
      <label for="password">Password</label>
      <input id="password" name="password" type="password" autocomplete="current-password" required autofocus>
      <button type="submit">Unlock Nano</button>
    </form>
  {{end}}
  <p class="fine">Protected by Tailscale HTTPS and Nano authentication.</p>
</main>` + remoteAuthPageEnd))

var remoteSecurityTemplate = template.Must(template.New("remote-security").Parse(remoteAuthPageStart + `
<main class="card security-card">
  <div class="topline"><div><p class="eyebrow">CODEX NANO</p><h1>Remote security</h1></div><a href="/">Back to Nano</a></div>
  {{if .Changed}}<div class="notice good">Password saved. Every remote browser was signed out.</div>{{end}}
  {{if .Revoked}}<div class="notice good">Session access revoked.</div>{{end}}
  {{if .LoginsChanged}}<div class="notice good">Password-free logins saved.</div>{{end}}
  {{if .Error}}<div class="notice bad" role="alert">{{.Error}}</div>{{end}}
  <section>
    <h2>Password-free Tailscale logins</h2>
    <p>Devices signed in to these exact Tailscale logins can open Nano without its password. Everyone else still needs the password. Tagged devices and public Funnel requests always need it.</p>
    {{if .Remote}}<p>Change this list from the local security page on this Mac.</p>{{else}}
    <form method="post" action="/security/allowed-logins" class="stack">
      <label for="allowed_logins">Full login addresses, one per line</label>
      <textarea id="allowed_logins" name="allowed_logins" rows="3" spellcheck="false" autocomplete="off">{{.AllowedLogins}}</textarea>
      <p class="fine">An empty list requires the password for every remote login. These grants do not expire. To require the password again, remove the login here; locking a browser does not revoke a login grant.</p>
      <button type="submit">Save logins</button>
    </form>
    {{end}}
  </section>
  <section>
    <h2>{{if .Configured}}Change password{{else}}Set remote password{{end}}</h2>
    <p>The password stays on this Mac as a one-way Argon2id hash. Use at least 14 characters.</p>
    <form method="post" action="/security/password" class="stack">
      {{if .Remote}}<label for="current_password">Current password</label><input id="current_password" name="current_password" type="password" autocomplete="current-password" required>{{end}}
      <label for="password">New password</label><input id="password" name="password" type="password" minlength="14" autocomplete="new-password" required>
      <label for="confirm_password">Confirm new password</label><input id="confirm_password" name="confirm_password" type="password" minlength="14" autocomplete="new-password" required>
      <button type="submit">{{if .Configured}}Change password{{else}}Set password{{end}}</button>
    </form>
  </section>
  <section>
    <div class="topline"><div><h2>Signed-in browsers</h2><p>Sessions expire after 30 idle days and always end after 90 days.</p></div>
    {{if .Sessions}}<form method="post" action="/security/sessions/revoke-all"><button class="secondary danger" type="submit">Sign out all</button></form>{{end}}</div>
    {{if .Sessions}}<div class="sessions">{{range .Sessions}}
      <div class="session"><div><strong>{{.Label}}{{if .IsCurrent}} <span class="pill">This browser</span>{{end}}</strong><span>{{if .Login}}{{.Login}} · {{end}}Last used {{.LastSeen}} · Expires {{.Expires}}</span></div>
      <form method="post" action="/security/sessions/revoke"><input type="hidden" name="session_id" value="{{.ID}}"><button class="secondary" type="submit">Revoke</button></form></div>
    {{end}}</div>{{else}}<p class="empty">No remote browsers are signed in.</p>{{end}}
  </section>
  {{if .Remote}}<form method="post" action="/auth/logout"><button class="secondary" type="submit">Sign out this browser</button></form>{{end}}
</main>` + remoteAuthPageEnd))

const remoteAuthPageStart = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Nano remote security</title><style>
:root{color-scheme:dark;font-family:Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#090b0f;color:#eef0f4}*{box-sizing:border-box}body{min-height:100vh;margin:0;display:grid;place-items:center;padding:28px;background:radial-gradient(circle at 50% 0,#192131 0,transparent 42%),#090b0f}.card{width:min(100%,680px);background:rgba(19,22,29,.96);border:1px solid #2a303c;border-radius:18px;padding:32px;box-shadow:0 24px 80px #0009}.login-card{width:min(100%,430px)}.mark{width:42px;height:42px;display:grid;place-items:center;border-radius:12px;background:#d7ff64;color:#111;font-weight:900;margin-bottom:22px}.eyebrow{font-size:11px;letter-spacing:.16em;color:#9aa3b3;margin:0 0 7px}h1{font-size:27px;letter-spacing:-.03em;margin:0 0 12px}h2{font-size:17px;margin:0 0 6px}p{color:#aeb5c2;line-height:1.5;margin:0}.lede{margin:0 0 22px}.fine{font-size:12px;margin-top:22px;color:#747e8e}.stack{display:grid;gap:10px;margin-top:18px}label{font-size:13px;color:#cbd0d9}input,textarea{width:100%;border:1px solid #343b48;border-radius:10px;background:#0c0f14;color:#fff;padding:12px 13px;font:inherit;outline:none}input:focus,textarea:focus{border-color:#d7ff64;box-shadow:0 0 0 3px #d7ff6422}button{border:0;border-radius:10px;background:#d7ff64;color:#111;padding:12px 16px;font-weight:750;cursor:pointer;margin-top:5px}button:hover{filter:brightness(1.06)}button.secondary{background:#262c37;color:#e5e8ee;padding:9px 12px;margin:0}.danger{color:#ffb8b8!important}.notice{padding:11px 13px;border-radius:10px;margin:16px 0;font-size:13px}.bad{background:#3a171b;color:#ffb8bf;border:1px solid #69262d}.good{background:#183223;color:#baf2cc;border:1px solid #28583a}code{font-size:12px;color:#d7ff64;overflow-wrap:anywhere}.topline{display:flex;align-items:flex-start;justify-content:space-between;gap:20px}.topline a{color:#d7ff64;text-decoration:none;font-size:13px}.security-card section{border-top:1px solid #2a303c;margin-top:26px;padding-top:24px}.sessions{display:grid;margin-top:15px;border:1px solid #2a303c;border-radius:12px;overflow:hidden}.session{display:flex;align-items:center;justify-content:space-between;gap:20px;padding:14px}.session+.session{border-top:1px solid #2a303c}.session strong,.session span{display:block}.session span{color:#8993a3;font-size:12px;margin-top:4px}.pill{display:inline!important;color:#cce889!important;background:#29351c;border-radius:99px;padding:2px 6px;margin-left:4px}.empty{margin-top:14px;padding:16px;border:1px dashed #303743;border-radius:12px}@media(max-width:520px){.card{padding:23px}.session,.topline{align-items:stretch;flex-direction:column}.session form button{width:100%}}
</style></head><body>`

const remoteAuthPageEnd = `</body></html>`
