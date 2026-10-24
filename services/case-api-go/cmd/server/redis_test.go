// Author: Patrick Munis at newwave
package main

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// fakeRedis starts a minimal RESP server on 127.0.0.1:0. handle is called once
// per accepted connection with the commands sent on it; it writes raw RESP
// replies directly to the connection in the order commands arrive.
func fakeRedis(t *testing.T, handle func(cmds [][]string) []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		var cmds [][]string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			if !strings.HasPrefix(line, "*") {
				continue
			}
			n := 0
			fmt.Sscanf(strings.TrimSpace(line), "*%d", &n)
			var args []string
			for i := 0; i < n; i++ {
				r.ReadString('\n') // $<len>
				val, _ := r.ReadString('\n')
				args = append(args, strings.TrimSuffix(val, "\r\n"))
			}
			cmds = append(cmds, args)
			replies := handle(cmds)
			if len(replies) < len(cmds) {
				break // handler has no more scripted replies; stop
			}
			if _, err := conn.Write([]byte(replies[len(cmds)-1])); err != nil {
				break
			}
		}
	}()
	return ln.Addr().String()
}

func TestSetnxFailOpenOnRedisError(t *testing.T) {
	// No AUTH configured on the client, but the server demands one -- exactly
	// the production incident (shared Redis requires AUTH; this client never
	// sent one). The old code treated this identically to "key exists" and
	// returned false forever; the fix must fail OPEN (return true) instead.
	addr := fakeRedis(t, func(cmds [][]string) []string {
		return []string{"-NOAUTH Authentication required.\r\n"}
	})
	rc := newRedis(addr, "")
	if got := rc.setnx("k", 10, "v"); got != true {
		t.Fatalf("setnx on a redis error must fail OPEN (true), got %v", got)
	}
}

func TestSetnxGenuineConflictStillBlocks(t *testing.T) {
	// A real NX conflict (key already exists) is a nil bulk reply -- this
	// must still return false. The fix must not have over-corrected into
	// "always allow".
	addr := fakeRedis(t, func(cmds [][]string) []string {
		return []string{"$-1\r\n"}
	})
	rc := newRedis(addr, "")
	if got := rc.setnx("k", 10, "v"); got != false {
		t.Fatalf("setnx on a genuine NX conflict must return false, got %v", got)
	}
}

func TestSetnxSucceedsOnRealOK(t *testing.T) {
	addr := fakeRedis(t, func(cmds [][]string) []string {
		return []string{"+OK\r\n"}
	})
	rc := newRedis(addr, "")
	if got := rc.setnx("k", 10, "v"); got != true {
		t.Fatalf("setnx on a real OK reply must return true, got %v", got)
	}
}

func TestCmdSendsAuthBeforeRealCommand(t *testing.T) {
	var sawAuth bool
	addr := fakeRedis(t, func(cmds [][]string) []string {
		if len(cmds) == 1 {
			if len(cmds[0]) == 2 && cmds[0][0] == "AUTH" && cmds[0][1] == "secret" {
				sawAuth = true
			}
			return []string{"+OK\r\n"} // AUTH reply
		}
		return []string{"+OK\r\n", "+OK\r\n"} // real command reply
	})
	rc := newRedis(addr, "secret")
	if _, err := rc.cmd("GET", "k"); err != nil {
		t.Fatalf("cmd: %v", err)
	}
	if !sawAuth {
		t.Fatal("client with a configured password must send AUTH before the real command")
	}
}
