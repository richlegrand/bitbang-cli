//go:build unix

package main

import (
	"context"
	"os/signal"
	"syscall"
)

const shareHostSupported = true

func signalShareProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}

func shareWorkerContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
}
