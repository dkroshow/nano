// Package store owns the shared hosted metadata, execution and attachment
// stores. These adapters use the existing canonical tables and attachment
// file; moving the adapters here introduces no second durable record owner.
//
// Hosted admission and control belong to the parent hosted package. Provider
// transports, live sessions and their synchronization belong to sessions.
// This package does not depend on either of those consumers.
package store
