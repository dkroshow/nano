// Package setup owns read-only Claude context resolution, instruction-source
// evidence and workspace inspection caches. It uses internal/inspection for Git
// reads. Session routes and mutable provider state stay with their app owners;
// bounded file access remains in internal/files.
package setup
