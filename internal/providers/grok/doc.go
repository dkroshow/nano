// Package grok owns the Grok adapter and its capability declaration. It
// depends on the provider contract, lifecycle and parsing model. App selects
// the concrete adapter at construction; shared code never tests its name.
// Runtime operation factories bind the native client behind this adapter.
package grok
