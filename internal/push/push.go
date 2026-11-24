package push

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"followapi/internal/access"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
)

const MaxPayloadBytes = 2900
const StaleDays = 120
const pushWorkers = 12

type Job struct {
	SubscriptionID int64
	UserID         int64
	Endpoint       string
	P256dh         string
	Auth           string
	Payload        map[string]any
	Urgency        string
	TTL            int
	ChatKey        *string
	MessageID      *int64
	NotificationID *int64
	Attempt        int
	Seq            uint64
	QueuedAt       time.Time
}

type NotificationRecipient struct {
	NotificationID int64
	UserID         int64
}

type subRecord struct {
	id       int64
	userID   int64
	endpoint string
	p256dh   string
	auth     string
}

type Service struct {
	db         *dbx.DB
	cfg        config.Config
	httpClient *http.Client

	mu          sync.Mutex
	queue       chan Job
	latestByKey map[string]uint64
	lastUsed    map[int64]time.Time
	seq         uint64
	started     bool
	stopCh      chan struct{}
}

func New(db *dbx.DB, cfg config.Config) *Service {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	s := &Service{
		db:          db,
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 12 * time.Second, Transport: tr},
		queue:       make(chan Job, 2048),
		latestByKey: make(map[string]uint64),
		lastUsed:    make(map[int64]time.Time),
		stopCh:      make(chan struct{}),
	}
	return s
}

func (s *Service) StartWorker() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()

	for i := 0; i < pushWorkers; i++ {
		go func() {
			for {
				select {
				case <-s.stopCh:
					return
				case job := <-s.queue:
					s.processJob(job)
				}
			}
		}()
	}
}

func (s *Service) StopWorker() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stopCh)
	s.mu.Unlock()
}

func (s *Service) IsConfigured() bool {
	return s.cfg.VAPIDPublicKey != "" && s.cfg.VAPIDPrivateKey != ""
}

func (s *Service) HandleSubscribe(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	m, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	endpoint := strings.TrimSpace(strVal(m["endpoint"]))
	parsed, parseErr := url.ParseRequestURI(endpoint)
	if endpoint == "" || !strings.HasPrefix(endpoint, "https://") || parseErr != nil || parsed.Host == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("endpoint", "api.push.endpoint_invalid"))
		return
	}

	keys, _ := m["keys"].(map[string]any)
	p256dh := strings.TrimSpace(strVal(keys["p256dh"]))
	auth := strings.TrimSpace(strVal(keys["auth"]))
	pBytes, _ := b64uDecode(p256dh)
	aBytes, _ := b64uDecode(auth)
	if p256dh == "" || auth == "" || len(pBytes) != 65 || len(aBytes) < 12 {
		httpx.HandleErr(w, r, httpx.ErrValidation("keys", "api.push.keys_invalid"))
		return
	}

	device := support.TruncateRunes(strings.TrimSpace(strVal(m["device"])), 120)
	if device == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("device", "api.push.device_required"))
		return
	}

	sum := sha256.Sum256([]byte(endpoint))
	hashHex := hex.EncodeToString(sum[:])
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var sid *string
	if cur := httpx.SessionIDFrom(r.Context()); cur != "" {
		sid = &cur
	}

	var id int64
	err = s.db.QueryRowContext(r.Context(), `SELECT id FROM push_subscriptions WHERE endpoint_hash = ?`, hashHex).Scan(&id)
	if err == nil {
		_, err = s.db.ExecContext(r.Context(), `
			UPDATE push_subscriptions SET user_id = ?, session_id = ?, endpoint = ?, p256dh = ?, auth = ?, device = ?, last_used_at = ?
			WHERE id = ?
		`, u.ID, sid, endpoint, p256dh, auth, device, nowStr, id)
	} else if err == sql.ErrNoRows {
		res, insErr := s.db.ExecContext(r.Context(), `
			INSERT INTO push_subscriptions (user_id, session_id, endpoint_hash, endpoint, p256dh, auth, device, created_at, last_used_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, u.ID, sid, hashHex, endpoint, p256dh, auth, device, nowStr, nowStr)
		if insErr != nil {
			httpx.HandleErr(w, r, insErr)
			return
		}
		id, _ = res.LastInsertId()
		err = nil
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{"id": id, "device": device})
}

func (s *Service) HandleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	m, _ := httpx.DecodeMap(r)
	endpoint := strings.TrimSpace(strVal(m["endpoint"]))
	if endpoint != "" {
		sum := sha256.Sum256([]byte(endpoint))
		hashHex := hex.EncodeToString(sum[:])
		_, _ = s.db.ExecContext(r.Context(), `DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint_hash = ?`, u.ID, hashHex)
	}
	httpx.Success(w, nil)
}

func (s *Service) DeleteForSessions(ctx context.Context, sessionIDs []string) {
	if len(sessionIDs) == 0 {
		return
	}
	ph := dbx.Placeholders(len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, sid := range sessionIDs {
		args[i] = sid
	}
	_, _ = s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM push_subscriptions WHERE session_id IN (%s)`, ph), args...)
}

