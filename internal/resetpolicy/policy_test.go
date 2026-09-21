package resetpolicy

import (
	"math"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

func weekly(id string, left float64, d time.Duration) Account {
	return Account{ID: id, Eligible: true, Weekly: Window{Known: true, Remaining: left, Running: true, ResetAt: epoch.Add(d), Period: 168 * time.Hour}}
}

func withShort(a Account, left float64, reset time.Duration, ratio float64) Account {
	a.HasShort = true
	a.Short = Window{Known: true, Remaining: left, Running: true, ResetAt: epoch.Add(reset), Period: 5 * time.Hour}
	a.LongToShort = ratio
	return a
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %.12f want %.12f", got, want)
	}
}

func TestFutureRefillsStrictBoundary(t *testing.T) {
	w := withShort(weekly("A", 1, 168*time.Hour), 1, 5*time.Hour, 10).Short
	cases := []struct {
		deadline time.Duration
		delay time.Duration
		want int64
	}{
		{4*time.Hour, 0, 0},
		{5*time.Hour, 0, 0},
		{5*time.Hour+time.Nanosecond, 0, 1},
		{10*time.Hour, 0, 1},
		{10*time.Hour+time.Nanosecond, 0, 2},
		{10*time.Hour+time.Nanosecond, time.Second, 1},
	}
	for _, tc := range cases {
		if got := FutureRefills(w, epoch.Add(tc.deadline), epoch, tc.delay); got != tc.want {
			t.Fatalf("deadline=%s delay=%s got=%d want=%d", tc.deadline, tc.delay, got, tc.want)
		}
	}
}

func TestCouplingAndGlobalDeadline(t *testing.T) {
	a := withShort(weekly("A", .63, 168*time.Hour), .4, 5*time.Hour, 10)
	k := Keys([]Account{a}, Config{GlobalResetAt: epoch.Add(26*time.Hour)}, epoch)[0]
	if k.FutureBuckets != 5 || !k.CouplingKnown {
		t.Fatal(k)
	}
	near(t, k.Reachable, .54)
	near(t, k.Unreachable, .09)
	near(t, k.MustUseCurrent, .04)
}

func TestKnownGlobalResetChangesPressure(t *testing.T) {
	a := withShort(weekly("A", 1, 168*time.Hour), .8, 5*time.Hour, 2)
	b := weekly("B", .2, 24*time.Hour)
	keys := Keys([]Account{a,b}, Config{GlobalResetAt: epoch.Add(2*time.Hour)}, epoch)
	if Compare(keys[0], keys[1]) >= 0 {
		t.Fatalf("expected A before B: %+v", keys)
	}
}

func TestCalibrationUsesOneScale(t *testing.T) {
	a, b := weekly("A", .5, 24*time.Hour), weekly("B", .5, 24*time.Hour)
	a.CapacityWeight, b.CapacityWeight = 1, 5
	keys := Keys([]Account{a,b}, Config{CalibratedCapacity:true}, epoch)
	if !keys[0].Calibrated || Compare(keys[1], keys[0]) >= 0 {
		t.Fatal(keys)
	}
	near(t, keys[1].BurnRate, 5*keys[0].BurnRate)

	b.CapacityWeight = 0
	keys = Keys([]Account{a,b}, Config{CalibratedCapacity:true}, epoch)
	if keys[0].Calibrated || keys[1].Calibrated {
		t.Fatal("mixed scales allowed")
	}
}

func TestEarlierWeeklyDeadlineCanBeatShortConstraint(t *testing.T) {
	a := withShort(weekly("A", .5, 48*time.Hour), .5, 5*time.Hour, 10)
	b := weekly("B", .2, time.Hour)
	keys := Keys([]Account{a,b}, Config{}, epoch)
	if Compare(keys[1], keys[0]) >= 0 {
		t.Fatalf("earlier weekly deadline should win: %+v", keys)
	}
}

func TestPastGlobalResetDoesNotInventAvailability(t *testing.T) {
	a := withShort(weekly("A", .8, 168*time.Hour), 0, 5*time.Hour, 10)
	now := epoch.Add(6*time.Hour)
	k := Keys([]Account{a}, Config{GlobalResetAt: epoch.Add(time.Hour)}, now)[0]
	if k.Usable {
		t.Fatal(k)
	}
}
