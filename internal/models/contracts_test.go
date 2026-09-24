package models

import "github.com/gopact-ai/steve/internal/harness"

// Bind the local session, so it losing Settings fails the build rather than
// turning every probe into "asked, and it does not tell". The remote session
// reports the same way but is unexported by its package, so only this one
// can be bound from here.
var _ settingsReporter = (*harness.Session)(nil)