func (s *Service) PruneStale(ctx context.Context) int64 {
	cutoff := dbx.FormatDBTime(time.Now().UTC().AddDate(0, 0, -StaleDays))
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM push_subscriptions
		WHERE last_used_at < ? OR (last_used_at IS NULL AND created_at < ?)
	`, cutoff, cutoff)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func (s *Service) QueueForNotification(ctx context.Context, notifID, recipientID int64, kind string, actorID, projectID, taskID, announcementID, bugID, teamID *int64, chatKey *string, data map[string]any) {
	s.QueueForNotificationsBatch(ctx, []NotificationRecipient{{NotificationID: notifID, UserID: recipientID}}, kind, actorID, projectID, taskID, announcementID, bugID, teamID, chatKey, data)
}

func (s *Service) QueueForNotificationsBatch(ctx context.Context, recipients []NotificationRecipient, kind string, actorID, projectID, taskID, announcementID, bugID, teamID *int64, chatKey *string, data map[string]any) {
	if !s.IsConfigured() || len(recipients) == 0 {
		return
	}
	run := func(qCtx context.Context) {
		userIDs := make([]int64, 0, len(recipients))
		seen := make(map[int64]bool, len(recipients))
		for _, r := range recipients {
			if r.UserID > 0 && !seen[r.UserID] {
				seen[r.UserID] = true
				userIDs = append(userIDs, r.UserID)
			}
		}
		if len(userIDs) == 0 {
			return
		}
		subsByUser := s.subsByUsers(qCtx, userIDs)
		if len(subsByUser) == 0 {
			return
		}
		lookupIDs := make([]int64, 0, len(subsByUser)+1)
		for uid := range subsByUser {
			lookupIDs = append(lookupIDs, uid)
		}
		if actorID != nil && *actorID > 0 {
			lookupIDs = append(lookupIDs, *actorID)
		}
		usersMap, _ := store.LoadUsersByIDs(qCtx, s.db, s.cfg.AppURL, lookupIDs)
		var actor *store.User
		if actorID != nil {
			actor = usersMap[*actorID]
		}
		var chatKind, chatAr, chatEn string
		if chatKey != nil && *chatKey != "" {
			chatKind, chatAr, chatEn = s.resolveChatTitle(qCtx, *chatKey)
		}
		urgency := notificationUrgency(kind)
		for _, rec := range recipients {
			subs := subsByUser[rec.UserID]
			if len(subs) == 0 {
				continue
			}
			recipient := usersMap[rec.UserID]
			if recipient == nil || !recipient.Active {
				continue
			}
			payload := s.buildNotificationPayload(rec.NotificationID, recipient, kind, actor, projectID, taskID, announcementID, bugID, teamID, chatKey, chatKind, chatAr, chatEn, data)
			nid := rec.NotificationID
			for _, sub := range subs {
				s.enqueue(Job{
					SubscriptionID: sub.id,
					UserID:         sub.userID,
					Endpoint:       sub.endpoint,
					P256dh:         sub.p256dh,
					Auth:           sub.auth,
					Payload:        payload,
					Urgency:        urgency,
					TTL:            86400,
					ChatKey:        chatKey,
					NotificationID: &nid,
				})
			}
		}
	}

	if !s.db.IsSQLite() {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run(bgCtx)
		}()
		return
	}
	run(ctx)
}

func (s *Service) QueueForChatMessage(ctx context.Context, messageID int64, body string, sender *store.User, chatKey string, memberIDs []int64) {
	if !s.IsConfigured() {
		return
	}
	var recipients []int64
	seen := map[int64]bool{}
	for _, id := range memberIDs {
		if id != sender.ID && !seen[id] {
			seen[id] = true
			recipients = append(recipients, id)
		}
	}
	if len(recipients) == 0 {
		return
	}

	run := func(qCtx context.Context) {
		subsByUser := s.subsByUsers(qCtx, recipients)
		if len(subsByUser) == 0 {
			return
		}
		subUserIDs := make([]int64, 0, len(subsByUser))
		for uid := range subsByUser {
			subUserIDs = append(subUserIDs, uid)
		}
		usersMap, _ := store.LoadUsersByIDs(qCtx, s.db, s.cfg.AppURL, subUserIDs)
		chatKind, titleAr, titleEn := s.resolveChatTitle(qCtx, chatKey)
		msgID := messageID
		ck := chatKey
		for uid, subs := range subsByUser {
			rec := usersMap[uid]
			if rec == nil || !rec.Active {
				continue
			}
			payload := buildChatPayload(body, sender, rec, chatKey, chatKind, titleAr, titleEn)
			for _, sub := range subs {
				s.enqueue(Job{
					SubscriptionID: sub.id,
					UserID:         sub.userID,
					Endpoint:       sub.endpoint,
					P256dh:         sub.p256dh,
					Auth:           sub.auth,
					Payload:        payload,
					Urgency:        "high",
					TTL:            86400,
					ChatKey:        &ck,
					MessageID:      &msgID,
				})
			}
		}
	}

	if !s.db.IsSQLite() {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run(bgCtx)
		}()
		return
	}
	run(ctx)
}

func (s *Service) enqueue(j Job) {
	if j.QueuedAt.IsZero() {
		j.QueuedAt = time.Now().UTC()
	}
	tag := strVal(j.Payload["tag"])
	if tag != "" {
		key := fmt.Sprintf("%d|%s", j.SubscriptionID, tag)
		s.mu.Lock()
		s.seq++
		j.Seq = s.seq
		s.latestByKey[key] = j.Seq
		s.mu.Unlock()
	}

	select {
	case s.queue <- j:
	default:
		// Channel full; drop non-blockingly so API request latency never stalls.
	}
}

func (s *Service) processJob(j Job) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Latest-only unread coalescing: if a newer push for the same (subscription, tag) is queued, skip this one.
	tag := strVal(j.Payload["tag"])
	if tag != "" && j.Seq > 0 {
		key := fmt.Sprintf("%d|%s", j.SubscriptionID, tag)
		s.mu.Lock()
		latest := s.latestByKey[key]
		s.mu.Unlock()
		if latest > j.Seq {
			return
		}
	}

	subUserID, endpoint, p256dh, auth := j.UserID, j.Endpoint, j.P256dh, j.Auth
	if endpoint == "" || p256dh == "" || auth == "" {
		err := s.db.QueryRowContext(ctx, `SELECT user_id, endpoint, p256dh, auth FROM push_subscriptions WHERE id = ?`, j.SubscriptionID).
			Scan(&subUserID, &endpoint, &p256dh, &auth)
		if err != nil {
			return
		}
	}

	// Skip extra round-trip DB checks on fresh immediate pushes; re-verify on retries or delayed queue items.
	if j.Attempt > 0 || time.Since(j.QueuedAt) >= 3*time.Second {
		if j.ChatKey != nil && j.MessageID != nil {
			var exists int
			_ = s.db.QueryRowContext(ctx, `
				SELECT 1 FROM messages WHERE chat_key = ? AND id > ? AND author_id != ? LIMIT 1
			`, *j.ChatKey, *j.MessageID, subUserID).Scan(&exists)
			if exists == 1 {
				return
			}
			var readExists int
			_ = s.db.QueryRowContext(ctx, `
				SELECT 1 FROM chat_reads WHERE chat_key = ? AND user_id = ? AND last_message_id >= ? LIMIT 1
			`, *j.ChatKey, subUserID, *j.MessageID).Scan(&readExists)
			if readExists == 1 {
				return
			}
		}
		if j.NotificationID != nil {
			var stillUnread int
			_ = s.db.QueryRowContext(ctx, `SELECT 1 FROM notifications WHERE id = ? AND `+"`read`"+` = 0 LIMIT 1`, *j.NotificationID).Scan(&stillUnread)
			if stillUnread != 1 {
				return
			}
		}
	}

	raw, err := json.Marshal(j.Payload)
	if err != nil {
		return
	}
	if len(raw) > MaxPayloadBytes {
		j.Payload["body"] = support.TruncateRunes(strVal(j.Payload["body"]), 120)
		raw, _ = json.Marshal(j.Payload)
	}

	sub := &webpush.Subscription{
		Endpoint: endpoint,
		Keys: webpush.Keys{
			P256dh: p256dh,
			Auth:   auth,
		},
	}
	resp, err := webpush.SendNotificationWithContext(ctx, raw, sub, &webpush.Options{
		HTTPClient:      s.httpClient,
		Subscriber:      strings.TrimPrefix(s.cfg.VAPIDSubject, "mailto:"),
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
		TTL:             j.TTL,
		Urgency:         webpush.Urgency(j.Urgency),
	})
	if err != nil {
		log.Printf("[WebPush] delivery error sub=%d: %v", j.SubscriptionID, err)
		if j.Attempt < 2 {
			j.Attempt++
			go func(retryJob Job) {
				time.Sleep(60 * time.Second)
				s.enqueue(retryJob)
			}(j)
		}
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.touchSubscriptionAsync(j.SubscriptionID)
		return
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, j.SubscriptionID)
		return
	}
	if resp.StatusCode >= 500 && j.Attempt < 2 {
		j.Attempt++
		go func(retryJob Job) {
			time.Sleep(60 * time.Second)
			s.enqueue(retryJob)
		}(j)
	}
}

func (s *Service) touchSubscriptionAsync(subID int64) {
	now := time.Now().UTC()
	s.mu.Lock()
	prev, ok := s.lastUsed[subID]
	if ok && now.Sub(prev) < 10*time.Minute {
		s.mu.Unlock()
		return
	}
	if len(s.lastUsed) > 4000 {
		s.lastUsed = make(map[int64]time.Time)
	}
	s.lastUsed[subID] = now
	s.mu.Unlock()

	if !s.db.IsSQLite() {
		nowStr := dbx.FormatDBTime(now)
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.db.ExecContext(bgCtx, `UPDATE push_subscriptions SET last_used_at = ? WHERE id = ?`, nowStr, subID)
		}()
	}
}

func (s *Service) subsByUsers(ctx context.Context, userIDs []int64) map[int64][]subRecord {
	out := map[int64][]subRecord{}
	if len(userIDs) == 0 {
		return out
	}
	ph, args := dbx.InInt64(userIDs)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, user_id, endpoint, p256dh, auth FROM push_subscriptions WHERE user_id IN (%s)`, ph), args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var sr subRecord
		if rows.Scan(&sr.id, &sr.userID, &sr.endpoint, &sr.p256dh, &sr.auth) == nil {
			out[sr.userID] = append(out[sr.userID], sr)
		}
	}
	return out
}

