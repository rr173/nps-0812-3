package client

import (
	"io/ioutil"
	"net"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestHandleUdpNoPanicOnListenFailure reproduces the bug where handleUdp
// crashed with a nil-pointer dereference when the local UDP port could not be
// listened on.
//
// Root cause: `defer local.Close()` was registered before the error returned by
// net.ListenUDP was checked. When ListenUDP fails it returns a nil
// *net.UDPConn, so the deferred Close() ran on a nil receiver and panicked,
// taking the whole process down instead of logging the error and returning.
//
// This test forces net.ListenUDP to fail (by lowering the file-descriptor
// limit so socket creation fails with EMFILE) and asserts that handleUdp logs
// the error and returns cleanly without panicking.
func TestHandleUdpNoPanicOnListenFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("rlimit-based listen-failure simulation is linux-only (got %s)", runtime.GOOS)
	}

	// Snapshot the original nofile limit so we can restore it.
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &orig); err != nil {
		t.Skipf("getrlimit: %v", err)
	}
	defer syscall.Setrlimit(syscall.RLIMIT_NOFILE, &orig)

	// Create the server connection up front while FDs are still plentiful;
	// net.Pipe is in-memory but we keep the ordering defensive.
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	// Lower the soft nofile limit until net.ListenUDP actually fails. The
	// directory fd used while counting /proc/self/fd is itself transient, so we
	// probe downward with the same call handleUdp uses instead of assuming a
	// fixed offset. We stop at 3 so stdin/stdout/stderr (and the runtime's
	// already-open descriptors) stay usable while new sockets cannot be
	// created.
	open := countOpenFds(t)
	forced := false
	for cur := uint64(open); cur >= 3; cur-- {
		rlim := orig
		rlim.Cur = cur
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
			continue
		}
		if probe, err := net.ListenUDP("udp", nil); err != nil {
			forced = true
			break
		} else {
			probe.Close()
		}
	}
	if !forced {
		t.Skip("could not force net.ListenUDP to fail; skipping")
	}

	s := &TRPClient{}
	done := make(chan struct{})
	var panicked bool
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
			close(done)
		}()
		s.handleUdp(serverConn)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleUdp did not return within 5s")
	}

	if panicked {
		t.Fatal("handleUdp panicked instead of logging the error and returning")
	}
}

// countOpenFds returns the number of currently open file descriptors on Linux.
func countOpenFds(t *testing.T) int {
	entries, err := ioutil.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}
