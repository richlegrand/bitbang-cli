//go:build !unix

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

const shareHostSupported = false

func signalShareProcess(pid int, sig syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(sig)
}

func shareWorkerContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}
