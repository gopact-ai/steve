package onboard

import "github.com/gopact-ai/steve/internal/home"

// Bind the shared reader, so it losing NeedsInit fails to compile rather
// than sending onboarding down the directory branch — which reads a path
// the shared identity does not live in. home.Dir is deliberately not bound:
// it is the reader the directory branch is for.
var _ initReporter = home.EditableReader{}
