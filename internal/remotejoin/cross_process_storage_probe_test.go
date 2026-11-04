package remotejoin

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func TestProbeStorageLockHolder(t *testing.T) {
	path := os.Getenv("MAGPIE_SYNTHETIC_LOCK_PROBE")
	if path == "" {
		t.Skip("helper only")
	}
	m := filelock.Mutex{Path: func() string { return path }}
	if err := m.LockContext(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	fmt.Println("synthetic-lock-held")
	time.Sleep(time.Minute)
	m.Unlock()
}

func TestProbeCrossProcessStorageContention(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	r.options.Now = time.Now
	r.options.Policy.OfflineWindow = time.Second
	tickRunner(t, r)
	holder := exec.Command(os.Args[0], "-test.run=^TestProbeStorageLockHolder$", "-test.count=1")
	holder.Env = append(os.Environ(), "MAGPIE_SYNTHETIC_LOCK_PROBE="+connectionPath()+".lock")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "synthetic-lock-held" {
		t.Fatalf("lock holder: %q %v", line, err)
	}
	start := time.Now()
	s := tickRunner(t, r)
	elapsed := time.Since(start)
	t.Logf("held-lock first tick duration=%v window=%v state=%s connected=%v restores=%d", elapsed, r.options.Policy.OfflineWindow, s.State, s.Connected, m.restored)
	// After detachment, keep the lock briefly so recovery needs more than
	// an expired window's minimal probe budget.
	go func() { time.Sleep(150 * time.Millisecond); _ = holder.Process.Kill() }()
	recovered := tickRunner(t, r)
	_ = holder.Wait()
	t.Logf("after release state=%s connected=%v restores=%d", recovered.State, recovered.Connected, m.restored)
	if recovered.State != "connected" || !recovered.Connected || m.restored != 1 {
		t.Error("did not recover after lock release")
	}
	if elapsed > r.options.Policy.OfflineWindow+2*time.Second || s.Connected {
		t.Error("blocked storage read did not detach within the bounded window")
	}
}

func TestProbeReleasedLockBeforeDeadlineKeepsWiring(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	r.options.Now = time.Now
	r.options.Policy.OfflineWindow = 2 * time.Second
	tickRunner(t, r)
	holder := exec.Command(os.Args[0], "-test.run=^TestProbeStorageLockHolder$", "-test.count=1")
	holder.Env = append(os.Environ(), "MAGPIE_SYNTHETIC_LOCK_PROBE="+connectionPath()+".lock")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "synthetic-lock-held" {
		t.Fatalf("holder: %q %v", line, err)
	}
	go func() { time.Sleep(150 * time.Millisecond); _ = holder.Process.Kill() }()
	s := tickRunner(t, r)
	_ = holder.Wait()
	if s.State != "connected" || !s.Connected || !m.connected || m.restored != 0 {
		t.Fatal("short lock contention lost wiring")
	}
}

func TestProbeNearDeadlineLockThenNetwork(t *testing.T) {
	for _, healthy := range []bool{false, true} {
		name := "network-unhealthy"
		if healthy {
			name = "network-healthy"
		}
		t.Run(name, func(t *testing.T) {
			r, m, _, health := runnerFixture(t)
			r.options.Now = time.Now
			r.options.Policy.OfflineWindow = 2 * time.Second
			tickRunner(t, r)
			if !healthy {
				*health = HealthResult{Code: "health_unreachable"}
			}
			holder := exec.Command(os.Args[0], "-test.run=^TestProbeStorageLockHolder$", "-test.count=1")
			holder.Env = append(os.Environ(), "MAGPIE_SYNTHETIC_LOCK_PROBE="+connectionPath()+".lock")
			out, err := holder.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := holder.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
			line, err := bufio.NewReader(out).ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "synthetic-lock-held" {
				t.Fatalf("holder: %q %v", line, err)
			}
			go func() { time.Sleep(1500 * time.Millisecond); _ = holder.Process.Kill() }()
			start := time.Now()
			first := tickRunner(t, r)
			_ = holder.Wait()
			if !first.Connected || m.restored != 0 {
				t.Fatal("detached before the window")
			}
			if delay := 2100*time.Millisecond - time.Since(start); delay > 0 {
				time.Sleep(delay)
			}
			s := tickRunner(t, r)
			t.Logf("elapsed=%v healthy=%v state=%s connected=%v restores=%d", time.Since(start), healthy, s.State, s.Connected, m.restored)
			if healthy {
				if !s.Connected || !m.connected || m.restored != 0 {
					t.Fatal("healthy network detached")
				}
			} else if s.Connected || m.connected || m.restored != 1 {
				t.Fatal("unavailable storage then network exceeded the shared window")
			}
		})
	}
}

func TestProbeDefaultWindowUnderCrossProcessLock(t *testing.T) {
	r, m, _, _ := runnerFixture(t)
	r.options.Now = time.Now
	tickRunner(t, r)
	holder := exec.Command(os.Args[0], "-test.run=^TestProbeStorageLockHolder$", "-test.count=1")
	holder.Env = append(os.Environ(), "MAGPIE_SYNTHETIC_LOCK_PROBE="+connectionPath()+".lock")
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "synthetic-lock-held" {
		t.Fatalf("holder: %q %v", line, err)
	}
	start := time.Now()
	for i := 1; i <= 5; i++ {
		s := tickRunner(t, r)
		t.Logf("tick=%d elapsed=%v state=%s connected=%v restores=%d", i, time.Since(start), s.State, s.Connected, m.restored)
		if m.restored != 0 {
			break
		}
	}
	if m.restored != 1 {
		t.Fatal("did not restore under sustained contention")
	}
	t.Logf("default window=%v elapsed=%v", r.options.Policy.OfflineWindow, time.Since(start))
	if time.Since(start) > r.options.Policy.OfflineWindow+3*time.Second {
		t.Fatal("restore exceeded default window by more than scheduling tolerance")
	}
}
