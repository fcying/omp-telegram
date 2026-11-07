package bridge

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Keep reset data unformatted so cached samples count down when rendered.
type quotaWindow struct {
	present        bool
	used           float64
	resetAt        time.Time
	approximate    bool
	resetPrecision time.Duration
}

type quotaSample struct {
	label   string
	windows [2]quotaWindow
}

func (q quotaSample) available() bool {
	return q.windows[0].present || q.windows[1].present
}

func (q quotaSample) text(now time.Time) string {
	var result strings.Builder
	q.appendTo(&result, now)
	return result.String()
}

func (q quotaSample) appendTo(result *strings.Builder, now time.Time) {
	if !q.available() {
		return
	}
	if q.label != "" {
		result.WriteString(q.label)
		result.WriteByte(' ')
	}
	both := q.windows[0].present && q.windows[1].present
	for i := range q.windows {
		window := &q.windows[i]
		if !window.present {
			continue
		}
		if i == 1 && q.windows[0].present {
			result.WriteByte(' ')
		}
		if !both {
			if i == 0 {
				result.WriteString("5h ")
			} else {
				result.WriteString("7d ")
			}
		}
		percent := strconv.FormatFloat(window.used, 'f', 2, 64)
		result.WriteString(strings.TrimRight(strings.TrimRight(percent, "0"), "."))
		result.WriteByte('%')
		if window.resetAt.After(now) {
			result.WriteByte('(')
			if window.approximate {
				result.WriteByte('~')
			}
			left := window.resetAt.Sub(now)
			if window.resetPrecision > 0 {
				units := math.Round(float64(left) / float64(window.resetPrecision))
				if units < 1 {
					units = 1
				}
				result.WriteString(strconv.FormatFloat(units, 'f', 0, 64))
				switch window.resetPrecision {
				case 24 * time.Hour:
					result.WriteByte('d')
				case time.Hour:
					result.WriteByte('h')
				case time.Minute:
					result.WriteByte('m')
				case time.Second:
					result.WriteByte('s')
				}
			} else {
				days := int64(left / (24 * time.Hour))
				hours := int64(left % (24 * time.Hour) / time.Hour)
				if days > 0 {
					result.WriteString(strconv.FormatInt(days, 10))
					result.WriteByte('d')
				}
				if hours > 0 {
					result.WriteString(strconv.FormatInt(hours, 10))
					result.WriteByte('h')
				} else if days == 0 {
					result.WriteString("<1h")
				}
			}
			result.WriteByte(')')
		}
	}
}
