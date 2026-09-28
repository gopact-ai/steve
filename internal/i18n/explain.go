package i18n

import "errors"

// Sayer is an error a package makes without knowing who will read it. Its
// Error is English, for logs and for machines where nobody's language is
// known; Say tells a reader in the reader's language. Packages below the
// edges return one of these, and only the edge that answers a person
// renders it, with that person's catalog.
type Sayer interface {
	error
	Say(Catalog) string
}

// Explain is err as c's reader should read it: what the first Sayer in
// err's chain says in c's language, or err's own text when there is none.
// What the errors wrapping that Sayer add to its text is not said. err must
// not be nil.
func (c Catalog) Explain(err error) string {
	var sayer Sayer
	if errors.As(err, &sayer) {
		return sayer.Say(c)
	}
	return err.Error()
}

// Explained is err worded as Explain says it, still wrapping err, so
// errors.Is and errors.As find what its chain holds. Explaining it again
// says the Sayer it wraps afresh, in the new catalog's language.
func (c Catalog) Explained(err error) error {
	return explained{text: c.Explain(err), err: err}
}

type explained struct {
	text string
	err  error
}

func (e explained) Error() string { return e.text }
func (e explained) Unwrap() error { return e.err }
