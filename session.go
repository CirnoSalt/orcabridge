package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is a detached snapshot of a caller session.
type Session struct {
	Key           string
	CID           string
	Last          time.Time
	Turns         int
	Tokens        int
	PendingTool   string // Kept for the sessions endpoint and older callers.
	PendingCallID string
}

type CallState string

const (
	CallPending  CallState = "pending"
	CallInFlight CallState = "in_flight"
	CallConsumed CallState = "consumed"
	CallExpired  CallState = "expired"
)

// PendingCall contains everything needed to route a later tool result.
type PendingCall struct {
	ID   string
	Name string
	// NativeName 是这条调用在**上游**那侧的名字。未桥接时等于 Name；
	// 桥接时 Name 是发给客户端的名字（如 Bash），NativeName 是上游原名
	// （如 execute_command）—— 回填结果必须以原生名的名义送回上游。
	NativeName string
	Arguments  string
	CID        string
	SessionKey string
	CreatedAt  time.Time
	State      CallState
}

// UpstreamName 返回把结果回填给上游时应当使用的工具名。
// 桥接时对外发的是客户端名（Bash），但上游只认自己的原生名（execute_command），
// 所以回填一律用 NativeName；未桥接时 NativeName 为空，退回 Name。
func (p PendingCall) UpstreamName() string {
	if strings.TrimSpace(p.NativeName) != "" {
		return p.NativeName
	}
	return p.Name
}

type StoreErrorKind string

const (
	StoreErrorUnknownCall StoreErrorKind = "unknown_call"
	StoreErrorConsumed    StoreErrorKind = "consumed_call"
	StoreErrorExpired     StoreErrorKind = "expired_call"
	StoreErrorMismatch    StoreErrorKind = "call_mismatch"
	StoreErrorInFlight    StoreErrorKind = "call_in_flight"
	StoreErrorCallExists  StoreErrorKind = "call_exists"
	StoreErrorLeaseClosed StoreErrorKind = "lease_closed"
	StoreErrorInvalid     StoreErrorKind = "invalid"
)

type StoreError struct {
	Kind     StoreErrorKind
	CallID   string
	Expected string
	Actual   string
}

func (e *StoreError) Error() string {
	if e.Expected != "" || e.Actual != "" {
		return fmt.Sprintf("session store %s for call %q: expected %q, got %q", e.Kind, e.CallID, e.Expected, e.Actual)
	}
	return fmt.Sprintf("session store %s for call %q", e.Kind, e.CallID)
}

func IsStoreError(err error, kind StoreErrorKind) bool {
	var target *StoreError
	return errors.As(err, &target) && target.Kind == kind
}

type sessionEntry struct {
	Session
	pending map[string]struct{}
	active  int
}

type callEntry struct {
	PendingCall
	owner uint64
}

type cidGate struct {
	ch   chan struct{}
	refs int
}

// SessionStore owns all mutable session and pending-call state.
type SessionStore struct {
	mu       sync.Mutex
	m        map[string]*sessionEntry
	calls    map[string]*callEntry
	gates    map[string]*cidGate
	ttl      time.Duration
	max      int
	nextID   uint64
	stop     chan struct{}
	stopOnce sync.Once
	now      func() time.Time
}

func NewSessionStore(ttl time.Duration, max int) *SessionStore {
	s := &SessionStore{
		m: make(map[string]*sessionEntry), calls: make(map[string]*callEntry),
		gates: make(map[string]*cidGate), ttl: ttl, max: max,
		stop: make(chan struct{}), now: time.Now,
	}
	go s.loop()
	return s
}

// Close stops background cleanup. The store remains usable.
func (s *SessionStore) Close() { s.stopOnce.Do(func() { close(s.stop) }) }

func (s *SessionStore) expired(last, now time.Time) bool {
	return s.ttl > 0 && now.Sub(last) > s.ttl
}

func (s *SessionStore) snapshotLocked(v *sessionEntry) Session {
	out := v.Session
	out.PendingTool, out.PendingCallID = "", ""
	var newest time.Time
	for id := range v.pending {
		if c := s.calls[id]; c != nil && c.State != CallConsumed && (out.PendingCallID == "" || c.CreatedAt.After(newest)) {
			out.PendingTool, out.PendingCallID, newest = c.Name, c.ID, c.CreatedAt
		}
	}
	return out
}

