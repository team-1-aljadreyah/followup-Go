package ws

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"followapi/internal/config"
	"followapi/internal/httpx"
	"followapi/internal/store"
)

const (
	// CloseUnauthorized is the custom WebSocket close code (docs/backend.md §25)
	// telling the frontend to sign out as if the API returned 401.
	CloseUnauthorized = 4401

	DefaultAuthTimeout  = 5 * time.Second
	DefaultPingInterval = 25 * time.Second
	DefaultPongWait     = 65 * time.Second
	DefaultWriteWait    = 10 * time.Second
)

// TokenValidator validates a raw Sanctum Bearer token (without "Bearer ") and
// returns the active user, session ID, and token ID on success.
type TokenValidator func(ctx context.Context, bearer string) (*store.User, string, int64, error)

type client struct {
	hub       *Hub
	conn      *websocket.Conn
	userID    int64
	sessionID string
	tokenID   int64
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (c *client) signalClose(code int, reason string) {
	c.closeOnce.Do(func() {
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(time.Second),
		)
		close(c.done)
		_ = c.conn.Close()
	})
}

type Hub struct {
	cfg       config.Config
	validate  TokenValidator
	upgrader  websocket.Upgrader

	AuthTimeout  time.Duration
	PingInterval time.Duration
	PongWait     time.Duration
	WriteWait    time.Duration

	mu             sync.RWMutex
	all            map[*client]struct{}
	byUser         map[int64]map[*client]struct{}
	bySession      map[string]map[*client]struct{}
	onInvalidateU  func(int64)
	onInvalidateT  func(int64)
	onInvalidateSs func([]string)
}

func (h *Hub) SetAuthInvalidators(onUser func(int64), onToken func(int64), onSessions func([]string)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.onInvalidateU = onUser
	h.onInvalidateT = onToken
	h.onInvalidateSs = onSessions
	h.mu.Unlock()
}

func (h *Hub) InvalidateAuthUser(userID int64) {
	if h == nil || userID <= 0 {
		return
	}
	h.mu.RLock()
	fn := h.onInvalidateU
	h.mu.RUnlock()
	if fn != nil {
		fn(userID)
	}
}

func New(cfg config.Config, validate TokenValidator) *Hub {
	originAllowed := httpx.OriginAllowedFn(cfg)
	h := &Hub{
		cfg:          cfg,
		validate:     validate,
		AuthTimeout:  DefaultAuthTimeout,
		PingInterval: DefaultPingInterval,
		PongWait:     DefaultPongWait,
		WriteWait:    DefaultWriteWait,
		all:          make(map[*client]struct{}),
		byUser:       make(map[int64]map[*client]struct{}),
		bySession:    make(map[string]map[*client]struct{}),
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin == "" {
				return true
			}
			if originAllowed(origin) {
				return true
			}
			// Also allow same-host origins (e.g. Swagger UI / direct IP / localhost)
			if r.Host != "" && (strings.HasSuffix(origin, "://"+r.Host)) {
				return true
			}
			log.Printf("[WS] rejected origin=%q host=%q", origin, r.Host)
			return false
		},
	}
	return h
}

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] upgrade failed host=%q origin=%q err=%v", r.Host, r.Header.Get("Origin"), err)
		return
	}

	c := &client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 64),
		done: make(chan struct{}),
	}

	// §25: Expect {"type":"auth","token":"..."} within ~5 seconds; otherwise close.
	conn.SetReadLimit(16 * 1024)
	_ = conn.SetReadDeadline(time.Now().Add(h.AuthTimeout))
	msgType, raw, err := conn.ReadMessage()
	if err != nil || msgType != websocket.TextMessage {
		if err != nil && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
			log.Printf("[WS] auth read failed: %v", err)
		}
		_ = conn.Close()
		return
	}

	var authMsg struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &authMsg); err != nil || authMsg.Type != "auth" {
		log.Printf("[WS] invalid initial auth payload (type=%q err=%v)", authMsg.Type, err)
		c.signalClose(CloseUnauthorized, "unauthorized")
		return
	}

	token := strings.TrimSpace(authMsg.Token)
	if len(token) > 7 && strings.EqualFold(token[:7], "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" || h.validate == nil {
		log.Printf("[WS] empty token in auth payload")
		c.signalClose(CloseUnauthorized, "unauthorized")
		return
	}

	valCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	user, sid, tid, err := h.validate(valCtx, token)
	cancel()
	if err != nil || user == nil {
		log.Printf("[WS] auth token rejected (err=%v)", err)
		c.signalClose(CloseUnauthorized, "unauthorized")
		return
	}

	c.userID = user.ID
	c.sessionID = sid
	c.tokenID = tid
	h.register(c)

	go h.writePump(c)
	h.readPump(c)
}

func (h *Hub) readPump(c *client) {
	defer func() {
		h.unregister(c)
		_ = c.conn.Close()
	}()

	_ = c.conn.SetReadDeadline(time.Now().Add(h.PongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(h.PongWait))
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(h.PongWait))
	}
}

