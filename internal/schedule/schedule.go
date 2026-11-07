// Package schedule is the backup schedule shared by sb-control (which starts backups) and
// sb-guard (whose nightly OS maintenance runs an hour after the first backup of the day).
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Weekly is the Every value of a weekly schedule.
const Weekly = 168

// Backup is when backups run: at Hour (UTC) and then every Every hours (6, 12 or 24), or
// once a week on Weekday at Hour.
type Backup struct {
	Hour    int          `json:"hour"`
	Every   int          `json:"every"`
	Weekday time.Weekday `json:"weekday,omitempty"`
}

// Default is daily at 03:00 UTC.
func Default() Backup { return Backup{Hour: 3, Every: 24} }

// Choices are the frequencies offered in the dashboard.
var Choices = []int{6, 12, 24, Weekly}

// Normalize fills in the default frequency (schedules stored before frequencies existed
// have only an hour).
func (b Backup) Normalize() Backup {
	if b.Every == 0 {
		b.Every = 24
	}
	if b.Every != Weekly {
		b.Weekday = 0
	}
	return b
}

// Validate checks a schedule.
func (b Backup) Validate() error {
	switch {
	case b.Hour < 0 || b.Hour > 23:
		return errors.New("hour must be 0-23 (UTC)")
	case b.Every != 6 && b.Every != 12 && b.Every != 24 && b.Every != Weekly:
		return errors.New("frequency must be every 6, 12 or 24 hours, or weekly")
	case b.Weekday < time.Sunday || b.Weekday > time.Saturday:
		return errors.New("bad weekday")
	}
	return nil
}

// period and offset in hours since the Unix epoch (a Thursday, 00:00 UTC): slots are the
// hours h with h = offset (mod period).
func (b Backup) periodOffset() (int64, int64) {
	b = b.Normalize()
	if b.Every == Weekly {
		days := (int64(b.Weekday) - int64(time.Thursday) + 7) % 7
		return Weekly, days*24 + int64(b.Hour)
	}
	p := int64(b.Every)
	return p, int64(b.Hour) % p
}

// LastSlot is the most recent backup time at or before t.
func (b Backup) LastSlot(t time.Time) time.Time {
	p, o := b.periodOffset()
	h := t.Unix() / 3600 // times before 1970 do not occur
	d := ((h-o)%p + p) % p
	return time.Unix((h-d)*3600, 0).UTC()
}

// NextSlot is the first backup time after t.
func (b Backup) NextSlot(t time.Time) time.Time {
	p, _ := b.periodOffset()
	return b.LastSlot(t).Add(time.Duration(p) * time.Hour)
}

// FirstHourOfDay is the first backup hour of a day (UTC).
func (b Backup) FirstHourOfDay() int {
	b = b.Normalize()
	if b.Every < 24 {
		return b.Hour % b.Every
	}
	return b.Hour
}

// MaintenanceHour is when the nightly OS maintenance starts: an hour after the first
// backup slot of the day.
func (b Backup) MaintenanceHour() int { return (b.FirstHourOfDay() + 1) % 24 }

// Hours lists the backup hours of a day, in order.
func (b Backup) Hours() []int {
	b = b.Normalize()
	if b.Every >= 24 {
		return []int{b.Hour}
	}
	var out []int
	for h := b.FirstHourOfDay(); h < 24; h += b.Every {
		out = append(out, h)
	}
	return out
}

func hh(h int) string { return fmt.Sprintf("%02d:00", h) }

// String describes the schedule for people.
func (b Backup) String() string {
	b = b.Normalize()
	switch b.Every {
	case Weekly:
		return "weekly on " + b.Weekday.String() + " at " + hh(b.Hour) + " UTC"
	case 24:
		return "daily at " + hh(b.Hour) + " UTC"
	}
	var hs []string
	for _, h := range b.Hours() {
		hs = append(hs, hh(h))
	}
	return fmt.Sprintf("every %d hours (%s UTC)", b.Every, strings.Join(hs, ", "))
}

// FrequencyLabel names a frequency for the dashboard.
func FrequencyLabel(every int) string {
	switch every {
	case Weekly:
		return "weekly"
	case 24:
		return "daily"
	}
	return fmt.Sprintf("every %d hours", every)
}

// FromStrings parses the stored form (the sb-backup Secret keys hour, every, weekday).
// Missing or bad values fall back to the defaults.
func FromStrings(hour, every, weekday string) Backup {
	b := Default()
	if h, err := strconv.Atoi(strings.TrimSpace(hour)); err == nil && h >= 0 && h < 24 {
		b.Hour = h
	}
	if e, err := strconv.Atoi(strings.TrimSpace(every)); err == nil {
		b.Every = e
	}
	if w, err := strconv.Atoi(strings.TrimSpace(weekday)); err == nil {
		b.Weekday = time.Weekday(w)
	}
	b = b.Normalize()
	if b.Validate() != nil {
		b.Every, b.Weekday = 24, 0
	}
	return b
}

// Strings is the stored form.
func (b Backup) Strings() (hour, every, weekday string) {
	b = b.Normalize()
	return strconv.Itoa(b.Hour), strconv.Itoa(b.Every), strconv.Itoa(int(b.Weekday))
}
