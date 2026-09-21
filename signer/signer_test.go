package signer

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/neuroplastio/engram/sshsig"
)

// A key made by ssh-keygen, read by us: what it signs must verify against the
// public half ssh-keygen wrote beside it.
func TestOpenFileReadsAnSSHKeygenKey(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	path := filepath.Join(t.TempDir(), "k")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "a comment", "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	key, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pubLine, _ := os.ReadFile(path + ".pub")
	keys, err := sshsig.ParseAllowedSigners(pubLine)
	if err != nil {
		t.Fatal(err)
	}
	if !keys[0].Equal(key.Public()) {
		t.Fatal("public half does not match the .pub file")
	}
	sig, err := sshsig.Sign(key, "engram", []byte("m"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sshsig.Verify([]ed25519.PublicKey{keys[0]}, "engram", []byte("m"), sig); err != nil {
		t.Fatal(err)
	}

	os.WriteFile(path, []byte("not a key"), 0o600)
	if _, err := OpenFile(path); err == nil {
		t.Error("garbage parsed as a key")
	}
}
