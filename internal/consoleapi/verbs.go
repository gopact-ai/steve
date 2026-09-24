package consoleapi

import "context"

// LocalizedVerbs lists what a console can be told, in the language the
// context carries.
type LocalizedVerbs interface {
	VerbsFor(context.Context) []Verb
}
