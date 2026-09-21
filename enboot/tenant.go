//go:build unix

package enboot

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Client is the tenant's side of the conversation.
//
// Lockstep is the tenant's duty: one request outstanding, ever. The mutex is
// what keeps a multi-threaded tenant honest about it.
type Client struct {
	mu         sync.Mutex
	sock       int
	rolledBack bool
}

var (
	dialOnce sync.Once
	dialed   *Client
)

// Dial returns the connection to the enboot that started this process, or
// nil if nothing did.
//
// It also makes sure the connection stops here. The descriptor arrives
// inheritable and named in the environment, which is what lets it survive this
// program's own execve; left that way, every shell and child this program
// starts would inherit both and take itself for a tenant. So Dial marks the
// descriptor close-on-exec and removes the variables. Exec puts them back for
// the one exec that should have them.
func Dial() *Client {
	dialOnce.Do(func() {
		fd, err := strconv.Atoi(os.Getenv(envFD))
		rolledBack := os.Getenv(envRollback) == "1"
		os.Unsetenv(envFD)
		os.Unsetenv(envRollback)
		if err != nil {
			return
		}
		// A stale variable must not make us talk to whatever descriptor 3
		// happens to be.
		if t, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || t != syscall.SOCK_STREAM {
			return
		}
		syscall.CloseOnExec(fd)
		dialed = &Client{sock: fd, rolledBack: rolledBack}
	})
	return dialed
}

// Wrap is how a program embeds its own enboot. Call it first in main. In a
// process enboot started, it returns the connection. In any other, the
// process becomes enboot — starting this same executable, with the same
// arguments, as its tenant — and Wrap never returns.
func Wrap() *Client {
	if c := Dial(); c != nil {
		return c
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "enboot:", err)
		os.Exit(127)
	}
	Run(self, os.Args[1:])
	panic("unreachable")
}

// RolledBack reports whether this image was started because the one after it
// died before saying ready.
func (c *Client) RolledBack() bool { return c.rolledBack }

// Exec replaces this program's image with the one at path, keeping the pid, the
// children, and the connection. It returns only on failure, with everything as
// it was.
func (c *Client) Exec(path string, argv []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fcntl(c.sock, syscall.F_SETFD, 0); err != nil {
		return fmt.Errorf("enboot: %w", err)
	}
	env := append(os.Environ(), envFD+"="+strconv.Itoa(c.sock))
	err := syscall.Exec(path, argv, env)
	syscall.CloseOnExec(c.sock)
	return fmt.Errorf("enboot: exec %s: %w", path, err)
}

func fcntl(fd, cmd, arg int) (int, error) {
	r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(cmd), uintptr(arg))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func (c *Client) call(req string, fd int) (string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := send(c.sock, req, fd); err != nil {
		return "", -1, err
	}
	rep, rfd, err := recv(c.sock)
	if err != nil {
		return "", -1, err
	}
	if rep == "ok" {
		return "", rfd, nil
	}
	if arg, ok := strings.CutPrefix(rep, "ok "); ok {
		return arg, rfd, nil
	}
	return "", -1, fmt.Errorf("enboot: %s", rep)
}

// Hello must be the first request of every program image. It returns the
// protocol version both sides will use.
func (c *Client) Hello() (int, error) {
	arg, _, err := c.call("hello "+strconv.Itoa(Protocol), -1)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(arg)
}

// Put deposits a duplicate of fd under name, replacing any previous holder.
func (c *Client) Put(name string, fd int) error { _, _, err := c.call("put "+name, fd); return err }

// Drop closes enboot's copy.
func (c *Client) Drop(name string) error { _, _, err := c.call("drop "+name, -1); return err }

// Ready says the image at path is up and has everything it needs. path becomes
// the rollback target.
func (c *Client) Ready(path string) error { _, _, err := c.call("ready "+path, -1); return err }

// Next says: when I exit, run path in my place instead of exiting with me.
func (c *Client) Next(path string) error { _, _, err := c.call("next "+path, -1); return err }

// Get returns a duplicate of the descriptor held under name. enboot
// keeps its own.
func (c *Client) Get(name string) (int, error) {
	_, fd, err := c.call("get "+name, -1)
	if err == nil && fd < 0 {
		err = errors.New("enboot: ok without a descriptor")
	}
	return fd, err
}

// List returns every held name.
func (c *Client) List() ([]string, error) {
	arg, _, err := c.call("list", -1)
	return strings.Fields(arg), err
}

// Reaped returns pid -> raw wait status for orphans enboot has reaped
// since the last call. Always empty where the platform does not re-parent.
func (c *Client) Reaped() (map[int]syscall.WaitStatus, error) {
	arg, _, err := c.call("reaped", -1)
	out := map[int]syscall.WaitStatus{}
	for _, f := range strings.Fields(arg) {
		p, s, _ := strings.Cut(f, ":")
		pid, _ := strconv.Atoi(p)
		st, _ := strconv.Atoi(s)
		out[pid] = syscall.WaitStatus(st)
	}
	return out, err
}
