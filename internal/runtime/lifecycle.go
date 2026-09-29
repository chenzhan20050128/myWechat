package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LifecycleTask is one named batch job (SPEC-10 §4.4).
type LifecycleTask struct {
	Name string
	Run  func(ctx context.Context) (int, error)
}

// Lifecycle runs registered tasks on a fixed schedule.
type Lifecycle struct {
	tasks []LifecycleTask
}

func NewLifecycle(tasks ...LifecycleTask) *Lifecycle {
	return &Lifecycle{tasks: tasks}
}

// RunOnce invokes every task once and returns any error.
func (l *Lifecycle) RunOnce(ctx context.Context) []error {
	var errs []error
	for _, t := range l.tasks {
		if _, err := t.Run(ctx); err != nil {
			errs = append(errs, fmt.Errorf("lifecycle %s: %w", t.Name, err))
		}
	}
	return errs
}

// NowUTC exposes a time helper for tasks that need a clock.
func NowUTC() time.Time { return time.Now().UTC() }

// Ensure sql import is used when lifecycle is compiled without other helpers.
var _ = sql.DB{}
