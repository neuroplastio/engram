//go:build unix && !linux

package enboot

// No re-parenting here: orphans go to the system's init, statuses and all.
func becomeSubreaper() bool { return false }
