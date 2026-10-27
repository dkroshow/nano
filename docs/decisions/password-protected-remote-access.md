# Password-protected remote access

Date: 2026-08-14
Status: accepted

## Decision

Nano keeps two separate HTTP trust boundaries:

- `127.0.0.1:7690` remains the trusted local interface for the browser,
  `nano-agent`, hosted agents, and scripts. Its API contract does not gain
  cookie authentication.
- `127.0.0.1:7693` is the protected remote interface. Tailscale Serve may
  proxy its external HTTPS port `7690` only to this listener. The listener
  accepts one configured `Host` and applies authentication before the shared
  Nano router.

The protected listener starts only when both `-remote-auth-port` and
`-remote-auth-host` are supplied. A wrong Host fails closed. Remote exposure stays off until the operator sets a password locally and
deliberately enables the reverse proxy.

## Owner login exception (2026-10-13)

The local security page also configures an allow-list of full Tailscale login
addresses. It defaults to empty. A protected request with exactly one
`Tailscale-User-Login` header that exactly matches a configured address may
proceed without a password cookie. Matching is case-sensitive, with no prefix,
wildcard, display-name, or device-name matching. The grant applies to every
device authenticated as that login, including future devices under the same
account. A different login or missing identity still requires the password.

This is an allow-list of the owner's identity, rather than a deny-list of one
other device. It keeps other users and new unrelated devices behind the
password. Tagged devices have no user identity header. Shared users receive
their own login, not the identity of the user who shared the server. Funnel
requests have no user identity. None of those identities inherits the owner's
grant.

Only the trusted local security page can change this list, with the existing
loopback Host and same-origin form checks. Remote password-authenticated
browsers and allowed logins cannot edit it. The list lives in
`remote_auth_allowed_logins` in the same `0600` remote-auth database; the
additive table starts empty and does not alter passwords or existing sessions.
Saving the list does not rotate the password or revoke cookie sessions.

The exception depends on Tailscale Serve's identity-header contract:
[Serve removes client-supplied identity headers before inserting verified
identity](https://tailscale.com/docs/features/tailscale-serve#identity-headers).
The protected listener must remain on loopback, with Serve as its remote
ingress. A local process can forge proxy headers, but already controls Nano
through the trusted local API; local compromise remains outside this boundary.
The login grant does not skip the protected Host check or state-changing
requests' Origin/CSRF checks.

Verification inspected Tailscale v1.102.4's
[`addTailscaleIdentityHeaders`](https://github.com/tailscale/tailscale/blob/v1.102.4/ipn/ipnlocal/serve.go).
It deletes incoming user headers before resolving the request source, returns
without a login for Funnel, unknown sources, or tagged nodes, and uses the
resolved user's login for other requests. Its upstream proxy-header test was
run locally with injected forged and duplicate login values for ordinary,
tagged, and unknown sources. Each backend saw only the verified login or no
login. This test uses synthetic source identities; it does not claim a probe
from another live tailnet device.

Login grants create no browser session and do not expire. **Lock Nano** applies
to password-cookie sessions, so it is absent for requests admitted by login
identity. To require a granted login to use the password again, remove it
locally. Existing password cookies remain valid until separately revoked.

## Password and session storage

The operator sets or rotates the password at
`http://127.0.0.1:7690/security`. The cleartext password is never persisted.
Nano stores an Argon2id hash with a random salt in
`~/.nanod/remote-auth.sqlite`, a separate `0600` database in a `0700`
directory. This database is not derived history and must survive catalog
rebuilds.

A successful login creates a random 256-bit opaque token. The database stores
only its SHA-256 hash. The browser receives the token in a host-only
`Secure`, `HttpOnly`, `SameSite=Strict` cookie. Sessions expire after 30 idle
days and after 90 days absolutely. Activity refreshes the idle deadline at
most once per day. Password changes revoke every session transactionally.

The security page lists signed-in browsers and can revoke one or all of them.
Remote password changes require the current password. Local changes rely on
Nano's existing localhost trust boundary, but still require a loopback Host
and exact same-origin POST so an unrelated website cannot mutate Nano through
the operator's browser.

An authenticated remote app reads `/auth/status`, which returns only two
booleans: whether this is the protected listener and whether the request has a
session. The settings menu then exposes **Lock Nano** only on that remote
surface. Locking posts to `/auth/logout`, revokes the current browser token,
clears its cookie, and returns directly to the password screen. It does not
disturb other signed-in browsers or local agent traffic.

## Request policy

Every protected request except `GET /healthz` and the login form requires a
valid session or the owner login exception above. Browser documents redirect
to login; APIs and streams return
`401`. Authenticated remote state changes require an exact HTTPS Origin. For
mobile form submissions that omit Origin, Nano accepts same-origin Fetch
Metadata or a same-origin Referer. A concrete mismatched Origin always fails.
The login endpoint permits five failures per Tailscale login in 15 minutes,
then returns `429` for the rest of the window.

The remote health response contains only `ok`; it does not forward internal
store errors. Login and security pages use no-store caching, a restrictive
content-security policy, frame denial, and no referrer.

## Consequences

- Hard refresh and browser restart do not prompt again while the persistent
  session is valid.
- The operator can require the password immediately on the current browser by
  choosing **Lock Nano**.
- A new or private browser must enter the password unless its verified login
  has the owner exception.
- Tailnet devices can reach the login form when Serve is enabled, but cannot
  read or operate Nano without the password or a configured login grant.
- Local compromise remains outside this boundary. A process already running
  as the operator can still use the trusted localhost API.
- Remote automation does not inherit browser cookies. If it is needed later,
  it requires a separately designed, scoped token contract.

## Verification

Backend tests cover password hashing, wrong-password behavior, rate limiting,
Host and Origin rejection, persistent-cookie attributes, hard-refresh access,
idle and absolute expiry, logout, and password-rotation revocation. Deployment
must additionally verify that local `:7690` stays available, direct protected
`:7693` requests reject the wrong Host, and the Tailscale URL cannot expose
Nano before a password exists.
