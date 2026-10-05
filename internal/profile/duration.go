package profile

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts days and weeks ("30d", "2w"),
// which is how expiry thresholds are usually written.
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

// String renders whole days as "Nd" and anything else in Go notation.
func (d Duration) String() string {
	td := time.Duration(d)
	if td != 0 && td%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", td/(24*time.Hour))
	}
	return td.String()
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// ParseDuration parses "30d", "2w", "1d12h" or any time.ParseDuration string.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var total time.Duration
	rest := s
	for _, unit := range []struct {
		suffix string
		size   time.Duration
	}{{"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}} {
		i := strings.Index(rest, unit.suffix)
		if i < 0 {
			continue
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += time.Duration(n) * unit.size
		rest = rest[i+1:]
	}
	if rest != "" {
		v, err := time.ParseDuration(rest)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += v
	}
	return total, nil
}
