package spool

import (
	"os"
	"testing"
	"time"
)

func TestRemoteAliveTTL(t *testing.T) {
	if fresh := (Record{Remote: true, UpdatedAt: time.Now()}); !fresh.Alive() {
		t.Fatal("fresh remote record should be alive")
	}
	if stale := (Record{Remote: true, UpdatedAt: time.Now().Add(-2 * RemoteTTL)}); stale.Alive() {
		t.Fatal("stale remote record should be dead")
	}
	// Remote liveness never consults a pid (a remote pid is meaningless here).
	if noPid := (Record{Remote: true, PID: 0, UpdatedAt: time.Now()}); !noPid.Alive() {
		t.Fatal("remote liveness must not depend on pid")
	}
}

func TestLocalAliveUsesPID(t *testing.T) {
	if self := (Record{PID: os.Getpid()}); !self.Alive() {
		t.Fatal("record for the current process should be alive")
	}
	if dead := (Record{PID: 1 << 30}); dead.Alive() {
		t.Fatal("record for a nonexistent pid should be dead")
	}
}
