// Package transcript decides whether a provider's saved transcript proves a
// prompt Nano delivered. SavedSettlement matches a stored prompt's exact
// binding, a native event id or native turn id, against the saved user rows;
// text, clocks and positions never establish identity. It has no store,
// network, clock or provider protocol.
package transcript
