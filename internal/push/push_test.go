package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/store"
)

func TestNotificationAndChatPayloads(t *testing.T) {
	svc := &Service{cfg: config.Config{AppURL: "https://followapi.ama8.uk"}}
	avatar := "https://followapi.ama8.uk/api/v1/avatars/2/abc.jpg"
	nameEn := "Ahmed Kareem"
	actor := &store.User{ID: 2, Name: "أحمد كريم", NameEn: &nameEn, AvatarURL: &avatar}
	recAr := &store.User{ID: 5, Name: "سارة", PreferredLocale: "ar", Active: true}
	recEn := &store.User{ID: 6, Name: "Sara", PreferredLocale: "en", Active: true}

	// Task assigned (Arabic)
	pAr := svc.buildNotificationPayload(10, recAr, "task_assigned", actor, ptrI64(1), ptrI64(118), nil, nil, nil, nil, "", "", "", map[string]any{
		"task_code":       "TSK-118",
		"task_title":      "بناء واجهة الدفع",
		"project_name":    "بوابة العملاء",
		"project_name_en": "Client Portal",
	})
	if !strings.Contains(pAr["title"].(string), "مهمة جديدة · TSK-118") {
		t.Fatalf("unexpected task_assigned title: %v", pAr["title"])
	}
	if !strings.Contains(pAr["body"].(string), "بناء واجهة الدفع") || !strings.Contains(pAr["body"].(string), "بوابة العملاء") {
		t.Fatalf("unexpected task_assigned body: %v", pAr["body"])
	}
	if pAr["url"] != "/tasks/118" || pAr["tag"] != "n-10" || pAr["icon"] != avatar {
		t.Fatalf("unexpected task_assigned metadata: %v", pAr)
	}

	// Task comment (English with comment preview)
	pEn := svc.buildNotificationPayload(11, recEn, "task_comment", actor, ptrI64(1), ptrI64(118), nil, nil, nil, nil, "", "", "", map[string]any{
		"task_code":  "TSK-118",
		"task_title": "Checkout API",
		"note":       "Fixed edge case on retries",
	})
	if pEn["title"] != "New comment · TSK-118" || !strings.Contains(pEn["body"].(string), "Fixed edge case on retries") {
		t.Fatalf("unexpected task_comment payload: %v", pEn)
	}

	// Critical bug sets require_interaction
	pBug := svc.buildNotificationPayload(12, recEn, "bug_reported", actor, ptrI64(1), nil, nil, ptrI64(44), nil, nil, "", "", "", map[string]any{
		"bug_code":        "BUG-44",
		"bug_title":       "Crash on login",
		"severity":        "critical",
		"project_name_en": "Mobile App",
	})
	if pBug["require_interaction"] != true || !strings.Contains(pBug["title"].(string), "Critical bug · BUG-44") {
		t.Fatalf("unexpected critical bug payload: %v", pBug)
	}

	// Chat mention with snippet and chat title
	ck := "g-3"
	pMent := svc.buildNotificationPayload(13, recAr, "chat_mention", actor, nil, nil, nil, nil, nil, &ck, "group", "فريق التطوير", "Dev", map[string]any{
		"note": "راجع التقرير لو سمحت",
	})
	if pMent["tag"] != "chat-g-3" || !strings.Contains(pMent["title"].(string), "فريق التطوير") || !strings.Contains(pMent["body"].(string), "راجع التقرير") {
		t.Fatalf("unexpected chat_mention payload: %v", pMent)
	}

	// Chat message payload empty body falls back cleanly
	pChat := buildChatPayload("", actor, recEn, "p-7", "project", "بوابة", "Portal")
	if pChat["title"] != "Ahmed Kareem · Portal" || !strings.Contains(pChat["body"].(string), "Sent an attachment") {
		t.Fatalf("unexpected chat payload: %v", pChat)
	}
}

func TestBatchQueueAndConcurrentDeliveryAndGoneCleanup(t *testing.T) {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatalf("vapid gen: %v", err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.Header.Get("Urgency") != "high" {
			t.Errorf("expected Urgency=high, got %q", r.Header.Get("Urgency"))
		}
		if n == 2 {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	cfg := config.Config{
		DBConnection:    "sqlite",
		AppURL:          "https://followapi.ama8.uk",
		VAPIDSubject:    "mailto:test@ama8.uk",
		VAPIDPublicKey:  pub,
		VAPIDPrivateKey: priv,
	}
	db, err := dbx.Open(cfg)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if err := db.BootstrapSQLite(context.Background()); err != nil {
		t.Fatalf("bootstrap sqlite: %v", err)
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO users (id, name, email, role, preferred_locale, title, active, password, created_at, updated_at)
		VALUES (1, 'Actor', 'a@x.test', 'pm', 'ar', 'PM', 1, 'x', ?, ?),
		       (2, 'User Two', 'u2@x.test', 'employee', 'ar', 'Dev', 1, 'x', ?, ?),
		       (3, 'User Three', 'u3@x.test', 'employee', 'en', 'Dev', 1, 'x', ?, ?)
	`, nowStr, nowStr, nowStr, nowStr, nowStr, nowStr)
	if err != nil {
		t.Fatalf("seed users: %v", err)
	}

	// Generate valid P-256 public key + 16-byte auth secret per subscription
	makeKeys := func() (string, string) {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		auth := make([]byte, 16)
		_, _ = rand.Read(auth)
		return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(auth)
	}
	p1, a1 := makeKeys()
	p2, a2 := makeKeys()

	_, err = db.ExecContext(context.Background(), `
		INSERT INTO push_subscriptions (id, user_id, endpoint_hash, endpoint, p256dh, auth, device, created_at, last_used_at)
		VALUES (10, 2, 'h1', ?, ?, ?, 'Android', ?, ?),
		       (11, 3, 'h2', ?, ?, ?, 'iOS', ?, ?)
	`, srv.URL+"/sub1", p1, a1, nowStr, nowStr, srv.URL+"/sub2", p2, a2, nowStr, nowStr)
	if err != nil {
		t.Fatalf("seed subs: %v", err)
	}
	_, _ = db.ExecContext(context.Background(), `
		INSERT INTO notifications (id, user_id, kind, actor_id, data, at, `+"`read`"+`)
		VALUES (101, 2, 'task_assigned', 1, '{}', ?, 0),
		       (102, 3, 'task_assigned', 1, '{}', ?, 0)
	`, nowStr, nowStr)

	svc := New(db, cfg)
	svc.StartWorker()
	defer svc.StopWorker()

	svc.QueueForNotificationsBatch(
		context.Background(),
		[]NotificationRecipient{{NotificationID: 101, UserID: 2}, {NotificationID: 102, UserID: 3}},
		"task_assigned",
		ptrI64(1), nil, ptrI64(50), nil, nil, nil, nil,
		map[string]any{"task_code": "TSK-50", "task_title": "Ship push"},
	)

	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 push HTTP deliveries, got %d", hits.Load())
	}

	// Wait briefly for 410 Gone subscription cleanup
	time.Sleep(80 * time.Millisecond)
	var remaining int
	_ = db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM push_subscriptions`).Scan(&remaining)
	if remaining != 1 {
		t.Fatalf("expected 410 Gone subscription to be pruned leaving 1, got %d", remaining)
	}
}

func ptrI64(v int64) *int64 { return &v }