// Get returns a detached snapshot. Expired idle sessions are removed.
func (s *SessionStore) Get(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.m[key]
	if v == nil {
		return nil
	}
	if s.expired(v.Last, s.now()) && v.active == 0 {
		s.deleteSessionLocked(key)
		return nil
	}
	out := s.snapshotLocked(v)
	return &out
}

// GetOrCreate atomically finds or creates a session and returns a snapshot.
func (s *SessionStore) GetOrCreate(key, cid string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if v := s.m[key]; v != nil {
		if !s.expired(v.Last, now) || v.active > 0 {
			return s.snapshotLocked(v), false
		}
		s.deleteSessionLocked(key)
	}
	if s.max > 0 && len(s.m) >= s.max {
		s.evictLocked()
	}
	v := &sessionEntry{Session: Session{Key: key, CID: cid, Last: now}, pending: make(map[string]struct{})}
	s.m[key] = v
	return s.snapshotLocked(v), true
}

// Create replaces key with a new session and returns a detached snapshot.
func (s *SessionStore) Create(key, cid string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.m[key]; old != nil {
		if old.active > 0 {
			out := s.snapshotLocked(old)
			return &out
		}
		s.deleteSessionLocked(key)
	}
	if s.max > 0 && len(s.m) >= s.max {
		s.evictLocked()
	}
	v := &sessionEntry{Session: Session{Key: key, CID: cid, Last: s.now()}, pending: make(map[string]struct{})}
	s.m[key] = v
	out := s.snapshotLocked(v)
	return &out
}

func (s *SessionStore) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.m[key]
	if v == nil || v.active > 0 {
		return false
	}
	s.deleteSessionLocked(key)
	return true
}

