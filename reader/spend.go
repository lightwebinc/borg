package reader

import "regexp"

// quoted is a Go-quoted string (%q) in an error's text; status is one that
// names a status of the node's UTXO view, such as "NOT_FOUND".
var (
	quoted = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	status = regexp.MustCompile(`^"[A-Z_]{0,24}"$`)
)

// quietError is a spend source's error with what the node named left out
// of its text. Unwrap keeps the chain, so errors.Is still finds
// nodeapi.ErrSpendUnknown.
type quietError struct {
	msg string
	err error
}

func (e *quietError) Error() string { return e.msg }
func (e *quietError) Unwrap() error { return e.err }

// QuietSpend is err, the answer of a spend source (nodeapi.Asset.Spender),
// with every quoted name in its text that the node chose (a spender that is
// not a transaction id, a status of any length) replaced, so what a node,
// somebody else's server, names is never printed. A short status word such
// as "NOT_FOUND" is kept. nil stays nil.
func QuietSpend(err error) error {
	if err == nil {
		return nil
	}
	msg := quoted.ReplaceAllStringFunc(err.Error(), func(q string) string {
		if status.MatchString(q) {
			return q
		}
		return "(a name the node gave, not repeated)"
	})
	if msg == err.Error() {
		return err
	}
	return &quietError{msg: msg, err: err}
}
