// Package app constructs stores and controllers, mounts routes and listeners,
// and owns process startup and shutdown. Domain state and HTTP operations belong
// to their lower owners; cmd/nanod only invokes Main.
package app
