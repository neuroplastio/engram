//go:build unix

package enlaunch

import (
	"fmt"
	"os"
	"syscall"
)

// run replaces this process with the program: same pid, same terminal.
func run(path string, args, env []string) error {
	return fmt.Errorf("enlaunch: starting %s: %w", path, syscall.Exec(path, args, env))
}

// lock holds a lock on a directory, builds/, until the returned func is
// called: no lock file, so a seed is only builds/ and bin.
func lock(dir string) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("enlaunch: lock: %w", err)
	}
	return func() { f.Close() }, nil
}