func notificationUrgency(kind string) string {
	switch kind {
	case "birthday_soon", "birthday_today", "task_due_soon":
		return "normal"
	default:
		return "high"
	}
}

func buildChatPayload(body string, sender, recipient *store.User, chatKey, chatKind, titleAr, titleEn string) map[string]any {
	lang := "ar"
	dir := "rtl"
	if recipient.PreferredLocale == "en" {
		lang = "en"
		dir = "ltr"
	}
	senderName := sender.EffectiveName(lang)
	chatTitle := titleAr
	if lang == "en" && titleEn != "" {
		chatTitle = titleEn
	}
	title := senderName
	if chatKind != "direct" && chatTitle != "" {
		title = senderName + " · " + chatTitle
	}
	trimmed := strings.TrimSpace(body)
	text := support.TruncateRunes(trimmed, 160)
	if text == "" {
		if lang == "en" {
			text = "📎 Sent an attachment"
		} else {
			text = "📎 أرسل مرفقاً"
		}
	}
	out := map[string]any{
		"title":     support.TruncateRunes(title, 90),
		"body":      text,
		"url":       "/chat/" + chatKey,
		"tag":       "chat-" + chatKey,
		"lang":      lang,
		"dir":       dir,
		"timestamp": time.Now().UnixMilli(),
		"vibrate":   []int{120, 40, 120},
	}
	if sender.AvatarURL != nil && strings.HasPrefix(*sender.AvatarURL, "https://") {
		out["icon"] = *sender.AvatarURL
	}
	return out
}

