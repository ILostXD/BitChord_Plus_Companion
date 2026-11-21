//go:build linux

package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestQobuzCancellationStopsChildWriter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker := filepath.Join(t.TempDir(), "orphan-wrote-file")
	command := exec.CommandContext(ctx, "sh", "-c", `(sleep 1; touch "$1") & echo ready; wait`, "sh", marker)
	configureQobuzCancellation(command)
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err = bufio.NewReader(output).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	if command.Wait() == nil {
		t.Fatal("cancelled command succeeded")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("orphan child kept writing after cancellation")
	}
}
