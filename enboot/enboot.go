//go:build unix

// Package enboot lets a program replace itself without losing what it is
// running.
//
// One process stays resident — enboot — and holds duplicates of whatever
// descriptors the program deposits with it: pty masters, listening sockets,
// socket pairs to children. The program — the tenant — updates by replacing its
// own image with execve, which keeps its pid and its children, and asks for its
// descriptors back. If the new image dies before saying it is ready, the
// enboot starts the last good one again, still holding everything.
//
// A program embeds both halves and needs no second binary:
//
//	func main() {
//		b := enboot.Wrap() // the first process becomes enboot and never returns
//		b.Hello()
//		…
//		b.Put("pty-3", fd) // when a descriptor is created, not at update time
//		b.Ready(self)      // "I am up": this path is now the rollback target
//		…
//		b.Exec(newPath, os.Args) // update
//	}
//
// PROTOCOL.md is the whole protocol between the two. The resident half is
// meant to change almost never — a program can only update itself freely if
// the thing underneath it does not need updating too — so it has no download,
// no verification and no version policy: it is handed a path, it does not
// choose one. PROTOCOL.md ends with the complete list of reasons it could
// change; anything else belongs in the tenant.
//
// Standard library only, and it stays that way.
package enboot

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Protocol is the highest PROTOCOL.md version this package speaks. A release's
// min_enboot is compared against the version the resident process answers
// hello with.
const Protocol = 1

// The environment a tenant is started with. A tenant reads and removes them
// (see Dial), so that the programs it runs do not mistake themselves for one.
const (
	envFD       = "ENBOOT_FD"
	envRollback = "ENBOOT_ROLLBACK"
	envLog      = "ENBOOT_LOG"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type resident struct {
	args []string

	mu         sync.Mutex
	held       map[string]int
	reaped     []string
	tenant     int
	ready      bool   // the running image has said ready
	rollback   string // last path that said ready
	next       string
	rolledBack bool
}

// logf appends to the file named by ENBOOT_LOG, if any. The resident
// process shares its standard streams with the tenant and must not write to
// them.
func logf(format string, a ...any) {
	p := os.Getenv(envLog)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%d enboot %d %s\n", time.Now().UnixNano(), os.Getpid(), fmt.Sprintf(format, a...))
	f.Close()
}

// Run makes this process the enboot of program, started with args, and
// never returns: it exits when the tenant does, with the tenant's status.
func Run(program string, args []string) {
	path, err := filepath.Abs(program)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enboot:", err)
		os.Exit(127)
	}
	logf("start subreaper=%v", becomeSubreaper())
	r := &resident{args: args, held: map[string]int{}}
	r.start(path, false)
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "enboot: wait:", err)
			os.Exit(127)
		}
		r.mu.Lock()
		if pid != r.tenant {
			r.reaped = append(r.reaped, fmt.Sprintf("%d:%d", pid, uint32(ws)))
			logf("reaped orphan pid=%d status=%d", pid, uint32(ws))
			r.mu.Unlock()
			continue
		}
		logf("tenant %d exited status=%d ready=%v next=%q", pid, uint32(ws), r.ready, r.next)
		switch {
		case r.next != "":
			p := r.next
			r.next = ""
			r.mu.Unlock()
			r.start(p, false)
		case !r.ready && r.rollback != "" && !r.rolledBack:
			r.rolledBack = true
			p := r.rollback
			r.mu.Unlock()
			logf("ROLLBACK to %s holding %d descriptors", p, len(r.held))
			r.start(p, true)
		default:
			if ws.Signaled() {
				os.Exit(128 + int(ws.Signal()))
			}
			os.Exit(ws.ExitStatus())
		}
	}
}

func (r *resident) start(path string, rollback bool) {
	sp, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "enboot: socketpair:", err)
		os.Exit(127)
	}
	syscall.CloseOnExec(sp[0])

	// Our own environment, minus anything an enboot above us may have left.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envFD+"=") && !strings.HasPrefix(kv, envRollback+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, envFD+"=3")
	if rollback {
		env = append(env, envRollback+"=1")
	}
	pid, err := syscall.ForkExec(path, append([]string{path}, r.args...), &syscall.ProcAttr{
		Env:   env,
		Files: []uintptr{0, 1, 2, uintptr(sp[1])},
	})
	syscall.Close(sp[1])
	if err != nil {
		logf("start %s: %v", path, err)
		fmt.Fprintf(os.Stderr, "enboot: start %s: %v\n", path, err)
		os.Exit(127)
	}
	r.mu.Lock()
	r.tenant, r.ready = pid, false
	r.mu.Unlock()
	logf("started tenant pid=%d path=%s", pid, path)
	go r.serve(sp[0])
}

func (r *resident) serve(sock int) {
	defer syscall.Close(sock)
	for {
		line, fd, err := recv(sock)
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		reply, rfd := r.handle(verb, arg, fd)
		if err := send(sock, reply, rfd); err != nil {
			return
		}
	}
}

func (r *resident) handle(verb, arg string, fd int) (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fd >= 0 && verb != "put" {
		syscall.Close(fd)
	}
	switch verb {
	case "hello":
		r.ready = false
		// m = min(n, ours). A tenant that sends nothing sensible gets version 1.
		m := Protocol
		if n, err := strconv.Atoi(arg); err == nil && n >= 1 && n < m {
			m = n
		}
		return fmt.Sprintf("ok %d", m), -1
	case "put":
		if fd < 0 || !nameRE.MatchString(arg) {
			if fd >= 0 {
				syscall.Close(fd)
			}
			return "no usage", -1
		}
		if old, ok := r.held[arg]; ok {
			syscall.Close(old)
		}
		r.held[arg] = fd
		return "ok", -1
	case "get":
		if h, ok := r.held[arg]; ok {
			return "ok", h
		}
		return "no absent", -1
	case "list":
		names := make([]string, 0, len(r.held))
		for n := range r.held {
			names = append(names, n)
		}
		sort.Strings(names)
		return strings.TrimSpace("ok " + strings.Join(names, " ")), -1
	case "drop":
		h, ok := r.held[arg]
		if !ok {
			return "no absent", -1
		}
		syscall.Close(h)
		delete(r.held, arg)
		return "ok", -1
	case "ready":
		r.ready, r.rollback, r.rolledBack = true, arg, false
		return "ok", -1
	case "next":
		r.next = arg
		return "ok", -1
	case "reaped":
		out := strings.TrimSpace("ok " + strings.Join(r.reaped, " "))
		r.reaped = nil
		return out, -1
	}
	return "no verb", -1
}
