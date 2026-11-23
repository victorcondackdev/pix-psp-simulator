// Package bizday is the business-day calendar used for regulatory deadlines.
//
// Dates are calendar days in Brasília time (UTC-03:00, no daylight saving
// since 2019), represented as time.Time at midnight in that zone. The default
// calendar treats as non-business days the weekends, the national holidays
// (including Good Friday and, since 2024, 20 November) and the days the
// financial system does not operate (Carnival Monday and Tuesday, Corpus
// Christi). Local holidays are business days, as the Documento 4111
// instructions require.
package bizday

import (
	"sort"
	"time"
)

// Brasilia is the fixed UTC-03:00 zone used for every regulatory date.
var Brasilia = time.FixedZone("BRT", -3*60*60)

// Date returns midnight of the calendar day of t in Brasília.
func Date(t time.Time) time.Time {
	y, m, d := t.In(Brasilia).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, Brasilia)
}

// EndOfDay returns the last instant of the day d (exclusive end minus 1ns).
func EndOfDay(d time.Time) time.Time { return Date(d).AddDate(0, 0, 1).Add(-time.Nanosecond) }

// Easter returns Easter Sunday of year y (anonymous Gregorian algorithm).
func Easter(y int) time.Time {
	a := y % 19
	b, c := y/100, y%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(y, time.Month(month), day, 0, 0, 0, 0, Brasilia)
}

// Holiday is a named non-business day.
type Holiday struct {
	Date time.Time `json:"date"`
	Name string    `json:"name"`
}

// Holidays returns the default non-business weekdays and holidays of year y,
// sorted by date.
func Holidays(y int) []Holiday {
	fixed := func(m time.Month, d int, name string) Holiday {
		return Holiday{time.Date(y, m, d, 0, 0, 0, 0, Brasilia), name}
	}
	e := Easter(y)
	hs := []Holiday{
		fixed(time.January, 1, "Confraternização Universal"),
		{e.AddDate(0, 0, -48), "Carnaval (segunda-feira)"},
		{e.AddDate(0, 0, -47), "Carnaval (terça-feira)"},
		{e.AddDate(0, 0, -2), "Paixão de Cristo"},
		fixed(time.April, 21, "Tiradentes"),
		fixed(time.May, 1, "Dia do Trabalho"),
		{e.AddDate(0, 0, 60), "Corpus Christi"},
		fixed(time.September, 7, "Independência do Brasil"),
		fixed(time.October, 12, "Nossa Senhora Aparecida"),
		fixed(time.November, 2, "Finados"),
		fixed(time.November, 15, "Proclamação da República"),
		fixed(time.December, 25, "Natal"),
	}
	if y >= 2024 {
		hs = append(hs, fixed(time.November, 20, "Dia Nacional de Zumbi e da Consciência Negra"))
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].Date.Before(hs[j].Date) })
	return hs
}

// Calendar answers business-day questions. The zero value uses the default
// holidays; Extra adds dates (for example a one-off national holiday).
type Calendar struct {
	Extra map[time.Time]string
}

// Holiday returns the holiday name of d, if any.
func (c Calendar) Holiday(d time.Time) (string, bool) {
	d = Date(d)
	if n, ok := c.Extra[d]; ok {
		return n, true
	}
	for _, h := range Holidays(d.Year()) {
		if h.Date.Equal(d) {
			return h.Name, true
		}
	}
	return "", false
}

// IsBusinessDay reports whether d is a weekday that is not a holiday.
func (c Calendar) IsBusinessDay(d time.Time) bool {
	d = Date(d)
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	_, hol := c.Holiday(d)
	return !hol
}

// Add moves n business days from d (n may be negative). The start day does
// not count, so Add(friday, 1) is the next Monday.
func (c Calendar) Add(d time.Time, n int) time.Time {
	d = Date(d)
	step := 1
	if n < 0 {
		step, n = -1, -n
	}
	for n > 0 {
		d = d.AddDate(0, 0, step)
		if c.IsBusinessDay(d) {
			n--
		}
	}
	return d
}

// Previous returns the last business day strictly before d.
func (c Calendar) Previous(d time.Time) time.Time { return c.Add(d, -1) }

// LastOfMonth returns the last business day of the month containing d.
func (c Calendar) LastOfMonth(d time.Time) time.Time {
	d = Date(d)
	first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, Brasilia)
	return c.Previous(first.AddDate(0, 1, 0))
}
