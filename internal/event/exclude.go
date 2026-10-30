package event

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Exclusions are applied by the publisher before anything reaches the bus:
// an excluded conversation or sender is never published (spec, principle 3).
//
// File format (TWALK_EXCLUDE, optional):
//
//	networks: [signal]
//	conversations: ["whatsapp:1203…@g.us", "Famille"]   # id or exact name
//	senders: ["telegram:123456"]                        # sender id
type Exclusions struct {
	Networks      []string `yaml:"networks"`
	Conversations []string `yaml:"conversations"`
	Senders       []string `yaml:"senders"`
}

// LoadExclusions reads the file; a missing path or file means no exclusion.
func LoadExclusions(path string) (*Exclusions, error) {
	ex := &Exclusions{}
	if path == "" {
		return ex, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ex, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(b, ex); err != nil {
		return nil, fmt.Errorf("exclusions %s: %w", path, err)
	}
	return ex, nil
}

// Excluded reports whether an event about this conversation/sender must be dropped.
func (e *Exclusions) Excluded(network string, c Conversation, s Sender) bool {
	if e == nil {
		return false
	}
	for _, n := range e.Networks {
		if strings.EqualFold(n, network) {
			return true
		}
	}
	for _, x := range e.Conversations {
		if x == c.ID || (c.Name != "" && x == c.Name) {
			return true
		}
	}
	for _, x := range e.Senders {
		if x == s.ID {
			return true
		}
	}
	return false
}
