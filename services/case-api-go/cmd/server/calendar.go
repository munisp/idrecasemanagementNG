// calendar.go — jurisdiction-aware day counting (NG phase 4).
//
// Statutes differ in what "day" means: 45 CFR federal IDR counts calendar
// days; several state programs count business days with jurisdiction
// holidays. The manifest declares day_type per clock; this file resolves the
// counting. Holidays are DATA (public.holidays), editable per tenant — the
// calendar is configuration, not code.
package main

import (
	"context"
	"time"
)

// holidaysFor loads the tenant's holiday calendar.
func (s *server) holidaysFor(ctx context.Context, tenant string) map[string]bool {
	rows, err := s.db.Query(ctx, `SELECT day FROM public.holidays WHERE tenant=$1`, tenant)
	if err != nil {
		return map[string]bool{}
	}
	defer rows.Close()
	days := map[string]bool{}
	for rows.Next() {
		var d time.Time
		if rows.Scan(&d) == nil {
			days[d.Format("2006-01-02")] = true
		}
	}
	return days
}

func isBusinessDay(d time.Time, holidays map[string]bool) bool {
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !holidays[d.Format("2006-01-02")]
}

// Day counting itself lives in main.go:businessDaysBetween (single
// implementation, now holiday-aware via isBusinessDay).

// clockDayType returns the manifest's day_type for the clock anchored on
// basis ("" when no manifest/clock — caller uses calendar days, the legacy
// behavior and the NSA convention).
func (s *server) clockDayType(ctx context.Context, tenant, basis string) string {
	m, err := s.manifestFor(ctx, tenant)
	if err != nil || m == nil {
		return ""
	}
	for _, c := range m.Clocks {
		if c.Basis == basis {
			return c.DayType
		}
	}
	return ""
}