func (s *Service) resolveChatTitle(ctx context.Context, chatKey string) (kind, ar, en string) {
	parts := strings.SplitN(chatKey, "-", 2)
	if len(parts) != 2 {
		return "direct", "", ""
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "direct", "", ""
	}
	switch parts[0] {
	case "p":
		if p, perr := access.LoadProject(ctx, s.db, id); perr == nil && p != nil {
			ne := ""
			if p.NameEn != nil {
				ne = *p.NameEn
			}
			return "project", p.Name, ne
		}
		var ne sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM projects WHERE id = ?`, id).Scan(&ar, &ne)
		return "project", ar, ne.String
	case "t":
		var ne sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM teams WHERE id = ?`, id).Scan(&ar, &ne)
		return "team", ar, ne.String
	case "g":
		_ = s.db.QueryRowContext(ctx, `SELECT name FROM chat_groups WHERE id = ?`, id).Scan(&ar)
		return "group", ar, ar
	default:
		return "direct", "", ""
	}
}

func (s *Service) buildNotificationPayload(id int64, rec *store.User, kind string, actor *store.User, projectID, taskID, annID, bugID, teamID *int64, chatKey *string, chatKind, chatAr, chatEn string, d map[string]any) map[string]any {
	lang := "ar"
	dir := "rtl"
	if rec.PreferredLocale == "en" {
		lang = "en"
		dir = "ltr"
	}
	ar := lang == "ar"
	who := "أحدهم"
	if !ar {
		who = "Someone"
	}
	if actor != nil {
		who = actor.EffectiveName(lang)
	}

	title, body := notificationTitleAndBody(kind, d, who, chatKind, chatAr, chatEn, ar)
	navURL := notificationURL(rec, kind, d, projectID, taskID, annID, bugID, teamID, chatKey)
	tag := fmt.Sprintf("n-%d", id)
	if kind == "chat_mention" && chatKey != nil && *chatKey != "" {
		tag = "chat-" + *chatKey
	}

	out := map[string]any{
		"title":     support.TruncateRunes(title, 90),
		"body":      support.TruncateRunes(body, 240),
		"url":       navURL,
		"tag":       tag,
		"lang":      lang,
		"dir":       dir,
		"timestamp": time.Now().UnixMilli(),
	}
	if kind == "approval_requested" || kind == "account_locked" || (kind == "bug_reported" && strVal(d["severity"]) == "critical") {
		out["require_interaction"] = true
	}
	if actor != nil && actor.AvatarURL != nil && strings.HasPrefix(*actor.AvatarURL, "https://") {
		out["icon"] = *actor.AvatarURL
	}
	return out
}

