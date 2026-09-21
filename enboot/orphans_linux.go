package enboot

import "syscall"

const prSetChildSubreaper = 36

// becomeSubreaper makes processes orphaned by a tenant's exit our children.
func becomeSubreaper() bool {
	_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0)
	return e == 0
}
