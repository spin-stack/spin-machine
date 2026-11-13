// SPDX-License-Identifier: Apache-2.0

// vsockproxy is ssh's ProxyCommand for a guest reached over vsock: it connects to a context
// id and port and copies stdin to the connection and the connection to stdout. task
// boot:ssh-first builds it, because the host that runs the probe is not guaranteed a
// systemd-ssh-proxy or a socat that speaks AF_VSOCK.
//
// Diagnostic, never part of a release.
//
//	vsockproxy <cid> <port>
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: vsockproxy <cid> <port>")
		os.Exit(2)
	}
	cid, err1 := strconv.ParseUint(os.Args[1], 10, 32)
	port, err2 := strconv.ParseUint(os.Args[2], 10, 32)
	if err1 != nil || err2 != nil {
		fmt.Fprintf(os.Stderr, "vsockproxy: cid %q and port %q must be numbers\n", os.Args[1], os.Args[2])
		os.Exit(2)
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vsockproxy: a vsock socket: %v\n", err)
		os.Exit(1)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: uint32(cid), Port: uint32(port)}); err != nil {
		fmt.Fprintf(os.Stderr, "vsockproxy: connecting to vsock %d:%d: %v\n", cid, port, err)
		os.Exit(1)
	}
	conn := os.NewFile(uintptr(fd), "vsock")
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		_ = unix.Shutdown(fd, unix.SHUT_WR)
	}()
	_, _ = io.Copy(os.Stdout, conn)
}
