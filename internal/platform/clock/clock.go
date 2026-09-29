// Package clock abstracts time so domain logic can be tested with fake clocks.
package clock

import "time"

// Clock returns the current UTC time (ADR-005: UTC everywhere).
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// System is the production clock.
var System Clock = systemClock{}

// Fake is a manually-controlled clock for tests.
type Fake struct{ T time.Time }

// NewFake builds a Fake anchored at t (normalized to UTC).
func NewFake(t time.Time) *Fake { return &Fake{T: t.UTC()} }

func (f *Fake) Now() time.Time { return f.T }

// Advance moves the fake clock forward.
func (f *Fake) Advance(d time.Duration) { f.T = f.T.Add(d) }
