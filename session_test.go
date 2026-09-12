package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T, ttl time.Duration, max int) *SessionStore {
	t.Helper()
	s := NewSessionStore(ttl, max)
	t.Cleanup(s.Close)
	return s
}

func TestSessionStoreGetOrCreateSnapshot(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	first, created := s.GetOrCreate("key", "cid-1")
	if !created || first.CID != "cid-1" {
		t.Fatalf("first lookup = %+v, created=%v", first, created)
	}
	first.CID = "mutated"
	first.Turns = 99
	got, created := s.GetOrCreate("key", "cid-2")
	if created || got.CID != "cid-1" || got.Turns != 0 {
		t.Fatalf("internal state escaped through snapshot: %+v created=%v", got, created)
	}

	const workers = 40
	var made atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created := s.GetOrCreate("shared", "cid-shared")
			if created {
				made.Add(1)
			}
		}()
	}
	wg.Wait()
	if made.Load() != 1 {
		t.Fatalf("atomic GetOrCreate created %d sessions", made.Load())
	}
}

func TestSessionStorePendingCallIndexAndSnapshots(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	s.GetOrCreate("key", "cid-1")
	want, err := s.PutPendingCall(PendingCall{ID: "call-1", Name: "fetch", Arguments: `{"url":"x"}`, CID: "cid-1", SessionKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolvePendingCall("call-1", "key", "cid-1")
	if err != nil || got.ID != want.ID || got.Arguments != want.Arguments || got.State != CallPending {
		t.Fatalf("resolve = %+v, err=%v", got, err)
	}
	list := s.List()
	if len(list) != 1 || list[0].PendingTool != "fetch" || list[0].PendingCallID != "call-1" {
		t.Fatalf("pending compatibility snapshot = %+v", list)
	}
	list[0].PendingTool = "mutated"
	if s.List()[0].PendingTool != "fetch" {
		t.Fatal("List exposed mutable state")
	}

	_, err = s.PutPendingCall(PendingCall{ID: "orphan", Name: "shell", Arguments: `{}`, CID: "cid-orphan"})
	if err != nil {
		t.Fatal(err)
	}
	orphanLease, acquiredOrphan, err := s.AcquireCall(context.Background(), "orphan", "")
	if err != nil || acquiredOrphan.State != CallInFlight || orphanLease.SessionKey != "" {
		t.Fatalf("sessionless AcquireCall = %+v lease=%+v err=%v", acquiredOrphan, orphanLease, err)
	}
	if err := orphanLease.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreTypedCallErrors(t *testing.T) {
	s := newTestStore(t, 10*time.Millisecond, 10)
	if _, err := s.ResolvePendingCall("missing", "", ""); !IsStoreError(err, StoreErrorUnknownCall) {
		t.Fatalf("unknown error = %v", err)
	}
	s.GetOrCreate("key", "cid")
	if _, err := s.PutPendingCall(PendingCall{ID: "mismatch", Name: "t", CID: "wrong", SessionKey: "key"}); !IsStoreError(err, StoreErrorMismatch) {
		t.Fatalf("put mismatch error = %v", err)
	}
	if _, err := s.PutPendingCall(PendingCall{ID: "call", Name: "t", CID: "cid", SessionKey: "key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePendingCall("call", "other", ""); !IsStoreError(err, StoreErrorMismatch) {
		t.Fatalf("resolve mismatch error = %v", err)
	}
	if _, err := s.ConsumePendingCall("call", "key", "cid"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePendingCall("call", "", ""); !IsStoreError(err, StoreErrorConsumed) {
		t.Fatalf("consumed error = %v", err)
	}
	if _, err := s.PutPendingCall(PendingCall{ID: "old", Name: "t", CID: "cid", CreatedAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolvePendingCall("old", "", ""); !IsStoreError(err, StoreErrorExpired) {
		t.Fatalf("expired error = %v", err)
	}
}

func TestSessionStoreLeaseSerializesByCID(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	s.GetOrCreate("a", "cid-a")
	s.GetOrCreate("b", "cid-b")

	first, err := s.Acquire(context.Background(), "a", "cid-a")
	if err != nil {
		t.Fatal(err)
	}
	sameAcquired := make(chan *Lease, 1)
	go func() {
		l, _ := s.Acquire(context.Background(), "a", "cid-a")
		sameAcquired <- l
	}()
	select {
	case <-sameAcquired:
		t.Fatal("same CID acquired concurrently")
	case <-time.After(25 * time.Millisecond):
	}

	different, err := s.Acquire(context.Background(), "b", "cid-b")
	if err != nil {
		t.Fatalf("different CID should acquire concurrently: %v", err)
	}
	if err := different.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := first.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-sameAcquired:
		if err := l.Rollback(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("same CID waiter did not resume")
	}

	held, err := s.Acquire(context.Background(), "a", "cid-a")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx, "a", "cid-a"); err != context.DeadlineExceeded {
		t.Fatalf("cancelled acquire error = %v", err)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreCommitConsumeAndRollback(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	s.GetOrCreate("key", "cid")
	l, err := s.Acquire(context.Background(), "key", "cid")
	if err != nil {
		t.Fatal(err)
	}
	next := &PendingCall{ID: "call-1", Name: "fetch", Arguments: `{}`}
	if err := l.Commit(1, 12, next); err != nil {
		t.Fatal(err)
	}
	got := s.Get("key")
	if got.Turns != 1 || got.Tokens != 12 || got.PendingCallID != "call-1" {
		t.Fatalf("committed session = %+v", got)
	}

	resultLease, call, err := s.AcquireCall(context.Background(), "call-1", "key")
	if err != nil || call.Name != "fetch" || call.State != CallInFlight {
		t.Fatalf("AcquireCall = %+v, err=%v", call, err)
	}
	if _, err := s.ResolvePendingCall("call-1", "", ""); !IsStoreError(err, StoreErrorInFlight) {
		t.Fatalf("in-flight error = %v", err)
	}
	if err := resultLease.Rollback(); err != nil {
		t.Fatal(err)
	}
	if call, err := s.ResolvePendingCall("call-1", "key", "cid"); err != nil || call.State != CallPending {
		t.Fatalf("rollback did not restore pending: %+v err=%v", call, err)
	}
	resultLease, _, err = s.AcquireCall(context.Background(), "call-1", "key")
	if err != nil {
		t.Fatal(err)
	}
	if err := resultLease.Commit(1, 25, nil); err != nil {
		t.Fatal(err)
	}
	got = s.Get("key")
	if got.Turns != 2 || got.Tokens != 25 || got.PendingCallID != "" {
		t.Fatalf("result commit = %+v", got)
	}
	if _, err := s.ResolvePendingCall("call-1", "", ""); !IsStoreError(err, StoreErrorConsumed) {
		t.Fatalf("post-commit error = %v", err)
	}
}

func TestSessionStoreErrorsDoNotPolluteState(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	s.GetOrCreate("key", "cid")
	l, err := s.Acquire(context.Background(), "key", "cid")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(1, 99, &PendingCall{Name: "missing-id"}); !IsStoreError(err, StoreErrorInvalid) {
		t.Fatalf("invalid commit error = %v", err)
	}
	got := s.Get("key")
	if got.Turns != 0 || got.Tokens != 0 || got.PendingCallID != "" {
		t.Fatalf("failed commit polluted state: %+v", got)
	}
	if err := l.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionStoreTTLDeleteEvictionAndInflight(t *testing.T) {
	s := newTestStore(t, 20*time.Millisecond, 2)
	s.GetOrCreate("old", "cid-old")
	if _, err := s.PutPendingCall(PendingCall{ID: "old-call", Name: "t", CID: "cid-old", SessionKey: "old"}); err != nil {
		t.Fatal(err)
	}
	if !s.Delete("old") {
		t.Fatal("delete failed")
	}
	if _, err := s.ResolvePendingCall("old-call", "", ""); !IsStoreError(err, StoreErrorExpired) {
		t.Fatalf("delete did not expire call index: %v", err)
	}

	s.GetOrCreate("one", "cid-one")
	time.Sleep(time.Millisecond)
	s.GetOrCreate("two", "cid-two")
	if _, err := s.PutPendingCall(PendingCall{ID: "one-call", Name: "t", CID: "cid-one", SessionKey: "one"}); err != nil {
		t.Fatal(err)
	}
	s.GetOrCreate("three", "cid-three")
	if s.Get("one") != nil {
		t.Fatal("oldest session was not evicted")
	}
	if _, err := s.ResolvePendingCall("one-call", "", ""); !IsStoreError(err, StoreErrorExpired) {
		t.Fatalf("eviction did not expire call index: %v", err)
	}

	held, err := s.Acquire(context.Background(), "two", "cid-two")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	s.cleanup()
	if s.Get("two") == nil {
		t.Fatal("cleanup evicted in-flight session")
	}
	if s.Delete("two") {
		t.Fatal("Delete removed in-flight session")
	}
	// At capacity with all eligible entries expired, creation may evict idle but not held.
	s.GetOrCreate("four", "cid-four")
	if s.Get("two") == nil {
		t.Fatal("capacity eviction removed in-flight session")
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	s.cleanup()
	if s.Get("two") != nil {
		t.Fatal("idle expired session survived cleanup")
	}
}

func TestSessionStoreConcurrentAccounting(t *testing.T) {
	s := newTestStore(t, time.Hour, 10)
	s.GetOrCreate("key", "cid")
	const workers = 50
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := s.Acquire(context.Background(), "key", "cid")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			if err := l.Commit(1, l.Session.Tokens+1, nil); err != nil {
				t.Errorf("Commit: %v", err)
			}
		}()
	}
	wg.Wait()
	got := s.Get("key")
	if got.Turns != workers || got.Tokens != workers {
		t.Fatalf("lost concurrent updates: %+v", got)
	}
}
