// Package clock provides the current time.
package clock

import "time"

// Now returns the current local time.
func Now() time.Time {
	return time.Now()
}
