package main

import (
	"fmt"
	"os"

	"go.uber.org/zap"
)

func humanizeDuration(seconds int64) string {
	const (
		year  = 365 * 24 * 60 * 60
		month = 30 * 24 * 60 * 60
		day   = 24 * 60 * 60
		hour  = 60 * 60
		min   = 60
	)

	result := ""

	if seconds >= year {
		y := seconds / year
		seconds %= year
		result += fmt.Sprintf("%dy", y)
	}
	if seconds >= month {
		mo := seconds / month
		seconds %= month
		result += fmt.Sprintf("%dmo", mo)
	}
	if seconds >= day {
		d := seconds / day
		seconds %= day
		result += fmt.Sprintf("%dd", d)
	}
	if seconds >= hour {
		h := seconds / hour
		seconds %= hour
		result += fmt.Sprintf("%dh", h)
	}
	if seconds >= min {
		m := seconds / min
		seconds %= min
		result += fmt.Sprintf("%dm", m)
	}
	if seconds > 0 || result == "" {
		result += fmt.Sprintf("%ds", seconds)
	}

	return result
}

func closeFile(f *os.File) {
	if err := f.Close(); err != nil {
		log.Error("Error closing file", zap.Error(err))
	}
}

func safeDivide(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
