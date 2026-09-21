//go:build unix

package enboot

import (
	"errors"
	"syscall"
)

const maxLine = 65536

// send writes one line (the LF is added here), attaching fd when fd >= 0.
func send(sock int, line string, fd int) error {
	var oob []byte
	if fd >= 0 {
		oob = syscall.UnixRights(fd)
	}
	for {
		err := syscall.Sendmsg(sock, []byte(line+"\n"), oob, nil, 0)
		if err != syscall.EINTR {
			return err
		}
	}
}

// recv reads one line and the descriptor that rode with it (-1 if none). The
// descriptor arrives close-on-exec.
func recv(sock int) (string, int, error) {
	var line []byte
	fd := -1
	buf := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(4))
	for {
		n, oobn, _, _, err := syscall.Recvmsg(sock, buf, oob, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if n == 0 {
			return "", -1, errors.New("enboot: peer closed")
		}
		if oobn > 0 {
			msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
			if err != nil {
				return "", -1, err
			}
			for _, m := range msgs {
				fds, _ := syscall.ParseUnixRights(&m)
				for _, f := range fds {
					syscall.CloseOnExec(f)
					if fd < 0 {
						fd = f
					} else {
						syscall.Close(f) // "at most one"
					}
				}
			}
		}
		line = append(line, buf[:n]...)
		if len(line) > maxLine {
			return "", -1, errors.New("enboot: line too long")
		}
		if line[len(line)-1] == '\n' {
			return string(line[:len(line)-1]), fd, nil
		}
	}
}
