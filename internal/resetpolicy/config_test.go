package resetpolicy

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeOptionsStrict(t *testing.T) {
	good := `{"enabled":true,"global_reset_at":"2026-09-21T10:00:00-07:00","accounts":{"A":{"long_to_short_ratio":10}}}`
	v, err := DecodeOptions(strings.NewReader(good))
	if err != nil || !v.Enabled || v.DeadlineFloorSeconds != 60 {
		t.Fatal(v, err)
	}
	if v.PolicyConfig().GlobalResetAt.IsZero() {
		t.Fatal("missing global reset")
	}

	bad := []string{
		`{"enabld":true}`,
		`{} {}`,
		`{"activation_delay_seconds":-1}`,
		`{"deadline_floor_seconds":90000}`,
		`{"accounts":{"A":{"capacity_weight":-1}}}`,
		`{"calibrated_capacity":true,"accounts":{"A":{}}}`,
		`{"global_reset_at":"tomorrow"}`,
	}
	for _, input := range bad {
		if _, err := DecodeOptions(strings.NewReader(input)); err == nil {
			t.Fatalf("bad config accepted: %s", input)
		}
	}
}

func TestPolicyConfigDurations(t *testing.T) {
	v := FileOptions{DeadlineFloorSeconds: 90, ActivationDelaySeconds: 2}
	c := v.PolicyConfig()
	if c.DeadlineFloor != 90*time.Second || c.ActivationDelay != 2*time.Second {
		t.Fatal(c)
	}
}