func (s *SessionStore) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]Session, 0, len(s.m))
	for key, v := range s.m {
		if s.expired(v.Last, now) && v.active == 0 {
			s.deleteSessionLocked(key)
			continue
		}
		out = append(out, s.snapshotLocked(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

func (s *SessionStore) deleteSessionLocked(key string) {
	v := s.m[key]
	if v == nil {
		return
	}
	for id := range v.pending {
		if c := s.calls[id]; c != nil {
			c.State, c.owner = CallExpired, 0
		}
	}
	delete(s.m, key)
}

func (s *SessionStore) removeCallLocked(c *callEntry, state CallState) {
	c.State, c.owner = state, 0
	if v := s.m[c.SessionKey]; v != nil {
		delete(v.pending, c.ID)
	}
}

func (s *SessionStore) evictLocked() bool {
	var oldest *sessionEntry
	for _, v := range s.m {
		if v.active == 0 && (oldest == nil || v.Last.Before(oldest.Last)) {
			oldest = v
		}
	}
	if oldest == nil {
		return false
	}
	s.deleteSessionLocked(oldest.Key)
	return true
}

func (s *SessionStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, v := range s.m {
		if v.active == 0 && s.expired(v.Last, now) {
			s.deleteSessionLocked(key)
		}
	}
	// Sessionless calls have no owning entry, so expire them directly.
	for _, c := range s.calls {
		if c.SessionKey == "" && c.State != CallInFlight && c.State != CallConsumed && c.State != CallExpired && s.expired(c.CreatedAt, now) {
			s.removeCallLocked(c, CallExpired)
		}
	}
}

func (s *SessionStore) loop() {
	interval := 5 * time.Minute
	if s.ttl > 0 && s.ttl < interval {
		interval = s.ttl
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.cleanup()
		case <-s.stop:
			return
		}
	}
}

// PutPendingCall adds a globally addressable call, including sessionless calls.
func (s *SessionStore) PutPendingCall(call PendingCall) (PendingCall, error) {
	if call.ID == "" || call.Name == "" || call.CID == "" {
		return PendingCall{}, &StoreError{Kind: StoreErrorInvalid, CallID: call.ID}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.calls[call.ID]; existing != nil {
		if existing.State == CallConsumed || existing.State == CallExpired {
			delete(s.calls, call.ID)
		} else {
			return PendingCall{}, &StoreError{Kind: StoreErrorCallExists, CallID: call.ID}
		}
	}
	if call.SessionKey != "" {
		v := s.m[call.SessionKey]
		if v == nil || v.CID != call.CID {
			actual := ""
			if v != nil {
				actual = v.CID
			}
			return PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: call.ID, Expected: call.CID, Actual: actual}
		}
	}
	if call.CreatedAt.IsZero() {
		call.CreatedAt = s.now()
	}
	call.State = CallPending
	s.calls[call.ID] = &callEntry{PendingCall: call}
	if v := s.m[call.SessionKey]; v != nil {
		v.pending[call.ID] = struct{}{}
	}
	return call, nil
}

// ResolvePendingCall resolves a tool_call_id without requiring a session key.
// Supplying non-empty expected values enforces routing consistency.
func (s *SessionStore) ResolvePendingCall(id, sessionKey, cid string) (PendingCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls[id]
	if c == nil {
		return PendingCall{}, &StoreError{Kind: StoreErrorUnknownCall, CallID: id}
	}
	if c.State == CallConsumed {
		return PendingCall{}, &StoreError{Kind: StoreErrorConsumed, CallID: id}
	}
	if c.State == CallExpired {
		return PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if c.State == CallInFlight {
		return PendingCall{}, &StoreError{Kind: StoreErrorInFlight, CallID: id}
	}
	if s.expired(c.CreatedAt, s.now()) {
		s.removeCallLocked(c, CallExpired)
		return PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if sessionKey != "" && c.SessionKey != sessionKey {
		return PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: id, Expected: c.SessionKey, Actual: sessionKey}
	}
	if cid != "" && c.CID != cid {
		return PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: id, Expected: c.CID, Actual: cid}
	}
	return c.PendingCall, nil
}

func (s *SessionStore) gateFor(cid string) *cidGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gates[cid]
	if g == nil {
		g = &cidGate{ch: make(chan struct{}, 1)}
		g.ch <- struct{}{}
		s.gates[cid] = g
	}
	g.refs++
	return g
}

func (s *SessionStore) releaseGate(cid string, g *cidGate) {
	g.ch <- struct{}{}
	s.mu.Lock()
	g.refs--
	if g.refs == 0 {
		delete(s.gates, cid)
	}
	s.mu.Unlock()
}

// Lease serializes one request per CID and stages all state changes until Commit.
type Lease struct {
	store      *SessionStore
	gate       *cidGate
	CID        string
	SessionKey string
	Session    Session
	owner      uint64
	callID     string
	closed     bool
}

// Acquire obtains a CID-scoped request lease. key may be empty.
func (s *SessionStore) Acquire(ctx context.Context, key, cid string) (*Lease, error) {
	if cid == "" {
		return nil, &StoreError{Kind: StoreErrorInvalid}
	}
	g := s.gateFor(cid)
	select {
	case <-ctx.Done():
		s.mu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(s.gates, cid)
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	case <-g.ch:
	}
	s.mu.Lock()
	s.nextID++
	l := &Lease{store: s, gate: g, CID: cid, SessionKey: key, owner: s.nextID}
	if key != "" {
		if v := s.m[key]; v != nil && v.CID == cid {
			v.active++
			l.Session = s.snapshotLocked(v)
		} else {
			s.mu.Unlock()
			s.releaseGate(cid, g)
			return nil, &StoreError{Kind: StoreErrorMismatch, Expected: cid, Actual: key}
		}
	}
	s.mu.Unlock()
	return l, nil
}

// AcquireCall resolves and exclusively reserves a pending tool call.
func (s *SessionStore) AcquireCall(ctx context.Context, id, sessionKey string) (*Lease, PendingCall, error) {
	s.mu.Lock()
	c := s.calls[id]
	if c == nil {
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorUnknownCall, CallID: id}
	}
	if c.State == CallConsumed {
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorConsumed, CallID: id}
	}
	if c.State == CallExpired {
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if c.State == CallInFlight {
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorInFlight, CallID: id}
	}
	if s.expired(c.CreatedAt, s.now()) {
		s.removeCallLocked(c, CallExpired)
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if sessionKey != "" && c.SessionKey != sessionKey {
		s.mu.Unlock()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: id, Expected: c.SessionKey, Actual: sessionKey}
	}
	call := c.PendingCall
	s.mu.Unlock()

	l, err := s.Acquire(ctx, call.SessionKey, call.CID)
	if err != nil {
		return nil, PendingCall{}, err
	}
	s.mu.Lock()
	c = s.calls[id]
	if c == nil || c.State != CallPending {
		s.mu.Unlock()
		l.Rollback()
		return nil, PendingCall{}, &StoreError{Kind: StoreErrorInFlight, CallID: id}
	}
	c.State, c.owner = CallInFlight, l.owner
	l.callID = id
	call = c.PendingCall
	s.mu.Unlock()
	return l, call, nil
}

