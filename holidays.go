package main

import (
	"sync"
	"time"
)

// German public holidays.
//
// Fuel pricing on a public holiday follows the holiday, not the calendar
// weekday: a Thursday holiday does not price like a Thursday. Left alone, those
// days pollute their weekday bucket in both directions — the holiday is scored
// against ordinary Thursdays, and ordinary Thursdays are scored against it.
//
// Only the nine nationwide (bundeseinheitlich) holidays are recognized.
// State-specific ones — Heilige Drei Könige, Fronleichnam, Allerheiligen,
// Reformationstag and the rest — would need each station mapped to its
// Bundesland, which the station data does not carry: Tankerkönig gives a post
// code and a place name, not a state. Treating a state holiday as a normal day
// is the same behavior as before this file existed, so the gap costs nothing
// relative to the previous model.

// easterSunday returns the Gregorian Easter Sunday of the given year using the
// Anonymous Gregorian algorithm (Meeus/Jones/Butcher). It is pure integer
// arithmetic and valid for every year in the Gregorian calendar.
func easterSunday(year int) time.Time {
	a := year % 19
	b := year / 100
	c := year % 100
	d := b / 4
	e := b % 4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i := c / 4
	k := c % 4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := ((h + l - 7*m + 114) % 31) + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

// easterYearDays caches each year's Easter Sunday as a day-of-year number.
// isGermanHoliday runs once per hourly price bucket — hundreds of thousands of
// times across a model build — and the calendar arithmetic is the expensive
// part. sync.Map rather than a plain map because tests run in parallel.
var easterYearDays sync.Map // int year -> int day-of-year

func easterYearDay(year int) int {
	if cached, ok := easterYearDays.Load(year); ok {
		return cached.(int)
	}
	yearDay := easterSunday(year).YearDay()
	easterYearDays.Store(year, yearDay)
	return yearDay
}

// movableFeastOffsets are the nationwide holidays defined relative to Easter
// Sunday. Easter Sunday itself and Pfingstsonntag are omitted: both are
// Sundays, which already form their own weekday bucket.
var movableFeastOffsets = [...]int{
	-2, // Karfreitag
	1,  // Ostermontag
	39, // Christi Himmelfahrt
	50, // Pfingstmontag
}

// isGermanHoliday reports whether the local date of t is a nationwide German
// public holiday. Only the date is considered; the time of day and the
// location's offset are irrelevant because callers already pass local times.
func isGermanHoliday(t time.Time) bool {
	_, month, day := t.Date()
	switch {
	case month == time.January && day == 1: // Neujahr
		return true
	case month == time.May && day == 1: // Tag der Arbeit
		return true
	case month == time.October && day == 3: // Tag der Deutschen Einheit
		return true
	case month == time.December && (day == 25 || day == 26): // Weihnachten
		return true
	}
	// Compared as day-of-year numbers rather than by building four dates.
	// Easter falls between 22 March and 25 April, so every offset here stays
	// inside the same year and the comparison holds in leap years too.
	easter := easterYearDay(t.Year())
	target := t.YearDay()
	for _, offset := range movableFeastOffsets {
		if target == easter+offset {
			return true
		}
	}
	return false
}
