// Package subscriptions delivers child completion notices through interact.
// It depends on interact; the server may invoke it after a completed-turn feed
// event, while provider and transport code must not depend on it.
package subscriptions
