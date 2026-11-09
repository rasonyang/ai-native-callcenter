// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"sync"
	syncatomic "sync/atomic"
	"testing"
	"time"
)

// #508: Stop is what lets a client close the channel its OnStall emits on. These
// tests pin the three promises it makes: nothing runs after it returns, it
// waits for a stall in progress, and it works whenever it is called.

func newStoppableDog(onStall func(bool), firstAudio time.Duration) (*Watchdog, chan struct{}) {
	done := make(chan struct{})
	dog := NewWatchdog(WatchdogConfig{
		FirstAudioDeadline: firstAudio,
		DeltaStallDeadline: firstAudio,
		Done:               done,
		IsResponseOpen:     func() bool { return true },
		OnStall:            onStall,
	})
	return dog, done
}

func TestNoStallIsReportedAfterStopReturns(t *testing.T) {
	for run := range 40 {
		var stalls syncatomic.Int32
		dog, _ := newStoppableDog(func(bool) { stalls.Add(1) }, time.Millisecond)
		runDone := make(chan struct{})
		go func() { dog.Run(); close(runDone) }()

		dog.Signal(WatchResponseStarted)
		// Stop lands around the deadline: some runs see the stall, some not.
		time.Sleep(time.Duration(run%4) * 500 * time.Microsecond)
		dog.Stop()
		// Run's last act is closing what Stop waits on, so it is over, or
		// about to report that it is, once Stop has returned.
		select {
		case <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after Stop")
		}

		seen := stalls.Load()
		time.Sleep(5 * time.Millisecond)
		if got := stalls.Load(); got != seen {
			t.Fatalf("run %d: OnStall ran after Stop returned (%d before, %d after)", run, seen, got)
		}
	}
}

func TestStopWaitsForAStallInProgress(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var isFinished syncatomic.Bool
	dog, _ := newStoppableDog(func(bool) {
		close(entered)
		<-release
		isFinished.Store(true)
	}, time.Millisecond)
	go dog.Run()
	dog.Signal(WatchResponseStarted)
	<-entered

	stopped := make(chan struct{})
	go func() { dog.Stop(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("Stop returned while OnStall was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned after OnStall finished")
	}
	if !isFinished.Load() {
		t.Error("Stop returned before OnStall had finished")
	}
}

func TestStopBeforeRunHasBeenScheduledReturnsOnceRunStarts(t *testing.T) {
	var stalls syncatomic.Int32
	dog, _ := newStoppableDog(func(bool) { stalls.Add(1) }, time.Millisecond)
	// A signal already waiting must not outlive the Stop either.
	dog.Signal(WatchResponseStarted)

	stopped := make(chan struct{})
	go func() { dog.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned although Run had not started")
	case <-time.After(30 * time.Millisecond):
	}

	go dog.Run()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned once Run started")
	}
	time.Sleep(10 * time.Millisecond)
	if stalls.Load() != 0 {
		t.Error("a watchdog stopped before it ran reported a stall")
	}
}

func TestStopTwiceIsFine(t *testing.T) {
	dog, _ := newStoppableDog(func(bool) {}, time.Hour)
	go dog.Run()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); dog.Stop() }()
	}
	wg.Wait()
	dog.Stop()

	// A watchdog the session's Done already ended can still be stopped.
	other, otherDone := newStoppableDog(func(bool) {}, time.Hour)
	go other.Run()
	close(otherDone)
	other.Stop()
}
