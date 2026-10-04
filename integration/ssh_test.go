//go:build integration

package integration

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// sshFixture stands up an ssh daemon of the test's own, listening on the
// loopback address with a freshly made key pair, and puts a wrapper named ssh
// ahead of the real one on PATH so that gbsnap reaches it.
//
// Nothing outside the temporary directory is touched: the invoking user's keys,
// authorized_keys and ssh configuration are all left alone, which they could
// not be if the test leant on an already configured ssh to localhost.
func sshFixture(t *testing.T, dir string) {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		if _, statErr := os.Stat("/usr/sbin/sshd"); statErr != nil {
			t.Skip("sshd is not installed")
		}
		sshd = "/usr/sbin/sshd"
	}
	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("ssh is not installed")
	}

	keygen := func(path string) {
		t.Helper()
		out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path).CombinedOutput()
		if err != nil {
			t.Skipf("ssh-keygen: %v\n%s", err, out)
		}
	}
	identity := filepath.Join(dir, "id")
	hostKey := filepath.Join(dir, "hostkey")
	keygen(identity)
	keygen(hostKey)

	authorized := filepath.Join(dir, "authorized_keys")
	pub, err := os.ReadFile(identity + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorized, pub, 0o600); err != nil {
		t.Fatal(err)
	}

	port := freePort(t)
	pidFile := filepath.Join(dir, "sshd.pid")
	config := filepath.Join(dir, "sshd_config")
	// Its log says why it turns a login down, which the client can't: a skip
	// carries it.
	logFile := filepath.Join(dir, "sshd.log")
	settings := fmt.Sprintf(
		"Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nPidFile %s\n"+
			"StrictModes no\nUsePAM no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\n"+
			"PubkeyAuthentication yes\nPermitRootLogin no\nAllowUsers %s\nLogLevel VERBOSE\n",
		port, hostKey, authorized, pidFile, currentUser(t))
	if err := os.WriteFile(config, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	start := exec.Command("sudo", "-n", sshd, "-f", config, "-E", logFile)
	if os.Geteuid() == 0 {
		start = exec.Command(sshd, "-f", config, "-E", logFile)
	}
	if out, err := start.CombinedOutput(); err != nil {
		t.Skipf("could not start an ssh daemon: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		pid, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		exec.Command("sudo", "-n", "kill", string(trimSpace(pid))).Run()
	})

	// The wrapper adds only the connection details; everything else about the
	// command, above all its quoting, is gbsnap's own.
	wrapper := filepath.Join(dir, "bin")
	if err := os.MkdirAll(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nexec %s -p %d -i %s -o IdentitiesOnly=yes "+
		"-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \"$@\"\n",
		realSSH, port, identity)
	if err := os.WriteFile(filepath.Join(wrapper, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", wrapper+string(os.PathListSeparator)+os.Getenv("PATH"))

	waitForPort(t, port)
	if out, err := exec.Command("ssh", "-o", "BatchMode=yes", "127.0.0.1", "true").CombinedOutput(); err != nil {
		log, _ := exec.Command("sudo", "-n", "cat", logFile).CombinedOutput()
		t.Skipf("the test ssh daemon would not accept a connection: %v\n%s\nits log:\n%s", err, out, log)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitForPort polls until the daemon is accepting connections. There is no
// event to wait on: the daemon forks into the background before it listens.
func waitForPort(t *testing.T, port int) {
	t.Helper()
	address := fmt.Sprintf("127.0.0.1:%d", port)
	for attempt := 0; attempt < 100; attempt++ {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Skipf("the test ssh daemon never listened on %s", address)
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