// Commit atomically applies successful request accounting and pending-call changes.
// A call acquired through AcquireCall is consumed. nextCall, when non-nil, is added.
func (l *Lease) Commit(turnDelta, tokens int, nextCall *PendingCall) error {
	if l == nil || l.store == nil {
		return &StoreError{Kind: StoreErrorLeaseClosed}
	}
	s := l.store
	s.mu.Lock()
	if l.closed {
		s.mu.Unlock()
		return &StoreError{Kind: StoreErrorLeaseClosed, CallID: l.callID}
	}
	if nextCall != nil {
		if nextCall.ID == "" || nextCall.Name == "" {
			s.mu.Unlock()
			return &StoreError{Kind: StoreErrorInvalid, CallID: nextCall.ID}
		}
		if existing := s.calls[nextCall.ID]; existing != nil && existing.State != CallConsumed && existing.State != CallExpired {
			s.mu.Unlock()
			return &StoreError{Kind: StoreErrorCallExists, CallID: nextCall.ID}
		}
	}
	if l.callID != "" {
		c := s.calls[l.callID]
		if c == nil || c.State != CallInFlight || c.owner != l.owner {
			s.mu.Unlock()
			return &StoreError{Kind: StoreErrorMismatch, CallID: l.callID}
		}
		c.State = CallConsumed
		c.owner = 0
		if v := s.m[c.SessionKey]; v != nil {
			delete(v.pending, c.ID)
		}
	}
	if v := s.m[l.SessionKey]; v != nil {
		v.Turns += turnDelta
		v.Tokens = tokens
		v.Last = s.now()
	}
	if nextCall != nil {
		c := *nextCall
		c.CID, c.SessionKey, c.State = l.CID, l.SessionKey, CallPending
		if c.CreatedAt.IsZero() {
			c.CreatedAt = s.now()
		}
		s.calls[c.ID] = &callEntry{PendingCall: c}
		if v := s.m[l.SessionKey]; v != nil {
			v.pending[c.ID] = struct{}{}
		}
	}
	l.finishLocked()
	s.mu.Unlock()
	s.releaseGate(l.CID, l.gate)
	return nil
}

// Rollback releases the lease and restores an acquired call to pending.
func (l *Lease) Rollback() error {
	if l == nil || l.store == nil {
		return &StoreError{Kind: StoreErrorLeaseClosed}
	}
	s := l.store
	s.mu.Lock()
	if l.closed {
		s.mu.Unlock()
		return &StoreError{Kind: StoreErrorLeaseClosed, CallID: l.callID}
	}
	if l.callID != "" {
		if c := s.calls[l.callID]; c != nil && c.State == CallInFlight && c.owner == l.owner {
			c.State, c.owner = CallPending, 0
		}
	}
	l.finishLocked()
	s.mu.Unlock()
	s.releaseGate(l.CID, l.gate)
	return nil
}

func (l *Lease) finishLocked() {
	if v := l.store.m[l.SessionKey]; v != nil && v.active > 0 {
		v.active--
	}
	l.closed = true
}

