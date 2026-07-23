package config

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from YAML strings like "15m".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"15m\"", value.Line)
	}
	if s == "" {
		*d = 0
		return nil
	}
	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}
	// A bare number is read as seconds.
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	return fmt.Errorf("line %d: invalid duration %q: want a value like \"15m\", \"2h30m\" or a number of seconds", value.Line, s)
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}
