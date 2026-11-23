package bizday

import (
	"testing"
	"time"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, Brasilia) }

func TestEaster(t *testing.T) {
	for y, want := range map[int]time.Time{2024: day(2024, 3, 31), 2025: day(2025, 4, 20), 2026: day(2026, 4, 5), 2027: day(2027, 3, 28)} {
		if got := Easter(y); !got.Equal(want) {
			t.Errorf("%d: %v", y, got)
		}
	}
}

func TestCalendar(t *testing.T) {
	var c Calendar
	cases := map[time.Time]bool{
		day(2026, 11, 20): false, // Consciência Negra (Friday)
		day(2026, 11, 23): true,  // Monday
		day(2026, 11, 21): false, // Saturday
		day(2026, 2, 16):  false, // Carnaval
		day(2026, 2, 17):  false,
		day(2026, 2, 18):  true, // Ash Wednesday
		day(2026, 4, 3):   false,
		day(2026, 6, 4):   false, // Corpus Christi
		day(2023, 11, 20): true,  // before the national holiday existed
		day(2026, 1, 20):  true,  // São Sebastião is a Rio holiday only
	}
	for d, want := range cases {
		if got := c.IsBusinessDay(d); got != want {
			t.Errorf("%s: %v", d.Format("2006-01-02"), got)
		}
	}
	if n, ok := c.Holiday(day(2026, 12, 25)); !ok || n != "Natal" {
		t.Error(n)
	}
	// Thursday 19/11/2026 + 3 business days skips Friday 20 (holiday) and the weekend.
	if got := c.Add(day(2026, 11, 19), 3); !got.Equal(day(2026, 11, 25)) {
		t.Error(got)
	}
	if got := c.Previous(day(2026, 11, 23)); !got.Equal(day(2026, 11, 19)) {
		t.Error(got)
	}
	if got := c.LastOfMonth(day(2026, 10, 5)); !got.Equal(day(2026, 10, 30)) {
		t.Error(got)
	}
	extra := Calendar{Extra: map[time.Time]string{day(2026, 11, 24): "feriado extra"}}
	if extra.IsBusinessDay(day(2026, 11, 24)) {
		t.Error("extra holiday")
	}
	if got := Date(time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)); !got.Equal(day(2026, 11, 23)) {
		t.Error("02:00 UTC is still the previous day in Brasília", got)
	}
	if got := EndOfDay(day(2026, 11, 23)); got.Day() != 23 || got.Hour() != 23 {
		t.Error(got)
	}
}
