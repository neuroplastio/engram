//go:build unix

package enboot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The test binary embeds its own enboot, the way a real program does. Run
// with testScenario set, it calls Wrap: the first process becomes the
// enboot, and the copy of itself that it starts plays a program that
// updates.
const (
	testScenario = "ENBOOT_TEST_SCENARIO"
	testResult   = "ENBOOT_TEST_RESULT"
	testStep     = "ENBOOT_TEST_STEP"
	testPID      = "ENBOOT_TEST_PID"
	testChild    = "ENBOOT_TEST_CHILD"
)

func TestMain(m *testing.M) {
	switch {
	case os.Getenv(testChild) != "":
		os.Exit(child())
	case os.Getenv(testScenario) != "":
		os.Exit(tenant(Wrap()))
	}
	os.Exit(m.Run())
}

// child is an ordinary program a tenant starts. It must not be able to tell
// — on descriptor 3 or on any other: it inherits no socket at all.
func child() int {
	_, err := syscall.GetsockoptInt(3, syscall.SOL_SOCKET, syscall.SO_TYPE)
	sockets := 0
	for fd := 4; fd < 256; fd++ {
		if _, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err == nil {
			sockets++
		}
	}
	fmt.Printf("env=%q dial=%v fd3-is-socket=%v other-sockets=%d", os.Getenv(envFD), Dial() != nil, err == nil, sockets)
	return 0
}

func tenant(c *Client) int {
	self, _ := os.Executable()
	result := os.Getenv(testResult)
	fail := func(format string, a ...any) int {
		os.WriteFile(result, []byte("FAIL "+fmt.Sprintf(format, a...)), 0o644)
		return 1
	}
	if v, err := c.Hello(); err != nil || v != Protocol {
		return fail("hello: %d %v", v, err)
	}

	// readBack proves continuity: the bytes were written by an image that no
	// longer exists, into a pipe only enboot kept open.
	readBack := func() (string, error) {
		fd, err := c.Get("pipe")
		if err != nil {
			return "", err
		}
		buf := make([]byte, 5)
		n, err := syscall.Read(fd, buf)
		return string(buf[:n]), err
	}

	switch step := os.Getenv(testStep); {
	case c.RolledBack():
		got, err := readBack()
		if err != nil {
			return fail("after rollback: %v", err)
		}
		os.WriteFile(result, []byte("rolledback:"+got), 0o644)
		c.Ready(self)
		return 0

	case step == "":
		if os.Getenv(testScenario) == "child" {
			cmd := exec.Command(self)
			cmd.Env = append(os.Environ(), testChild+"=1")
			out, err := cmd.Output()
			if err != nil {
				return fail("child: %v", err)
			}
			os.WriteFile(result, out, 0o644)
			return 0
		}

		// The first image: make something worth keeping, deposit it, say
		// ready, and replace ourselves in place.
		var p [2]int
		if err := syscall.Pipe(p[:]); err != nil {
			return fail("pipe: %v", err)
		}
		syscall.Write(p[1], []byte("hello"))
		if err := c.Put("pipe", p[0]); err != nil {
			return fail("put: %v", err)
		}
		if err := c.Ready(self); err != nil {
			return fail("ready: %v", err)
		}
		syscall.Close(p[0]) // what we hold dies with this image; enboot's copy must not
		next := "updated"
		if os.Getenv(testScenario) == "rollback" {
			next = "broken"
		}
		os.Setenv(testStep, next)
		os.Setenv(testPID, fmt.Sprint(os.Getpid()))
		return fail("%v", c.Exec(self, os.Args))

	case step == "updated":
		got, err := readBack()
		if err != nil {
			return fail("after exec: %v", err)
		}
		names, _ := c.List()
		os.WriteFile(result, []byte(fmt.Sprintf("updated:%s:%v:pid-kept=%v", got, names, os.Getenv(testPID) == fmt.Sprint(os.Getpid()))), 0o644)
		c.Ready(self)
		return 0

	case step == "broken":
		return 3 // a new build that dies before it says ready
	}
	return fail("unknown step")
}

func run(t *testing.T, scenario string) (result string, exit int) {
	t.Helper()
	self, _ := os.Executable()
	resultFile := filepath.Join(t.TempDir(), "result")

	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), testScenario+"="+scenario, testResult+"="+resultFile)
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(resultFile)
	return string(data), exit
}

// The happy path: the tenant replaces its own image with execve, and the new
// image gets back a descriptor the old one deposited.
func TestExecInPlaceKeepsDescriptors(t *testing.T) {
	got, exit := run(t, "exec")
	if want := "updated:hello:[pipe]:pid-kept=true"; got != want || exit != 0 {
		t.Errorf("result %q exit %d, want %q exit 0", got, exit, want)
	}
}

// The safety net: the new image dies before saying ready, and enboot
// starts the last good one again, still holding everything.
func TestRollbackWhenTheNewImageDies(t *testing.T) {
	got, exit := run(t, "rollback")
	if want := "rolledback:hello"; got != want || exit != 0 {
		t.Errorf("result %q exit %d, want %q exit 0", got, exit, want)
	}
}

// A tenant runs other programs — for a multiplexer, that is the whole job. None
// of them may inherit the connection, or the belief that they are a tenant.
func TestChildrenDoNotInheritTheConnection(t *testing.T) {
	got, exit := run(t, "child")
	if strings.HasPrefix(got, "FAIL") || exit != 0 {
		t.Fatalf("result %q exit %d", got, exit)
	}
	if want := `env="" dial=false fd3-is-socket=false other-sockets=0`; got != want {
		t.Errorf("a child saw %s, want %s", got, want)
	}
}

// A stale variable must not make a program talk to whatever descriptor 3 is.
func TestDialIgnoresAStaleVariable(t *testing.T) {
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), testChild+"=1", envFD+"=3")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "dial=false") {
		t.Errorf("dialed a descriptor that is not enboot: %s", out)
	}
}
