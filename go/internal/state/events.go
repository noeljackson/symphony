package state

// PushRecentEvent appends `entry` to `buf` and trims to RecentEventsCap by
// dropping the oldest entry when full (SPEC §13.7.2).
func PushRecentEvent(buf []RecentEvent, entry RecentEvent) []RecentEvent {
	if len(buf) == RecentEventsCap {
		buf = buf[1:]
	}
	return append(buf, entry)
}
