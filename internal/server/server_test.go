package server

import (
	"context"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"cmdgate/internal/client"
	"cmdgate/internal/store"
)

// TestServerExclusiveLock 第二个实例不能移除现有 socket，停止后锁可被新实例取得
func TestServerExclusiveLock(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cg-lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	st := &store.Store{Root: dir}
	if err := st.Init(&store.Config{Mode: store.ModeDev}); err != nil {
		t.Fatal(err)
	}
	logger := log.New(io.Discard, "", 0)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, st, logger) }()
		c := client.NewLocal(st.Socket())
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := c.WhoAmI(); err == nil {
				break
			}
			select {
			case err := <-done:
				cancel()
				t.Fatalf("startup: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("server did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		before, err := os.Stat(st.Socket())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		other, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
		err = Run(other, st, logger)
		stop()
		if err == nil || !strings.Contains(err.Error(), "already in use") {
			cancel()
			<-done
			t.Fatalf("duplicate server: %v", err)
		}
		after, err := os.Stat(st.Socket())
		if err != nil || !os.SameFile(before, after) {
			cancel()
			<-done
			t.Fatal("duplicate instance replaced socket")
		}
		if _, err := c.WhoAmI(); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