func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(h.PingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case <-c.done:
			return
		case payload := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.WriteWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(h.WriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (h *Hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.all[c] = struct{}{}
	if h.byUser[c.userID] == nil {
		h.byUser[c.userID] = make(map[*client]struct{})
	}
	h.byUser[c.userID][c] = struct{}{}
	if c.sessionID != "" {
		if h.bySession[c.sessionID] == nil {
			h.bySession[c.sessionID] = make(map[*client]struct{})
		}
		h.bySession[c.sessionID][c] = struct{}{}
	}
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.all[c]; !ok {
		return
	}
	delete(h.all, c)
	if m := h.byUser[c.userID]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byUser, c.userID)
		}
	}
	if c.sessionID != "" {
		if m := h.bySession[c.sessionID]; m != nil {
			delete(m, c)
			if len(m) == 0 {
				delete(h.bySession, c.sessionID)
			}
		}
	}
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

func (h *Hub) sendToUsers(userIDs []int64, event any) {
	if h == nil || len(userIDs) == 0 {
		return
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}

	seenUsers := make(map[int64]struct{}, len(userIDs))
	var targets []*client

	h.mu.RLock()
	for _, uid := range userIDs {
		if uid <= 0 {
			continue
		}
		if _, dup := seenUsers[uid]; dup {
			continue
		}
		seenUsers[uid] = struct{}{}
		for c := range h.byUser[uid] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		enqueueOrDrop(c, raw)
	}
}

func (h *Hub) broadcastAll(event any) {
	if h == nil {
		return
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}

	h.mu.RLock()
	targets := make([]*client, 0, len(h.all))
	for c := range h.all {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		enqueueOrDrop(c, raw)
	}
}

func enqueueOrDrop(c *client, raw []byte) {
	select {
	case <-c.done:
		return
	case c.send <- raw:
	default:
		// Slow/stuck client: drop asynchronously without blocking callers.
		go c.signalClose(websocket.CloseGoingAway, "slow consumer")
	}
}

// §25 Events (server -> site)

func (h *Hub) NotifyNotification(userID, notifID int64) {
	h.sendToUsers([]int64{userID}, map[string]any{
		"type": "notification",
		"id":   notifID,
	})
}

func (h *Hub) NotifyChatMessage(memberIDs []int64, chatKey string, messageID int64) {
	h.sendToUsers(memberIDs, map[string]any{
		"type": "chat_message",
		"chat": chatKey,
		"id":   messageID,
	})
}

func (h *Hub) NotifyChatRead(userID int64, chatKey string) {
	h.sendToUsers([]int64{userID}, map[string]any{
		"type": "chat_read",
		"chat": chatKey,
	})
}

func (h *Hub) NotifyChatChanged(memberIDs []int64, chatKey string) {
	h.sendToUsers(memberIDs, map[string]any{
		"type": "chat_changed",
		"chat": chatKey,
	})
}

func (h *Hub) NotifyCelebration(recipientIDs []int64, celebrationID int64) {
	h.sendToUsers(recipientIDs, map[string]any{
		"type": "celebration",
		"id":   celebrationID,
	})
}

func (h *Hub) NotifySiteLive() {
	h.broadcastAll(map[string]any{
		"type": "site_live",
	})
}

// Session/user termination helpers (closes with §25 code 4401)

func (h *Hub) CloseUnauthorizedUser(userID int64) {
	if h == nil || userID <= 0 {
		return
	}
	h.mu.RLock()
	fn := h.onInvalidateU
	var targets []*client
	for c := range h.byUser[userID] {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	if fn != nil {
		fn(userID)
	}

	for _, c := range targets {
		c.signalClose(CloseUnauthorized, "unauthorized")
	}
}

func (h *Hub) CloseUnauthorizedToken(tokenID int64) {
	if h == nil || tokenID <= 0 {
		return
	}
	h.mu.RLock()
	fn := h.onInvalidateT
	var targets []*client
	for c := range h.all {
		if c.tokenID == tokenID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	if fn != nil {
		fn(tokenID)
	}

	for _, c := range targets {
		c.signalClose(CloseUnauthorized, "unauthorized")
	}
}

func (h *Hub) CloseUnauthorizedSessions(sessionIDs []string) {
	if h == nil || len(sessionIDs) == 0 {
		return
	}
	h.mu.RLock()
	fn := h.onInvalidateSs
	var targets []*client
	for _, sid := range sessionIDs {
		if sid == "" {
			continue
		}
		for c := range h.bySession[sid] {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	if fn != nil {
		fn(sessionIDs)
	}

	for _, c := range targets {
		c.signalClose(CloseUnauthorized, "unauthorized")
	}
}

func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.RLock()
	targets := make([]*client, 0, len(h.all))
	for c := range h.all {
		targets = append(targets, c)
	}
	h.mu.RUnlock()

	for _, c := range targets {
		c.signalClose(websocket.CloseNormalClosure, "shutdown")
	}
}

func (h *Hub) ConnectedCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.all)
}
