// Package resetpolicy ranks already eligible accounts. It never changes quota,
 // credentials, account availability, or running tasks.
package resetpolicy

import (
	"fmt"
	"math"
	"time"
)

// Window contains observed remaining capacity in [0,1], not percent points.
type Window struct {
	Known     bool
	Remaining float64
	Running   bool
	ResetAt   time.Time
	Period    time.Duration
}

type Account struct {
	ID       string
	Eligible bool
	Weekly   Window
	HasShort bool
	Short    Window

	// LongToShort is long-window maximum / short-window maximum. Zero means unknown.
	LongToShort float64
	// CapacityWeight is an independently calibrated relative long capacity.
	// Do not estimate it by comparing arbitrary task percentage drops.
	CapacityWeight float64
	// Forecast consumption from already-bound work, as a fraction of the
	// account's full long-window capacity per hour.
	AssignedFractionPerHour float64
}

type Config struct {
	GlobalResetAt       time.Time
	DeadlineFloor       time.Duration
	ActivationDelay     time.Duration
	CalibratedCapacity  bool
}

func (c Config) Validate() error {
	if c.DeadlineFloor < 0 || c.ActivationDelay < 0 {
		return fmt.Errorf("durations cannot be negative")
	}
	return nil
}

func finite(x float64) bool      { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func fraction(x float64) bool    { return finite(x) && x >= 0 && x <= 1 }
func nonnegative(x float64) bool { return finite(x) && x >= 0 }

func Usable(a Account) bool {
	if !a.Eligible || !a.Weekly.Known || !fraction(a.Weekly.Remaining) || a.Weekly.Remaining <= 0 {
		return false
	}
	return !a.HasShort || (a.Short.Known && fraction(a.Short.Remaining) && a.Short.Remaining > 0)
}

func earlier(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}

func deadline(a Account, c Config, now time.Time) time.Time {
	var d time.Time
	if a.Weekly.Running && a.Weekly.ResetAt.After(now) {
		d = a.Weekly.ResetAt
	}
	if c.GlobalResetAt.After(now) {
		d = earlier(d, c.GlobalResetAt)
	}
	return d
}

// FutureRefills counts full short buckets becoming usable strictly before d.
// A refill exactly at d cannot preserve credit that expires at d.
func FutureRefills(w Window, d, now time.Time, delay time.Duration) int64 {
	if !w.Running || !w.ResetAt.After(now) || !w.ResetAt.Before(d) || w.Period <= 0 || delay < 0 {
		return 0
	}
	if delay > time.Duration(math.MaxInt64)-w.Period {
		return 0
	}
	step := w.Period + delay
	span := d.Sub(w.ResetAt)
	return 1 + int64((span-time.Nanosecond)/step)
}

type Key struct {
	ServiceDeadline  time.Time
	RequiredRate     float64
	NeedsWork        bool
	Usable           bool
	HasDeadline      bool
	Calibrated       bool
	CouplingKnown    bool
	Deadline         time.Time
	CurrentDeadline  time.Time
	FutureBuckets    int64
	Remaining        float64
	Reachable        float64
	Unreachable      float64
	MustUseCurrent   float64
	CriticalRate     float64
	BurnRate         float64
	ShortConstrained bool
}

// Keys prepares one comparison universe. If calibrated mode is requested but
// any usable candidate lacks a positive weight, the whole set falls back to
// percentage units so incomparable scales are never mixed.
func Keys(accounts []Account, c Config, now time.Time) []Key {
	calibrated := c.CalibratedCapacity
	if c.Validate() != nil {
		c = Config{}
		calibrated = false
	}
	if calibrated {
		for _, a := range accounts {
			if Usable(a) && (!finite(a.CapacityWeight) || a.CapacityWeight <= 0) {
				calibrated = false
				break
			}
		}
	}
	out := make([]Key, len(accounts))
	for i, a := range accounts {
		out[i] = key(a, c, now, calibrated)
	}
	return out
}

func key(a Account, c Config, now time.Time, calibrated bool) Key {
	k := Key{Usable: Usable(a), Calibrated: calibrated, ShortConstrained: a.HasShort}
	if !k.Usable {
		return k
	}

	weight := 1.0
	if calibrated {
		weight = a.CapacityWeight
	}
	k.Remaining = weight * a.Weekly.Remaining
	k.Reachable = k.Remaining
	k.Deadline = deadline(a, c, now)
	k.HasDeadline = !k.Deadline.IsZero()
	if !k.HasDeadline {
		return k
	}

	floor := c.DeadlineFloor
	if floor == 0 {
		floor = time.Minute
	}
	horizon := k.Deadline.Sub(now)
	if horizon < floor {
		horizon = floor
	}

	assignedRate := a.AssignedFractionPerHour
	if !nonnegative(assignedRate) {
		assignedRate = 0
	}
	assignedRate *= weight

	if a.HasShort && finite(a.LongToShort) && a.LongToShort > 0 &&
		a.Short.Running && a.Short.ResetAt.After(now) && a.Short.Period > 0 {
		shortCapacity := weight / a.LongToShort
		if finite(shortCapacity) && shortCapacity > 0 {
			n := FutureRefills(a.Short, k.Deadline, now, c.ActivationDelay)
			future := float64(n) * shortCapacity
			current := shortCapacity * a.Short.Remaining
			if finite(future) && finite(current) {
				k.CouplingKnown = true
				k.FutureBuckets = n
				k.Reachable = math.Min(k.Remaining, current+future)
				k.Unreachable = math.Max(0, k.Remaining-k.Reachable)
				k.MustUseCurrent = math.Min(current, math.Max(0, k.Remaining-future))
				k.CurrentDeadline = earlier(a.Short.ResetAt, k.Deadline)

				currentHorizon := k.CurrentDeadline.Sub(now)
				if currentHorizon < floor {
					currentHorizon = floor
				}
				k.CriticalRate = math.Max(0, k.MustUseCurrent/currentHorizon.Hours()-assignedRate)
			}
		}
	}

	k.BurnRate = math.Max(0, k.Reachable/horizon.Hours()-assignedRate)
	if !finite(k.BurnRate) {
		k.BurnRate = 0
	}
	if !finite(k.CriticalRate) {
		k.CriticalRate = 0
	}

	k.ServiceDeadline = k.Deadline
	if k.CriticalRate > 0 {
		k.ServiceDeadline = k.CurrentDeadline
	}
	k.RequiredRate = math.Max(k.CriticalRate, k.BurnRate)
	k.NeedsWork = k.RequiredRate > 0
	return k
}

// Compare returns -1 when a should precede b, +1 when b should precede a.
func Compare(a, b Key) int {
	if a.Usable != b.Usable {
		if a.Usable {
			return -1
		}
		return 1
	}
	if !a.Usable {
		return 0
	}
	if a.HasDeadline != b.HasDeadline {
		if a.HasDeadline {
			return -1
		}
		return 1
	}
	if a.NeedsWork != b.NeedsWork {
		if a.NeedsWork {
			return -1
		}
		return 1
	}
	if a.NeedsWork && !a.ServiceDeadline.Equal(b.ServiceDeadline) {
		if a.ServiceDeadline.Before(b.ServiceDeadline) {
			return -1
		}
		return 1
	}
	if a.RequiredRate > b.RequiredRate {
		return -1
	}
	if a.RequiredRate < b.RequiredRate {
		return 1
	}
	if a.ShortConstrained != b.ShortConstrained {
		if a.ShortConstrained {
			return -1
		}
		return 1
	}
	return 0
}
