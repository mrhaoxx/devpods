// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// A queue job's rank its submitter attaches to (SUPERVISOR_ATTACH, set
// with the job's attach) starts its command only once the submitter is
// there, so that no byte of what it writes is out before anyone reads it
// (srun's way):
//
//   - stdin: the attacher's first byte on the container's stdin (a
//     sentinel) opens the gate; the command's stdin is the rest.
//   - tty: the container's terminal getting a size (the attacher's first
//     resize) opens the gate; the command then runs in a session of its
//     own on a terminal of its own, which the supervisor relays to the
//     container's (raw: Ctrl-C, job control and the size are the
//     command's terminal's to handle, as over SSH).
//
// Nobody there within the wait: a stdin job starts, its stdin empty; a
// terminal job fails (nobody is at its terminal).

// attachWait: how long a rank waits for its attacher (SUPERVISOR_ATTACH_WAIT
// overrides, as a Go duration).
func attachWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("SUPERVISOR_ATTACH_WAIT")); err == nil && d > 0 {
		return d
	}
	return 2 * time.Minute
}

// gateStdin reads the attacher's sentinel byte from in, within wait; false
// when none came (in ended, or the wait ran out).
func gateStdin(in *os.File, wait time.Duration) bool {
	got := make(chan bool, 1)
	go func() {
		b := make([]byte, 1)
		n, _ := in.Read(b)
		got <- n == 1
	}()
	select {
	case ok := <-got:
		return ok
	case <-time.After(wait):
		return false
	}
}

// gateTTY waits, within wait, for f's terminal to have a size.
func gateTTY(f *os.File, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if ws, err := winsize(f); err == nil && ws.Col > 0 && ws.Row > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// winsize, setWinsize: f's terminal's size — through its raw connection,
// so f keeps its mode (Fd would make it blocking, and a read of it then
// outlives its Close).
func winsize(f *os.File) (*unix.Winsize, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var ws *unix.Winsize
	var ierr error
	if err := rc.Control(func(fd uintptr) { ws, ierr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ) }); err != nil {
		return nil, err
	}
	return ws, ierr
}

func setWinsize(f *os.File, ws *unix.Winsize) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, ws) }); err != nil {
		return err
	}
	return ierr
}

// openPTY opens a new pseudo-terminal: its master and its slave.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	rc, err := master.SyscallConn()
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	var n uint32
	var ierr error
	err = rc.Control(func(fd uintptr) {
		if ierr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ierr == nil { // unlock
			n, ierr = unix.IoctlGetUint32(int(fd), unix.TIOCGPTN)
		}
	})
	if err == nil {
		err = ierr
	}
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// ptyRelay relays between the container's terminal outer (read from in,
// written to out) and the command's terminal master: bytes both ways, and
// outer's size onto master now and on every SIGWINCH. The returned stop
// waits (a little) for what the command wrote to be out, then ends the
// relay; call it once the command has exited.
func ptyRelay(outer *os.File, in io.Reader, out io.Writer, master *os.File) (stop func()) {
	resize := func() {
		if ws, err := winsize(outer); err == nil && ws.Col > 0 {
			_ = setWinsize(master, ws)
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-winch:
				resize()
			case <-quit:
				return
			}
		}
	}()
	go func() { _, _ = io.Copy(master, in) }()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(out, master); close(drained) }()
	return func() {
		select {
		case <-drained: // the command's terminal closed: all of it is out
		case <-time.After(2 * time.Second):
		}
		signal.Stop(winch)
		close(quit)
	}
}

// attachedCommand prepares the user command uc for an attached rank (mode
// "stdin" or "tty"): it waits at the gate and, for a terminal, puts uc on
// a terminal of its own, in a session of its own. It returns what to do
// once uc has started (the relay begins) and once it has exited (what it
// wrote is out); an error when the rank cannot run (a terminal job nobody
// attached to).
func attachedCommand(mode string, uc *exec.Cmd) (started, exited func(), err error) {
	noop := func() {}
	switch mode {
	case "stdin":
		if !gateStdin(os.Stdin, attachWait()) {
			infof("supervisor: nobody attached within %s: the command starts with no stdin", attachWait())
			if devnull, err := os.Open(os.DevNull); err == nil {
				uc.Stdin = devnull
			}
		}
		return noop, noop, nil
	case "tty":
		if !gateTTY(os.Stdin, attachWait()) {
			return nil, nil, fmt.Errorf("nobody attached to the terminal within %s", attachWait())
		}
		master, slave, err := openPTY()
		if err != nil {
			return nil, nil, fmt.Errorf("a terminal for the command: %w", err)
		}
		// the container's terminal raw: the command's own terminal does the
		// rest (echo, Ctrl-C, line editing); the container ends with the
		// command, so nothing is restored
		_, _ = term.MakeRaw(int(os.Stdin.Fd()))
		uc.Stdin, uc.Stdout, uc.Stderr = slave, slave, slave
		uc.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		var stop func()
		return func() {
				_ = slave.Close() // the command holds it now: its end closes master
				stop = ptyRelay(os.Stdin, os.Stdin, os.Stdout, master)
			}, func() {
				if stop != nil {
					stop()
				}
			}, nil
	}
	return noop, noop, nil
}

// infoOut: where the supervisor says what it does (not errors): stderr, or
// in a job a log file — a job's output is its command's alone.
var infoOut io.Writer = os.Stderr

func infof(format string, args ...any) {
	fmt.Fprintf(infoOut, format+"\n", args...)
}