func withProjectSuffix(text, project string) string {
	if project == "" {
		return text
	}
	if text == "" {
		return project
	}
	return text + " — " + project
}

func notificationTitleAndBody(kind string, d map[string]any, who, chatKind, chatAr, chatEn string, ar bool) (string, string) {
	taskCode := strVal(d["task_code"])
	taskTitle := strVal(d["task_title"])
	project := strVal(d["project_name"])
	if !ar && strVal(d["project_name_en"]) != "" {
		project = strVal(d["project_name_en"])
	}
	bugCode := strVal(d["bug_code"])
	bugTitle := strVal(d["bug_title"])
	note := strings.TrimSpace(strVal(d["note"]))

	taskSubject := taskCode
	if taskTitle != "" {
		taskSubject = support.TruncateRunes(taskTitle, 90)
	}
	bugSubject := bugCode
	if bugTitle != "" {
		bugSubject = support.TruncateRunes(bugTitle, 90)
	}

	switch kind {
	case "task_assigned":
		if ar {
			return fmt.Sprintf("مهمة جديدة · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s أسند إليك: %s", who, taskSubject), project)
		}
		return fmt.Sprintf("New task · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s assigned you: %s", who, taskSubject), project)
	case "task_status":
		st := strVal(d["status"])
		if !ar && strVal(d["status_en"]) != "" {
			st = strVal(d["status_en"])
		}
		if st == "" {
			st = "-"
		}
		b := fmt.Sprintf("%s: %s", who, taskSubject)
		if note != "" {
			b += " (" + support.TruncateRunes(note, 70) + ")"
		}
		return fmt.Sprintf("%s · %s", taskCode, st), withProjectSuffix(b, project)
	case "task_comment":
		if ar {
			b := fmt.Sprintf("%s علّق على %s", who, taskSubject)
			if note != "" {
				b = fmt.Sprintf("%s (%s): %s", who, taskCode, support.TruncateRunes(note, 110))
			}
			return fmt.Sprintf("تعليق جديد · %s", taskCode), b
		}
		b := fmt.Sprintf("%s commented on %s", who, taskSubject)
		if note != "" {
			b = fmt.Sprintf("%s (%s): %s", who, taskCode, support.TruncateRunes(note, 110))
		}
		return fmt.Sprintf("New comment · %s", taskCode), b
	case "task_due_changed":
		due := strVal(d["due"])
		if ar {
			if due != "" {
				return fmt.Sprintf("تعديل موعد · %s", taskCode), fmt.Sprintf("%s حدّد التسليم %s — %s", who, due, taskSubject)
			}
			return fmt.Sprintf("تعديل موعد · %s", taskCode), fmt.Sprintf("%s أزال موعد تسليم %s", who, taskSubject)
		}
		if due != "" {
			return fmt.Sprintf("Due date updated · %s", taskCode), fmt.Sprintf("%s set due to %s — %s", who, due, taskSubject)
		}
		return fmt.Sprintf("Due date cleared · %s", taskCode), fmt.Sprintf("%s removed due date — %s", who, taskSubject)
	case "task_due_soon":
		if ar {
			return fmt.Sprintf("موعد قريب · %s", taskCode), withProjectSuffix("اقترب موعد تسليم: "+taskSubject, project)
		}
		return fmt.Sprintf("Due soon · %s", taskCode), withProjectSuffix(taskSubject+" is due soon", project)
	case "task_overdue":
		if ar {
			return fmt.Sprintf("مهمة متأخرة · %s", taskCode), withProjectSuffix("تجاوزت موعد التسليم: "+taskSubject, project)
		}
		return fmt.Sprintf("Overdue task · %s", taskCode), withProjectSuffix(taskSubject+" is overdue", project)
	case "approval_requested":
		if ar {
			return fmt.Sprintf("طلب اعتماد · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s يطلب اعتماد إكمال: %s", who, taskSubject), project)
		}
		return fmt.Sprintf("Approval needed · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s requested approval for: %s", who, taskSubject), project)
	case "approval_decided":
		if strVal(d["decision"]) == "rejected" {
			b := fmt.Sprintf("%s أرجع: %s", who, taskSubject)
			if !ar {
				b = fmt.Sprintf("%s sent back: %s", who, taskSubject)
			}
			if note != "" {
				b += " — " + support.TruncateRunes(note, 80)
			}
			if ar {
				return fmt.Sprintf("أُعيدت للمراجعة · %s", taskCode), b
			}
			return fmt.Sprintf("Sent back · %s", taskCode), b
		}
		if ar {
			return fmt.Sprintf("تم الاعتماد · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s اعتمد إكمال: %s", who, taskSubject), project)
		}
		return fmt.Sprintf("Approved · %s", taskCode), withProjectSuffix(fmt.Sprintf("%s approved: %s", who, taskSubject), project)
	case "project_added":
		if ar {
			return fmt.Sprintf("مشروع جديد · %s", orDash(project)), fmt.Sprintf("%s أضافك إلى فريق المشروع", who)
		}
		return fmt.Sprintf("Project invite · %s", orDash(project)), fmt.Sprintf("%s added you to the project", who)
	case "project_pm":
		if ar {
			return orDash(project), fmt.Sprintf("%s حدّث مسؤول المشروع", who)
		}
		return orDash(project), fmt.Sprintf("%s updated the project manager", who)
	case "project_plan":
		if ar {
			return orDash(project), fmt.Sprintf("%s حدّث خطة ومواعيد المشروع", who)
		}
		return orDash(project), fmt.Sprintf("%s updated the project schedule", who)
	case "announcement":
		annTitle := strVal(d["title"])
		if ar {
			t := "إعلان جديد"
			if boolVal(d["important"]) {
				t = "إعلان هام"
			}
			return t, fmt.Sprintf("%s: %s", who, annTitle)
		}
		t := "Announcement"
		if boolVal(d["important"]) {
			t = "Important announcement"
		}
		return t, fmt.Sprintf("%s: %s", who, annTitle)
	case "bug_reported":
		sev := strVal(d["severity"])
		if ar {
			t := fmt.Sprintf("مشكلة جديدة · %s", bugCode)
			if sev == "critical" {
				t = fmt.Sprintf("مشكلة حرجة · %s", bugCode)
			}
			return t, withProjectSuffix(fmt.Sprintf("%s سجّل: %s", who, bugSubject), project)
		}
		t := fmt.Sprintf("New bug · %s", bugCode)
		if sev == "critical" {
			t = fmt.Sprintf("Critical bug · %s", bugCode)
		}
		return t, withProjectSuffix(fmt.Sprintf("%s reported: %s", who, bugSubject), project)
	case "bug_assigned":
		if ar {
			return fmt.Sprintf("مشكلة مسندة إليك · %s", bugCode), withProjectSuffix(fmt.Sprintf("%s أسند إليك: %s", who, bugSubject), project)
		}
		return fmt.Sprintf("Bug assigned · %s", bugCode), withProjectSuffix(fmt.Sprintf("%s assigned you: %s", who, bugSubject), project)
	case "bug_status":
		st := strVal(d["bug_status"])
		if ar {
			t := fmt.Sprintf("تحديث مشكلة · %s", bugCode)
			if st != "" {
				t = fmt.Sprintf("%s · %s", bugCode, st)
			}
			return t, withProjectSuffix(fmt.Sprintf("%s: %s", who, bugSubject), project)
		}
		t := fmt.Sprintf("Bug updated · %s", bugCode)
		if st != "" {
			t = fmt.Sprintf("%s · %s", bugCode, st)
		}
		return t, withProjectSuffix(fmt.Sprintf("%s: %s", who, bugSubject), project)
	case "bug_late":
		if ar {
			return fmt.Sprintf("مشكلة متأخرة · %s", bugCode), withProjectSuffix("تجاوزت موعد الإصلاح: "+bugSubject, project)
		}
		return fmt.Sprintf("Overdue bug · %s", bugCode), withProjectSuffix(bugSubject+" is past fix due date", project)
	case "chat_mention":
		chatName := chatAr
		if !ar && chatEn != "" {
			chatName = chatEn
		}
		if ar {
			t := "إشارة إليك (@)"
			if chatKind != "direct" && chatName != "" {
				t = fmt.Sprintf("إشارة إليك · %s", chatName)
			}
			b := who + " ذكرك في محادثة"
			if note != "" {
				b = fmt.Sprintf("%s: %s", who, support.TruncateRunes(note, 130))
			}
			return t, b
		}
		t := "Mentioned you (@)"
		if chatKind != "direct" && chatName != "" {
			t = fmt.Sprintf("Mentioned in %s", chatName)
		}
		b := who + " mentioned you"
		if note != "" {
			b = fmt.Sprintf("%s: %s", who, support.TruncateRunes(note, 130))
		}
		return t, b
	case "dependency_done":
		dep := orDash(note)
		if ar {
			return fmt.Sprintf("جاهزة للبدء · %s", taskCode), fmt.Sprintf("أُنجزت %s ويمكن متابعة: %s", dep, taskSubject)
		}
		return fmt.Sprintf("Unblocked · %s", taskCode), fmt.Sprintf("%s finished — %s is ready", dep, taskSubject)
	case "review_final":
		if ar {
			return "تقييم الأداء", who + " اعتمد تقييم أدائك النهائي"
		}
		return "Performance review", who + " finalized your review"
	case "team_joined":
		tname := orDash(note)
		if tname == "-" && strVal(d["team_name"]) != "" {
			tname = strVal(d["team_name"])
			if !ar && strVal(d["team_name_en"]) != "" {
				tname = strVal(d["team_name_en"])
			}
		}
		if ar {
			return fmt.Sprintf("انضمام لفريق · %s", tname), fmt.Sprintf("%s أضافك إلى «%s»", who, tname)
		}
		return fmt.Sprintf("Joined team · %s", tname), fmt.Sprintf("%s added you to \"%s\"", who, tname)
	case "account_locked":
		acc := orDash(note)
		if ar {
			return "تنبيه أمان · قفل حساب", fmt.Sprintf("تم قفل حساب %s بعد محاولات دخول خاطئة", acc)
		}
		return "Security alert · Account locked", fmt.Sprintf("%s's account locked after failed logins", acc)
	case "github_pr_merged":
		pr := orDash(note)
		if ar {
			return fmt.Sprintf("دمج GitHub · %s", taskCode), fmt.Sprintf("دُمج %s المرتبط بـ %s", pr, taskSubject)
		}
		return fmt.Sprintf("PR merged · %s", taskCode), fmt.Sprintf("%s merged for %s", pr, taskSubject)
	case "note_sent":
		if ar {
			return "ملاحظة متابعة جديدة", fmt.Sprintf("%s: %s", who, strVal(d["title"]))
		}
		return "New follow-up note", fmt.Sprintf("%s: %s", who, strVal(d["title"]))
	case "note_answered":
		if ar {
			return "تم الرد على الطلب", fmt.Sprintf("%s: %s", who, strVal(d["title"]))
		}
		return "Note answered", fmt.Sprintf("%s: %s", who, strVal(d["title"]))
	case "person_rated":
		score := orDash(fmt.Sprint(d["score"]))
		if ar {
			return fmt.Sprintf("تقييم سريع · ★ %s/5", score), fmt.Sprintf("%s قيّم أداءك (%s من 5)", who, score)
		}
		return fmt.Sprintf("Quick rating · ★ %s/5", score), fmt.Sprintf("%s rated you %s of 5", who, score)
	case "team_rated":
		tname := orDash(strVal(d["team_name"]))
		if !ar && strVal(d["team_name_en"]) != "" {
			tname = strVal(d["team_name_en"])
		}
		score := orDash(fmt.Sprint(d["score"]))
		if ar {
			return fmt.Sprintf("تقييم فريق · ★ %s/5", score), fmt.Sprintf("%s قيّم «%s» (%s من 5)", who, tname, score)
		}
		return fmt.Sprintf("Team rating · ★ %s/5", score), fmt.Sprintf("%s rated \"%s\" (%s of 5)", who, tname, score)
	case "birthday_soon", "birthday_today":
		name := orDash(strVal(d["person_name"]))
		if !ar && strVal(d["person_name_en"]) != "" {
			name = strVal(d["person_name_en"])
		}
		if kind == "birthday_today" {
			if ar {
				return "عيد ميلاد سعيد 🎂", "اليوم عيد ميلاد " + name
			}
			return "Happy Birthday 🎂", "Today is " + name + "'s birthday"
		}
		due := strVal(d["due"])
		loc := "en"
		if ar {
			loc = "ar"
		}
		dayLabel := ""
		if due != "" {
			dayLabel = support.FormatDayForLocale(due, loc)
		}
		if ar {
			return "عيد ميلاد قريب", fmt.Sprintf("عيد ميلاد %s بعد أسبوع (%s)", name, dayLabel)
		}
		return "Upcoming birthday", fmt.Sprintf("%s's birthday is in a week (%s)", name, dayLabel)
	default:
		if t := strVal(d["title"]); t != "" {
			if ar {
				return "متابعة العمل", t
			}
			return "FollowUP", t
		}
		if ar {
			return "متابعة العمل", "إشعار جديد"
		}
		return "FollowUP", "New notification"
	}
}

