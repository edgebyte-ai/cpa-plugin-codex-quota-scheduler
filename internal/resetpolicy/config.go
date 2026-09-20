package resetpolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type Calibration struct {
	CapacityWeight          float64 `json:"capacity_weight"`
	LongToShort             float64 `json:"long_to_short_ratio"`
	AssignedFractionPerHour float64 `json:"assigned_long_fraction_per_hour"`
}

type FileOptions struct {
	Enabled                bool                   `json:"enabled"`
	GlobalResetAt          time.Time              `json:"global_reset_at"`
	DeadlineFloorSeconds   int64                  `json:"deadline_floor_seconds"`
	ActivationDelaySeconds int64                  `json:"activation_delay_seconds"`
	CalibratedCapacity     bool                   `json:"calibrated_capacity"`
	Accounts               map[string]Calibration `json:"accounts"`
}

func DecodeOptions(r io.Reader) (FileOptions, error) {
	data, err := io.ReadAll(io.LimitReader(r, 65537))
	if err != nil {
		return FileOptions{}, err
	}
	if len(data) > 65536 {
		return FileOptions{}, fmt.Errorf("policy config exceeds 64 KiB")
	}

	var v FileOptions
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return FileOptions{}, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return FileOptions{}, fmt.Errorf("unexpected trailing JSON")
	}
	if v.DeadlineFloorSeconds < 0 || v.DeadlineFloorSeconds > 86400 ||
		v.ActivationDelaySeconds < 0 || v.ActivationDelaySeconds > 86400 {
		return FileOptions{}, fmt.Errorf("duration must be 0..86400 seconds")
	}
	if v.DeadlineFloorSeconds == 0 {
		v.DeadlineFloorSeconds = 60
	}
	for id, c := range v.Accounts {
		if id == "" || !nonnegative(c.CapacityWeight) || !nonnegative(c.LongToShort) || !nonnegative(c.AssignedFractionPerHour) {
			return FileOptions{}, fmt.Errorf("invalid calibration")
		}
		if v.CalibratedCapacity && c.CapacityWeight <= 0 {
			return FileOptions{}, fmt.Errorf("calibrated mode requires positive weights for configured accounts")
		}
	}
	return v, nil
}

func (o FileOptions) PolicyConfig() Config {
	return Config{
		GlobalResetAt:      o.GlobalResetAt,
		DeadlineFloor:      time.Duration(o.DeadlineFloorSeconds) * time.Second,
		ActivationDelay:    time.Duration(o.ActivationDelaySeconds) * time.Second,
		CalibratedCapacity: o.CalibratedCapacity,
	}
}

// FileLoader reads at most once per interval. Missing/invalid reload disables
// the addon; upstream selection remains authoritative.
type FileLoader struct {
	mu      sync.Mutex
	last    time.Time
	path    string
	options FileOptions
	err     error
}

func (l *FileLoader) Load(path string, now time.Time) (FileOptions, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if path == "" {
		l.path = ""
		l.options = FileOptions{}
		l.err = nil
		return l.options, nil
	}
	if path == l.path && !l.last.IsZero() && !now.Before(l.last) && now.Sub(l.last) < 5*time.Second {
		return l.options, l.err
	}

	l.path = path
	l.last = now
	l.options = FileOptions{}

	f, err := os.Open(path)
	if err != nil {
		l.err = err
		return l.options, err
	}
	defer f.Close()

	l.options, l.err = DecodeOptions(f)
	if l.err != nil {
		l.options = FileOptions{}
	}
	return l.options, l.err
}
