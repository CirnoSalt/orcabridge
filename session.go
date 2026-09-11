package main

import (
	"sort"
	"sync"
	"time"
)

// Session 映射调用方会话 -> 上游 conversationId。
// 上游服务端会持久化会话消息，因此复用同一个 conversationId 即可获得连续上下文。
type Session struct {
	Key         string    // 调用方会话标识
	CID         string    // 上游 cid-<uuid>-<product>
	Last        time.Time // 最近一次请求时间
	Turns       int       // 已进行轮次
	Tokens      int       // 估算累计 token（用于"上下文剩余量"）
	PendingTool string    // 上游已请求、调用方尚未回传结果的工具名（空=无待处理）
}

// SessionStore 带 TTL 与容量上限的会话表。
type SessionStore struct {
	mu  sync.Mutex
	m   map[string]*Session
	ttl time.Duration
	max int
}

func NewSessionStore(ttl time.Duration, max int) *SessionStore {
	s := &SessionStore{m: make(map[string]*Session), ttl: ttl, max: max}
	go s.loop()
	return s
}

// Get 返回未过期的会话；已过期则删除并返回 nil。
func (s *SessionStore) Get(key string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return nil
	}
	if s.ttl > 0 && time.Since(v.Last) > s.ttl {
		delete(s.m, key)
		return nil
	}
	return v
}

func (s *SessionStore) Create(key, cid string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.max > 0 && len(s.m) >= s.max {
		s.evictLocked()
	}
	v := &Session{Key: key, CID: cid, Last: time.Now()}
	s.m[key] = v
	return v
}

func (s *SessionStore) Delete(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		return false
	}
	delete(s.m, key)
	return true
}

func (s *SessionStore) List() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.m))
	for _, v := range s.m {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out
}

// evictLocked 淘汰最久未使用的会话（需持有锁）。
func (s *SessionStore) evictLocked() {
	var oldest *Session
	for _, v := range s.m {
		if oldest == nil || v.Last.Before(oldest.Last) {
			oldest = v
		}
	}
	if oldest != nil {
		delete(s.m, oldest.Key)
	}
}

func (s *SessionStore) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ttl <= 0 {
		return
	}
	for k, v := range s.m {
		if time.Since(v.Last) > s.ttl {
			delete(s.m, k)
		}
	}
}

func (s *SessionStore) loop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		s.cleanup()
	}
}

// sessionKeyOf 决定调用方会话标识：优先 X-Session-Id，其次 OpenAI 的 user 字段。
// 返回空表示不使用会话（每次新建，无上下文）。
func sessionKeyOf(header string, user string) string {
	if header != "" {
		return header
	}
	return user
}
