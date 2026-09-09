package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	tun "github.com/sagernet/sing-tun"
)

func init() {
	processTestHelpers["process-test-wintun"] = processTestWintun
}

func processTestWintun() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	tunnel, err := tun.New(tun.Options{Name: os.Args[2], MTU: 1500, EXP_ExternalConfiguration: true})
	if err != nil {
		panic(err)
	}
	fmt.Println("ready", os.Args[2])
	select {
	case <-signals:
	case <-time.After(20 * time.Second):
		panic("Wintun helper did not receive shutdown signal")
	}
	fmt.Println("shutdown signal received")
	err = tunnel.Close()
	if err != nil {
		panic(err)
	}
	err = os.WriteFile(os.Args[3], []byte("closed"), 0o600)
	if err != nil {
		panic(err)
	}
	os.Exit(0)
}

func TestWindowsTunnelShutdown(t *testing.T) {
	name := "tun-bench-test-" + strconv.Itoa(os.Getpid())
	for range 3 {
		marker := filepath.Join(t.TempDir(), "closed")
		child := startTestProcess(t, "process-test-wintun", name, marker)
		_, err := net.InterfaceByName(name)
		if err != nil {
			t.Fatal("Wintun interface was not created: ", err)
		}
		runner := benchmark{environment: environment{interfaceName: name}, tunnels: []*process{child}}
		err = runner.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, err = os.Stat(marker)
		if err != nil {
			t.Fatal("Wintun Close did not finish: ", err)
		}
		_, err = net.InterfaceByName(name)
		if err == nil {
			t.Fatal("Wintun interface survived shutdown")
		}
	}
}
