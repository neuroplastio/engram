package sshsig

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := Key(priv)
	msg := []byte("engram version=1 kind=manifest\n")

	sig, err := Sign(key, "engram", msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]ed25519.PublicKey{key.Public()}, "engram", msg, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify([]ed25519.PublicKey{key.Public()}, "engram", append(msg, '!'), sig); err == nil {
		t.Error("a changed message verified")
	}
	if _, err := Verify([]ed25519.PublicKey{key.Public()}, "git", msg, sig); err == nil {
		t.Error("another namespace verified")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify([]ed25519.PublicKey{other}, "engram", msg, sig); err == nil {
		t.Error("a key that is not allowed verified")
	}
	if _, err := Verify([]ed25519.PublicKey{key.Public()}, "engram", msg, sig[:len(sig)/2]); err == nil {
		t.Error("a truncated signature verified")
	}

	keys, err := ParseAllowedSigners([]byte(AllowedSigners("release@example.org", "engram", key.Public())))
	if err != nil || len(keys) != 1 || !keys[0].Equal(key.Public()) {
		t.Errorf("allowed_signers round trip: %v %v", keys, err)
	}
}

// The point of this format is that anyone can check a release with a tool they
// already have. So the real ssh-keygen is the oracle, in both directions.
func TestAgainstSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	dir := t.TempDir()
	msg := []byte("engram version=1 kind=manifest\nbuild project=acme channel=dev seq=1\n")

	// Ours, checked by ssh-keygen.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	key := Key(priv)
	sig, err := Sign(key, "engram", msg)
	if err != nil {
		t.Fatal(err)
	}
	signers := filepath.Join(dir, "allowed_signers")
	os.WriteFile(signers, []byte(AllowedSigners("release@example.org", "engram", key.Public())), 0o644)
	os.WriteFile(filepath.Join(dir, "m.sig"), sig, 0o644)

	cmd := exec.Command(keygen, "-Y", "verify", "-f", signers, "-I", "release@example.org", "-n", "engram", "-s", filepath.Join(dir, "m.sig"))
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen rejected our signature: %v\n%s", err, out)
	}

	// ssh-keygen's, checked by us.
	keyFile := filepath.Join(dir, "k")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-f", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	msgFile := filepath.Join(dir, "theirs")
	os.WriteFile(msgFile, msg, 0o644)
	if out, err := exec.Command(keygen, "-Y", "sign", "-f", keyFile, "-n", "engram", msgFile).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	theirs, _ := os.ReadFile(msgFile + ".sig")
	pubLine, _ := os.ReadFile(keyFile + ".pub")
	keys, err := ParseAllowedSigners(pubLine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(keys, "engram", msg, theirs); err != nil {
		t.Fatalf("we rejected ssh-keygen's signature: %v", err)
	}
}
