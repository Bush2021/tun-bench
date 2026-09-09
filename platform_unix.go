//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

func interruptProcess(_ context.Context, child *exec.Cmd) error {
	return child.Process.Signal(syscall.SIGTERM)
}

func expectedProcessExit(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return err == nil
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGTERM
}

func execInterrupt(_ string) error {
	return E.New("internal-interrupt is only used on Windows")
}

func runCommand(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := commandOutput(ctx, name, args...)
	return err
}

func parseRoutePrefix(value string) (netip.Prefix, error) {
	if value == "default" {
		return netip.Prefix{}, nil
	}
	host, bits, hasBits := strings.Cut(value, "/")
	host, _, _ = strings.Cut(host, "%")
	if !strings.Contains(host, ":") {
		parts := strings.Split(host, ".")
		if len(parts) < 4 && !hasBits {
			bits, hasBits = strconv.Itoa(len(parts)*8), true
		}
		for len(parts) < 4 {
			parts = append(parts, "0")
		}
		host = strings.Join(parts, ".")
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !hasBits {
		bits = strconv.Itoa(address.BitLen())
	}
	return netip.ParsePrefix(host + "/" + bits)
}

func prepareMemoryLimit() error {
	var limit syscall.Rlimit
	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit)
	if err != nil {
		return err
	}
	required := uint64(memoryMaxConnections)*4 + 256
	if uint64(limit.Max) < required {
		return E.New("memory requires an open-file hard limit of at least ", required, "; available ", limit.Max)
	}
	limit.Cur = max(limit.Cur, required)
	return syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limit)
}

func validateCacheDirectory(_ string, info os.FileInfo) error {
	stat, loaded := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !loaded || stat.Uid != uint32(os.Geteuid()) {
		return E.New("cache must be a private directory owned by the current user")
	}
	return nil
}

var openResult = os.Open

var replaceResult = os.Rename
