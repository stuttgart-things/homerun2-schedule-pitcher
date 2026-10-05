// Package status defines the bands a check can be in.
package status

// Band is the state of a check, ordered from good to bad.
type Band int

const (
	// Unknown means the check has not completed yet.
	Unknown Band = iota - 1
	OK
	Warning
	Error
	Critical
)

func (b Band) String() string {
	switch b {
	case OK:
		return "ok"
	case Warning:
		return "warning"
	case Error:
		return "error"
	case Critical:
		return "critical"
	default:
		return "unknown"
	}
}

// Parse is the inverse of String; anything unrecognised is Unknown.
func Parse(s string) Band {
	switch s {
	case "ok":
		return OK
	case "warning":
		return Warning
	case "error":
		return Error
	case "critical":
		return Critical
	default:
		return Unknown
	}
}

// Max returns the worse of two bands.
func Max(a, b Band) Band {
	if a > b {
		return a
	}
	return b
}

// MarshalText makes bands readable in JSON ("warning" instead of 1).
func (b Band) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

func (b *Band) UnmarshalText(text []byte) error {
	*b = Parse(string(text))
	return nil
}