func boolVal(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func notificationURL(rec *store.User, kind string, d map[string]any, projectID, taskID, annID, bugID, teamID *int64, chatKey *string) string {
	if kind == "birthday_soon" || kind == "birthday_today" {
		if pid := fmt.Sprint(d["person_id"]); pid != "" && pid != "<nil>" && (rec.IsCEO() || rec.Role == "monitor") {
			return "/performance/" + pid
		}
		return "/dashboard#birthdays-title"
	}
	if taskID != nil {
		return fmt.Sprintf("/tasks/%d", *taskID)
	}
	if annID != nil {
		return fmt.Sprintf("/announcements/%d", *annID)
	}
	if bugID != nil {
		return fmt.Sprintf("/bugs/%d", *bugID)
	}
	if chatKey != nil && *chatKey != "" {
		return "/chat/" + *chatKey
	}
	if kind == "review_final" {
		return fmt.Sprintf("/performance/%d", rec.ID)
	}
	if kind == "note_sent" {
		switch strVal(d["queue"]) {
		case "monitor":
			return "/monitoring?show=requests"
		case "received":
			return "/requests?tab=received"
		default:
			return "/inbox"
		}
	}
	if kind == "note_answered" {
		if strVal(d["note_kind"]) == "request" {
			return "/requests"
		}
		return "/monitoring"
	}
	if kind == "account_locked" {
		return "/admin"
	}
	if kind == "person_rated" {
		return fmt.Sprintf("/performance/%d#ratings", rec.ID)
	}
	if kind == "team_rated" && teamID != nil {
		return fmt.Sprintf("/teams/%d", *teamID)
	}
	if projectID != nil {
		return fmt.Sprintf("/projects/%d", *projectID)
	}
	return "/notifications"
}

func orDash(s string) string {
	if s == "" || s == "<nil>" {
		return "-"
	}
	return s
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func b64uDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
