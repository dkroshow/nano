// Package usage polls registered account-usage sources, retains normalized
// observations, and serves their HTTP read models. It owns no provider client.
//
// Nano registers three outbound reads when usage is enabled:
//   - Codex: account/rateLimits/read on the local supervised app-server.
//   - Claude: GET https://api.anthropic.com/api/oauth/usage with Claude's
//     read-only OAuth access token.
//   - Grok: initialize then _x.ai/billing through the local supervised leader.
//
// Failures back off per source. Turning usage off registers no sources, opens
// no store, starts no poller, and makes none of these calls.
package usage