// ConsumePendingCall provides a non-leased atomic consume operation.
func (s *SessionStore) ConsumePendingCall(id, sessionKey, cid string) (PendingCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls[id]
	if c == nil {
		return PendingCall{}, &StoreError{Kind: StoreErrorUnknownCall, CallID: id}
	}
	if c.State == CallConsumed {
		return PendingCall{}, &StoreError{Kind: StoreErrorConsumed, CallID: id}
	}
	if c.State == CallExpired {
		return PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if c.State == CallInFlight {
		return PendingCall{}, &StoreError{Kind: StoreErrorInFlight, CallID: id}
	}
	if s.expired(c.CreatedAt, s.now()) {
		s.removeCallLocked(c, CallExpired)
		return PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	if sessionKey != "" && c.SessionKey != sessionKey {
		return PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: id, Expected: c.SessionKey, Actual: sessionKey}
	}
	if cid != "" && c.CID != cid {
		return PendingCall{}, &StoreError{Kind: StoreErrorMismatch, CallID: id, Expected: c.CID, Actual: cid}
	}
	c.State = CallConsumed
	if v := s.m[c.SessionKey]; v != nil {
		delete(v.pending, c.ID)
	}
	return c.PendingCall, nil
}

// RollbackPendingCall makes an in-flight call pending again. It is intended for
// integrations that reserve calls outside AcquireCall; leases roll back directly.
func (s *SessionStore) RollbackPendingCall(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calls[id]
	if c == nil {
		return &StoreError{Kind: StoreErrorUnknownCall, CallID: id}
	}
	if c.State == CallConsumed {
		return &StoreError{Kind: StoreErrorConsumed, CallID: id}
	}
	if c.State == CallExpired {
		return &StoreError{Kind: StoreErrorExpired, CallID: id}
	}
	c.State, c.owner = CallPending, 0
	return nil
}

// UniquePendingCall returns the single pending call owned by a session key.
//
// legacy role="function" 结果没有 tool_call_id，只能靠会话键定位；只有当该会话
// 恰好存在唯一待处理调用时才允许继续，否则返回类型化错误。
func (s *SessionStore) UniquePendingCall(key string) (PendingCall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if key == "" {
		return PendingCall{}, &StoreError{Kind: StoreErrorInvalid}
	}
	v := s.m[key]
	if v == nil {
		return PendingCall{}, &StoreError{Kind: StoreErrorUnknownCall}
	}
	var found *callEntry
	for id := range v.pending {
		c := s.calls[id]
		if c == nil || c.State == CallConsumed || c.State == CallExpired {
			continue
		}
		if c.State == CallInFlight {
			return PendingCall{}, &StoreError{Kind: StoreErrorInFlight, CallID: c.ID}
		}
		if found != nil {
			return PendingCall{}, &StoreError{Kind: StoreErrorInvalid}
		}
		found = c
	}
	if found == nil {
		// 该会话已经没有待处理调用。若最近有一条已被消费的调用，返回 consumed
		// 以便调用方区分"重复回传"与"从未有过调用"。
		if c := s.latestConsumedLocked(key); c != nil {
			return PendingCall{}, &StoreError{Kind: StoreErrorConsumed, CallID: c.ID}
		}
		return PendingCall{}, &StoreError{Kind: StoreErrorUnknownCall}
	}
	if s.expired(found.CreatedAt, s.now()) {
		s.removeCallLocked(found, CallExpired)
		return PendingCall{}, &StoreError{Kind: StoreErrorExpired, CallID: found.ID}
	}
	return found.PendingCall, nil
}

// latestConsumedLocked 返回某会话最近一次已消费的调用（用于识别重复回传）。
func (s *SessionStore) latestConsumedLocked(key string) *callEntry {
	var newest *callEntry
	for _, c := range s.calls {
		if c.SessionKey != key || c.State != CallConsumed {
			continue
		}
		if newest == nil || c.CreatedAt.After(newest.CreatedAt) {
			newest = c
		}
	}
	return newest
}

// sessionKeyOf decides the caller session key.
func sessionKeyOf(header string, user string) string {
	if header != "" {
		return header
	}
	return user
}
