// Package httpapi owns Nano's HTTP transport conventions. It depends only on
// the standard library and owns compression, discovery document delivery and
// connection measurements. It does not register application routes. It may be used by edge packages; application and
// domain packages must not depend on it.
package httpapi
