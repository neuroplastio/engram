//go:build !unix

package enlaunch

import (
	"errors"
	"os"
	"os/exec"
)

// run starts the program as a child, where a process cannot replace
// itself, and exits with its status.
func run(path string, args, env []string) error {
	cmd := exec.Command(path, args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdin, os.Stdout, os.Stderr, env
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err == nil {
		os.Exit(0)
	}
	return err
}

// lock is a no-op: installs into one home are not serialized here.
func lock(dir string) (func(), error) { return func() {}, nil }
