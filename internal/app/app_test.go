package app_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"followapi/internal/app"
	"followapi/internal/auth"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/notifications"
	"followapi/internal/store"
)

type testEnv struct {
	t   *testing.T
	cfg config.Config
	db  *dbx.DB
	app *app.App
	srv *httptest.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	cfg := config.Config{
		AppName:              "FollowUP-Test",
		AppEnv:               "testing",
		AppKey:               "test-secret-key-for-signed-downloads-32bytes",
		AppURL:               "http://127.0.0.1",
		Port:                 "0",
		WorkingTimezone:      "Asia/Baghdad",
		SwaggerEnabled:       true,
		DBConnection:         "sqlite",
		DBDatabase:           ":memory:",
		AllowedOrigins:       []string{"http://localhost:3000"},
		StorageRoot:          t.TempDir(),
		GithubWebhookSecret:  "gh-secret-123",
		RateLimitLoginPerMin: 1000,
		RateLimitAPIPerMin:   5000,
	}
	db, err := dbx.Open(cfg)
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.BootstrapSQLite(context.Background()); err != nil {
		t.Fatalf("bootstrap sqlite failed: %v", err)
	}
	a := app.New(cfg, db)
	a.Push.StartWorker()
	srv := httptest.NewServer(a.Router)
	cfg.AppURL = srv.URL

	// Rebuild app with live test server URL for avatar/download signed URLs
	a.Push.StopWorker()
	a = app.New(cfg, db)
	a.Push.StartWorker()
	srv.Config.Handler = a.Router

	t.Cleanup(func() {
		a.Push.StopWorker()
		srv.Close()
		_ = db.Close()
	})
	return &testEnv{t: t, cfg: cfg, db: db, app: a, srv: srv}
}

func (e *testEnv) seedUser(name, email, role, password string, teamID *int64, birthDate *string) int64 {
	e.t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		e.t.Fatalf("hash failed: %v", err)
	}
	now := dbx.FormatDBTime(time.Now().UTC())
	res, err := e.db.ExecContext(context.Background(), `
		INSERT INTO users (name, email, role, preferred_locale, title, team_id, birth_date, active, password, created_at, updated_at)
		VALUES (?, ?, ?, 'ar', ?, ?, ?, 1, ?, ?, ?)
	`, name, email, role, role, teamID, birthDate, hash, now, now)
	if err != nil {
		e.t.Fatalf("seed user %s failed: %v", email, err)
	}
	id, _ := res.LastInsertId()
	_, _ = e.db.ExecContext(context.Background(), `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, id, now, now)
	return id
}

func (e *testEnv) seedTeam(name string, leadID *int64) int64 {
	e.t.Helper()
	now := dbx.FormatDBTime(time.Now().UTC())
	res, err := e.db.ExecContext(context.Background(), `INSERT INTO teams (name, lead_id, created_at, updated_at) VALUES (?, ?, ?, ?)`, name, leadID, now, now)
	if err != nil {
		e.t.Fatalf("seed team failed: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

type apiResp struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
	Errors  map[string]any  `json:"errors"`
}

func (e *testEnv) req(method, path, token string, body any) (int, apiResp, []byte) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("http do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed apiResp
	_ = json.Unmarshal(raw, &parsed)
	return resp.StatusCode, parsed, raw
}

func (e *testEnv) login(identifier, password string) string {
	e.t.Helper()
	code, resp, raw := e.req("POST", "/api/v1/auth/login", "", map[string]any{
		"identifier": identifier,
		"password":   password,
	})
	if code != http.StatusOK || !resp.Success {
		e.t.Fatalf("login %s failed (%d): %s", identifier, code, string(raw))
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(resp.Data, &out)
	if out.Token == "" {
		e.t.Fatalf("empty token on login for %s", identifier)
	}
	return out.Token
}

func TestDocsAndHealthAndLaravel2yBcryptAuth(t *testing.T) {
	e := newTestEnv(t)

	for _, path := range []string{"/", "/up", "/docs", "/docs/openapi.yaml"} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s expected 200, got %d", path, resp.StatusCode)
		}
	}

	// Simulate PHP Laravel $2y$ bcrypt hash stored in MySQL users table
	rawHash, err := bcrypt.GenerateFromPassword([]byte("SecretPass#123"), 10)
	if err != nil {
		t.Fatal(err)
	}
	php2y := "$2y$" + strings.TrimPrefix(string(rawHash), "$2a$")
	now := dbx.FormatDBTime(time.Now().UTC())
	res, err := e.db.ExecContext(context.Background(), `
		INSERT INTO users (name, email, role, preferred_locale, title, active, password, created_at, updated_at)
		VALUES ('CEO Laravel', 'ceo@example.com', 'ceo', 'ar', 'CEO', 1, ?, ?, ?)
	`, php2y, now, now)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	_, _ = e.db.ExecContext(context.Background(), `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, uid, now, now)

	// Login must work seamlessly with $2y$ hash
	tok := e.login("ceo@example.com", "SecretPass#123")

	code, meResp, _ := e.req("GET", "/api/v1/me", tok, nil)
	if code != 200 || !meResp.Success {
		t.Fatalf("GET /me expected 200, got %d", code)
	}
	var me map[string]any
	_ = json.Unmarshal(meResp.Data, &me)
	// Confirm explicit null keys per UserResource
	if _, hasNull := me["team_id"]; !hasNull {
		t.Fatalf("expected team_id key in /me user JSON")
	}

	// Register push subscription linked to session, then logout -> subscription removed!
	p256 := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x04}, 65))
	authKey := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x05}, 16))
	code, _, raw := e.req("POST", "/api/v1/me/push-subscriptions", tok, map[string]any{
		"endpoint": "https://fcm.googleapis.com/fcm/send/abc-123",
		"keys":     map[string]any{"p256dh": p256, "auth": authKey},
		"device":   "Pixel 9",
	})
	if code != 200 {
		t.Fatalf("push subscribe failed (%d): %s", code, string(raw))
	}
	var subCnt int
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, uid).Scan(&subCnt)
	if subCnt != 1 {
		t.Fatalf("expected 1 push subscription, got %d", subCnt)
	}

	code, _, _ = e.req("POST", "/api/v1/auth/logout", tok, nil)
	if code != 200 {
		t.Fatalf("logout failed (%d)", code)
	}
	_ = e.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM push_subscriptions WHERE user_id = ?`, uid).Scan(&subCnt)
	if subCnt != 0 {
		t.Fatalf("expected 0 push subscriptions after logout, got %d", subCnt)
	}
	code, _, _ = e.req("GET", "/api/v1/auth/me", tok, nil)
	if code != 401 {
		t.Fatalf("expected 401 after logout, got %d", code)
	}
}

func TestLockoutUnlockAdvancedSettingsAndReadOnlyMode(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser("CEO", "ceo@example.com", "ceo", "Password#1234", nil, nil)
	e.seedUser("Admin", "admin@example.com", "admin", "Password#1234", nil, nil)
	empID := e.seedUser("Emp", "emp@example.com", "employee", "Password#1234", nil, nil)

	adminTok := e.login("admin@example.com", "Password#1234")
	ceoTok := e.login("ceo@example.com", "Password#1234")
	empTok := e.login("emp@example.com", "Password#1234")

	// Default lock_after = 5: 5th attempt triggers lock (returns 422), 6th attempt returns 423 account_locked
	for i := 0; i < 6; i++ {
		code, _, _ := e.req("POST", "/api/v1/auth/login", "", map[string]any{
			"identifier": "emp@example.com",
			"password":   "wrong-pw",
		})
		if i < 5 && code != 422 {
			t.Fatalf("failed login #%d expected 422, got %d", i+1, code)
		}
		if i == 5 && code != 429 {
			t.Fatalf("6th failed login expected 429 lockout, got %d", code)
		}
	}

	// Admin unlocks emp
	code, _, raw := e.req("POST", fmt.Sprintf("/api/v1/admin/users/%d/unlock", empID), adminTok, nil)
	if code != 200 {
		t.Fatalf("unlock failed (%d): %s", code, string(raw))
	}
	_ = e.login("emp@example.com", "Password#1234")

	// CEO saves custom role & turns on read-only mode
	code, advGet, _ := e.req("GET", "/api/v1/admin/advanced", ceoTok, nil)
	if code != 200 {
		t.Fatalf("GET advanced failed: %d", code)
	}
	var advData map[string]any
	_ = json.Unmarshal(advGet.Data, &advData)
	rules := advData["rules"].(map[string]any)
	rules["read_only"] = map[string]any{"on": true, "message": "Maintenance window"}
	rules["roles"] = []any{
		map[string]any{
			"key":     "lead-helper",
			"base":    "lead",
			"color":   "progress",
			"name":    "مساعد قائد",
			"name_en": "Lead Helper",
			"actions": []string{"approve_done", "see_reports"},
			"pages":   []string{"projects"},
		},
	}

	code, _, raw = e.req("PUT", "/api/v1/admin/advanced", ceoTok, map[string]any{
		"rules": rules,
	})
	if code != 200 {
		t.Fatalf("PUT advanced failed (%d): %s", code, string(raw))
	}

	// Non-CEO write must return 423 in read-only mode
	code, _, _ = e.req("POST", "/api/v1/projects", adminTok, map[string]any{"name": "Blocked"})
	if code != 423 {
		t.Fatalf("expected 423 in read-only mode for admin POST, got %d", code)
	}
	// Bookkeeping route must bypass 423!
	code, _, _ = e.req("POST", "/api/v1/notifications/read-all", empTok, nil)
	if code != 200 {
		t.Fatalf("expected 200 for bookkeeping route during read-only, got %d", code)
	}
	// CEO still allowed in read-only mode
	code, _, _ = e.req("GET", "/api/v1/admin/advanced/history", ceoTok, nil)
	if code != 200 {
		t.Fatalf("expected 200 for advanced history, got %d", code)
	}
}

func TestProjectsTasksDependenciesBugsAndAttachments(t *testing.T) {
	e := newTestEnv(t)
	e.seedUser("CEO", "ceo@example.com", "ceo", "Password#1234", nil, nil)
	pmID := e.seedUser("PM", "pm@example.com", "pm", "Password#1234", nil, nil)
	empID := e.seedUser("Dev", "dev@example.com", "employee", "Password#1234", nil, nil)

	pmTok := e.login("pm@example.com", "Password#1234")
	empTok := e.login("dev@example.com", "Password#1234")

	// Create project
	code, pResp, raw := e.req("POST", "/api/v1/projects", pmTok, map[string]any{
		"name":        "FollowUP Go",
		"description": "Rewrite backend",
		"pm_id":       pmID,
		"start":       "2026-01-01",
		"end":         "2026-12-31",
		"priority":    "high",
		"member_ids":  []int64{empID},
	})
	if code != 201 {
		t.Fatalf("create project failed (%d): %s", code, string(raw))
	}
	var proj map[string]any
	_ = json.Unmarshal(pResp.Data, &proj)
	pid := int64(proj["id"].(float64))

	// Create 2 tasks
	code, t1Resp, raw := e.req("POST", fmt.Sprintf("/api/v1/projects/%d/tasks", pid), pmTok, map[string]any{
		"title":       "Setup DB",
		"description": "Bootstrap schema",
		"assignee_id": empID,
		"priority":    "high",
	})
	if code != 201 {
		t.Fatalf("create task1 failed (%d): %s", code, string(raw))
	}
	var t1, t2 map[string]any
	_ = json.Unmarshal(t1Resp.Data, &t1)
	t1ID := int64(t1["id"].(float64))

	code, t2Resp, _ := e.req("POST", fmt.Sprintf("/api/v1/projects/%d/tasks", pid), pmTok, map[string]any{
		"title":       "Wire Router",
		"description": "Connect chi handlers",
		"assignee_id": empID,
		"priority":    "medium",
	})
	if code != 201 {
		t.Fatalf("create task2 failed: %d", code)
	}
	_ = json.Unmarshal(t2Resp.Data, &t2)
	t2ID := int64(t2["id"].(float64))

	// T2 depends on T1
	code, _, raw = e.req("PUT", fmt.Sprintf("/api/v1/tasks/%d/dependencies", t2ID), pmTok, map[string]any{
		"depends_on": []int64{t1ID},
	})
	if code != 200 {
		t.Fatalf("set dependency failed (%d): %s", code, string(raw))
	}
	// Cycle T1 depends on T2 must fail 422
	code, _, _ = e.req("PUT", fmt.Sprintf("/api/v1/tasks/%d/dependencies", t1ID), pmTok, map[string]any{
		"depends_on": []int64{t2ID},
	})
	if code != 422 {
		t.Fatalf("expected 422 on dependency cycle, got %d", code)
	}
	// Moving T2 to completed while T1 open must fail 409
	code, _, _ = e.req("PATCH", fmt.Sprintf("/api/v1/tasks/%d/status", t2ID), empTok, map[string]any{"status": "completed"})
	if code != 409 {
		t.Fatalf("expected 409 moving dependent task before blocker done, got %d", code)
	}
	// Employee requests approval on T1 -> outcome = approval_requested, then PM approves T1, then PM completes T2!
	code, m1Resp, raw := e.req("PATCH", fmt.Sprintf("/api/v1/tasks/%d/status", t1ID), empTok, map[string]any{"status": "completed"})
	if code != 200 {
		t.Fatalf("employee request completion on T1 failed (%d): %s", code, string(raw))
	}
	var m1 map[string]any
	_ = json.Unmarshal(m1Resp.Data, &m1)
	if m1["outcome"] != "approval_requested" {
		t.Fatalf("expected approval_requested on employee move to completed, got %v", m1["outcome"])
	}
	code, _, raw = e.req("POST", fmt.Sprintf("/api/v1/tasks/%d/approval", t1ID), pmTok, map[string]any{"approve": true})
	if code != 200 {
		t.Fatalf("PM approve T1 failed (%d): %s", code, string(raw))
	}
	code, _, raw = e.req("PATCH", fmt.Sprintf("/api/v1/tasks/%d/status", t2ID), pmTok, map[string]any{"status": "completed"})
	if code != 200 {
		t.Fatalf("move T2 completed after blocker approved failed (%d): %s", code, string(raw))
	}

	// Report & move bug
	code, bResp, raw := e.req("POST", fmt.Sprintf("/api/v1/projects/%d/bugs", pid), empTok, map[string]any{
		"title":       "Null check missing",
		"description": "Panics on empty body",
		"severity":    "high",
		"priority":    "high",
		"assignee_id": empID,
	})
	if code != 201 {
		t.Fatalf("report bug failed (%d): %s", code, string(raw))
	}
	var bug map[string]any
	_ = json.Unmarshal(bResp.Data, &bug)
	bid := int64(bug["id"].(float64))

	// Assign bug via PATCH (auto-transitions reported -> assigned), then advance fixing -> testing -> closed
	code, assignResp, raw := e.req("PATCH", fmt.Sprintf("/api/v1/bugs/%d", bid), pmTok, map[string]any{"assignee_id": empID})
	if code != 200 {
		t.Fatalf("assign bug failed (%d): %s", code, string(raw))
	}
	var assignedBug map[string]any
	_ = json.Unmarshal(assignResp.Data, &assignedBug)
	if assignedBug["status"] != "assigned" {
		t.Fatalf("expected bug auto-assigned status, got %v", assignedBug["status"])
	}
	for _, nextStatus := range []string{"fixing", "testing", "closed"} {
		code, _, raw = e.req("PATCH", fmt.Sprintf("/api/v1/bugs/%d/status", bid), pmTok, map[string]any{"status": nextStatus})
		if code != 200 {
			t.Fatalf("bug move to %s failed (%d): %s", nextStatus, code, string(raw))
		}
	}
	// Reopen without note must 422
	code, _, _ = e.req("PATCH", fmt.Sprintf("/api/v1/bugs/%d/status", bid), pmTok, map[string]any{"status": "reopened"})
	if code != 422 {
		t.Fatalf("expected 422 on bug reopen without note, got %d", code)
	}

	// Upload attachment & verify signed download URL
	var mbuf bytes.Buffer
	mw := multipart.NewWriter(&mbuf)
	fw, _ := mw.CreateFormFile("file", "notes.txt")
	_, _ = fw.Write([]byte("hello attachment payload"))
	_ = mw.Close()

	upReq, _ := http.NewRequest("POST", e.srv.URL+fmt.Sprintf("/api/v1/tasks/%d/attachments", t1ID), &mbuf)
	upReq.Header.Set("Authorization", "Bearer "+empTok)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upRes, err := http.DefaultClient.Do(upReq)
	if err != nil {
		t.Fatal(err)
	}
	defer upRes.Body.Close()
	if upRes.StatusCode != 201 {
		b, _ := io.ReadAll(upRes.Body)
		t.Fatalf("upload attachment failed (%d): %s", upRes.StatusCode, string(b))
	}
	var attWrap apiResp
	_ = json.NewDecoder(upRes.Body).Decode(&attWrap)
	var attObj map[string]any
	_ = json.Unmarshal(attWrap.Data, &attObj)
	dlURL := attObj["url"].(string)

	dlRes, err := http.Get(dlURL)
	if err != nil {
		t.Fatal(err)
	}
	defer dlRes.Body.Close()
	dlBytes, _ := io.ReadAll(dlRes.Body)
	if dlRes.StatusCode != 200 || string(dlBytes) != "hello attachment payload" {
		t.Fatalf("signed download failed (%d): %q", dlRes.StatusCode, string(dlBytes))
	}
}

func TestAvatarsChatCelebrationsTrialsMonitoringAdminSearchGithubReminders(t *testing.T) {
	e := newTestEnv(t)
	leadID := e.seedUser("Lead User", "lead@example.com", "lead", "Password#1234", nil, nil)
	teamID := e.seedTeam("Backend Team", &leadID)
	emptyTeamID := e.seedTeam("Empty Team", nil)
	_, _ = e.db.ExecContext(context.Background(), `UPDATE users SET team_id = ? WHERE id = ?`, teamID, leadID)

	ceoID := e.seedUser("CEO User", "ceo@example.com", "ceo", "Password#1234", nil, nil)
	mon1ID := e.seedUser("Monitor 1", "mon1@example.com", "monitor", "Password#1234", nil, nil)
	_ = e.seedUser("Monitor 2", "mon2@example.com", "monitor", "Password#1234", nil, nil)
	pmID := e.seedUser("PM User", "pm@example.com", "pm", "Password#1234", nil, nil)
	bday := "1996-02-29" // leap day birth date test!
	empID := e.seedUser("Ali Dev", "ali@example.com", "employee", "Password#1234", &teamID, &bday)

	ceoTok := e.login("ceo@example.com", "Password#1234")
	mon1Tok := e.login("mon1@example.com", "Password#1234")
	mon2Tok := e.login("mon2@example.com", "Password#1234")
	pmTok := e.login("pm@example.com", "Password#1234")
	empTok := e.login("ali@example.com", "Password#1234")
	leadTok := e.login("lead@example.com", "Password#1234")

	// 1. Avatar upload PNG -> serve EXIF-stripped JPEG -> delete
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{20, 140, 220, 255})
		}
	}
	var abuf bytes.Buffer
	mw := multipart.NewWriter(&abuf)
	afw, _ := mw.CreateFormFile("file", "pic.png")
	_ = png.Encode(afw, img)
	_ = mw.Close()
	areq, _ := http.NewRequest("PUT", e.srv.URL+"/api/v1/me/avatar", &abuf)
	areq.Header.Set("Authorization", "Bearer "+empTok)
	areq.Header.Set("Content-Type", mw.FormDataContentType())
	ares, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatal(err)
	}
	defer ares.Body.Close()
	if ares.StatusCode != 200 {
		t.Fatalf("avatar upload failed: %d", ares.StatusCode)
	}
	var avWrap apiResp
	_ = json.NewDecoder(ares.Body).Decode(&avWrap)
	var avUser map[string]any
	_ = json.Unmarshal(avWrap.Data, &avUser)
	avURL := avUser["avatar_url"].(string)
	avGet, _ := http.Get(avURL)
	if avGet == nil || avGet.StatusCode != 200 {
		t.Fatalf("avatar fetch failed")
	}
	avGet.Body.Close()
	code, _, _ := e.req("DELETE", "/api/v1/me/avatar", empTok, nil)
	if code != 200 {
		t.Fatalf("delete own avatar failed: %d", code)
	}

	// 2. Celebrations (CEO sends to team, empty team returns 422, member sees & marks seen)
	code, _, _ = e.req("POST", "/api/v1/celebrations", ceoTok, map[string]any{
		"team_id": emptyTeamID,
		"kind":    "good",
	})
	if code != 422 {
		t.Fatalf("expected 422 on empty team celebration, got %d", code)
	}
	code, celResp, raw := e.req("POST", "/api/v1/celebrations", ceoTok, map[string]any{
		"team_id": teamID,
		"kind":    "big",
		"message": "Shipped Go API!",
	})
	if code != 201 {
		t.Fatalf("celebration failed (%d): %s", code, string(raw))
	}
	var cel map[string]any
	_ = json.Unmarshal(celResp.Data, &cel)
	if int(cel["sent_to"].(float64)) != 2 {
		t.Fatalf("expected celebration sent_to=2, got %v", cel["sent_to"])
	}
	code, mineResp, _ := e.req("GET", "/api/v1/me/celebrations", empTok, nil)
	if code != 200 {
		t.Fatalf("GET /me/celebrations failed: %d", code)
	}
	var mine []map[string]any
	_ = json.Unmarshal(mineResp.Data, &mine)
	if len(mine) != 1 {
		t.Fatalf("expected 1 unseen celebration, got %d", len(mine))
	}
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/celebrations/%v/seen", cel["id"]), empTok, nil)
	if code != 200 {
		t.Fatalf("mark celebration seen failed: %d", code)
	}

	// 3. Trials (monitor only, creator isolated)
	code, _, _ = e.req("GET", "/api/v1/trials", ceoTok, nil)
	if code != 403 {
		t.Fatalf("expected 403 for CEO accessing trials, got %d", code)
	}
	code, trResp, raw := e.req("POST", "/api/v1/trials", mon1Tok, map[string]any{
		"name":       "Backend Cohort",
		"goal":       "Evaluate Go",
		"started_on": "2026-11-01",
	})
	if code != 201 {
		t.Fatalf("create trial team failed (%d): %s", code, string(raw))
	}
	var trTeam map[string]any
	_ = json.Unmarshal(trResp.Data, &trTeam)
	trID := int64(trTeam["id"].(float64))
	// Another monitor must get 404
	code, _, _ = e.req("GET", fmt.Sprintf("/api/v1/trials/%d", trID), mon2Tok, nil)
	if code != 404 {
		t.Fatalf("expected 404 for another monitor accessing trial team, got %d", code)
	}
	code, mResp, _ := e.req("POST", fmt.Sprintf("/api/v1/trials/%d/members", trID), mon1Tok, map[string]any{
		"name":      "Omar",
		"specialty": "Go Developer",
		"joined_on": "2026-11-02",
	})
	if code != 201 {
		t.Fatalf("add trial member failed: %d", code)
	}
	var tmem map[string]any
	_ = json.Unmarshal(mResp.Data, &tmem)
	tmemID := int64(tmem["id"].(float64))
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/trials/members/%d/entries", tmemID), mon1Tok, map[string]any{"score": 5, "text": "Solid PRs"})
	if code != 201 {
		t.Fatalf("add trial entry failed: %d", code)
	}
	code, _, _ = e.req("PATCH", fmt.Sprintf("/api/v1/trials/%d", trID), mon1Tok, map[string]any{"status": "ended"})
	if code != 200 {
		t.Fatalf("end trial failed: %d", code)
	}

	// 4. Monitor cannot assign ceo/admin in POST /admin/users, and gets 403 on PATCH /admin/users/{id}
	code, _, _ = e.req("POST", "/api/v1/admin/users", mon1Tok, map[string]any{
		"name":     "Blocked Admin",
		"email":    "bad@example.com",
		"role":     "admin",
		"password": "Password#1234",
	})
	if code != 422 {
		t.Fatalf("expected 422 when monitor creates admin, got %d", code)
	}
	code, crResp, raw := e.req("POST", "/api/v1/admin/users", mon1Tok, map[string]any{
		"name":     "New Tester",
		"email":    "tester@example.com",
		"role":     "employee",
		"password": "Password#1234",
	})
	if code != 201 {
		t.Fatalf("monitor create employee failed (%d): %s", code, string(raw))
	}
	var newUsr map[string]any
	_ = json.Unmarshal(crResp.Data, &newUsr)
	code, _, _ = e.req("PATCH", fmt.Sprintf("/api/v1/admin/users/%v", newUsr["id"]), mon1Tok, map[string]any{"name": "Denied"})
	if code != 403 {
		t.Fatalf("expected 403 for monitor PATCH /admin/users/{id}, got %d", code)
	}

	// 5. Chat group + mention + Search
	code, grpResp, raw := e.req("POST", "/api/v1/chats/groups", pmTok, map[string]any{
		"name":       "Backend Guild",
		"member_ids": []int64{empID, leadID},
	})
	if code != 201 {
		t.Fatalf("create chat group failed (%d): %s", code, string(raw))
	}
	var grp map[string]any
	_ = json.Unmarshal(grpResp.Data, &grp)
	gkey := grp["key"].(string)
	code, _, raw = e.req("POST", fmt.Sprintf("/api/v1/chats/%s/messages", gkey), pmTok, map[string]any{
		"body":     "Hello @Ali review needed",
		"mentions": []int64{empID},
	})
	if code != 201 {
		t.Fatalf("send chat message failed (%d): %s", code, string(raw))
	}
	code, _, _ = e.req("GET", fmt.Sprintf("/api/v1/chats/%s/messages", gkey), empTok, nil)
	if code != 200 {
		t.Fatalf("read chat messages failed: %d", code)
	}

	code, srchResp, _ := e.req("GET", "/api/v1/search?q=Ali", leadTok, nil)
	if code != 200 {
		t.Fatalf("search failed: %d", code)
	}
	var srch map[string]any
	_ = json.Unmarshal(srchResp.Data, &srch)
	if people, _ := srch["people"].([]any); len(people) == 0 {
		t.Fatalf("expected search for 'Ali' to match seeded employee")
	}

	// 6. GitHub settings + HMAC webhook for merged PR mentioning task code
	code, pResp, _ := e.req("POST", "/api/v1/projects", pmTok, map[string]any{
		"name": "Alpha", "description": "GH test", "pm_id": pmID,
		"start": "2026-01-01", "end": "2026-12-31", "member_ids": []int64{empID},
	})
	var proj map[string]any
	_ = json.Unmarshal(pResp.Data, &proj)
	pid := int64(proj["id"].(float64))
	code, _, _ = e.req("PUT", fmt.Sprintf("/api/v1/projects/%d/github", pid), pmTok, map[string]any{
		"enabled": true,
		"repos":   []string{"acme/followup"},
	})
	if code != 200 {
		t.Fatalf("save github settings failed: %d", code)
	}
	code, tResp, _ := e.req("POST", fmt.Sprintf("/api/v1/projects/%d/tasks", pid), pmTok, map[string]any{
		"title": "Fix push", "description": "desc", "assignee_id": empID,
		"due": "2026-11-23", "priority": "high",
	})
	var taskObj map[string]any
	_ = json.Unmarshal(tResp.Data, &taskObj)
	tid := int64(taskObj["id"].(float64))
	tcode := taskObj["code"].(string)

	whBody, _ := json.Marshal(map[string]any{
		"action":     "closed",
		"repository": map[string]any{"full_name": "acme/followup"},
		"pull_request": map[string]any{
			"number":    42,
			"title":     tcode + " implement webpush",
			"html_url":  "https://github.com/acme/followup/pull/42",
			"merged":    true,
			"state":     "closed",
			"merged_at": "2026-11-24T10:00:00Z",
			"head":      map[string]any{"ref": "feat/push"},
			"user":      map[string]any{"login": "alidev"},
		},
	})
	mac := hmac.New(sha256.New, []byte(e.cfg.GithubWebhookSecret))
	mac.Write(whBody)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	whReq, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/webhooks/github", bytes.NewReader(whBody))
	whReq.Header.Set("X-GitHub-Event", "pull_request")
	whReq.Header.Set("X-GitHub-Delivery", "deliv-1")
	whReq.Header.Set("X-Hub-Signature-256", sig)
	whRes, err := http.DefaultClient.Do(whReq)
	if err != nil || whRes.StatusCode != 200 {
		t.Fatalf("github webhook failed: %v", err)
	}
	whRes.Body.Close()

	code, giResp, _ := e.req("GET", fmt.Sprintf("/api/v1/tasks/%d/github", tid), empTok, nil)
	if code != 200 {
		t.Fatalf("GET task github items failed: %d", code)
	}
	var gi []map[string]any
	_ = json.Unmarshal(giResp.Data, &gi)
	if len(gi) != 1 || gi[0]["state"] != "merged" {
		t.Fatalf("expected 1 merged github PR linked to %s, got %v", tcode, gi)
	}

	// 7. Daily reminders on non-leap 2026-02-28 triggers birthday_today for 1996-02-29 + task_due_soon/overdue, and dedups on 2nd run
	stats1, err := e.app.Reminders.Send(context.Background(), "2026-02-28")
	if err != nil {
		t.Fatalf("reminders run 1 failed: %v", err)
	}
	if stats1["birthday_today"] == 0 {
		t.Fatalf("expected leap-day fallback birthday_today notifications on 2026-02-28, got %v", stats1)
	}
	stats2, err := e.app.Reminders.Send(context.Background(), "2026-02-28")
	if err != nil {
		t.Fatalf("reminders run 2 failed: %v", err)
	}
	if stats2["birthday_today"] != 0 || stats2["task_overdue"] != 0 {
		t.Fatalf("expected idempotent 0 new reminders on 2nd run, got %v", stats2)
	}

	// 8. Check performance, monitoring/CEO inbox, ratings, announcements, admin import/audit & reports
	code, _, _ = e.req("PUT", "/api/v1/performance/criteria", ceoTok, map[string]any{
		"items": []map[string]any{
			{"id": "c1", "name": "الالتزام", "name_en": "Discipline", "kind": "rating", "weight": 50},
			{"id": "c2", "name": "الإنجاز", "name_en": "Delivery", "kind": "metric", "metric": "done_rate", "weight": 50},
		},
	})
	if code != 200 {
		t.Fatalf("save criteria failed: %d", code)
	}
	code, _, _ = e.req("POST", "/api/v1/performance/criteria/approve", ceoTok, nil)
	if code != 200 {
		t.Fatalf("approve criteria failed: %d", code)
	}
	code, _, _ = e.req("GET", "/api/v1/performance?from=2026-01-01&to=2026-12-31", ceoTok, nil)
	if code != 200 {
		t.Fatalf("performance overview failed: %d", code)
	}

	// Monitoring: employee request -> monitor forwards -> CEO answers
	code, reqResp, raw := e.req("POST", "/api/v1/monitoring/notes", empTok, map[string]any{
		"kind":    "request",
		"subject": "Hardware",
		"title":   "Need test device",
		"body":    "Please provision Android test phone",
	})
	if code != 201 {
		t.Fatalf("create monitoring request failed (%d): %s", code, string(raw))
	}
	var monReq map[string]any
	_ = json.Unmarshal(reqResp.Data, &monReq)
	monReqID := int64(monReq["id"].(float64))
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/monitoring/requests/%d/forward", monReqID), mon1Tok, map[string]any{
		"forward_note": "Escalating to CEO",
	})
	if code != 200 {
		t.Fatalf("forward request failed: %d", code)
	}
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/ceo/inbox/%d/answer", monReqID), ceoTok, map[string]any{
		"state": "done",
		"reply": "Approved today",
	})
	if code != 200 {
		t.Fatalf("ceo answer inbox failed: %d", code)
	}

	// Ratings: CEO rates team & updates within 24h
	code, rtResp, raw := e.req("POST", fmt.Sprintf("/api/v1/teams/%d/ratings", teamID), ceoTok, map[string]any{
		"score":       5,
		"reason_type": "speed",
		"reason":      "Great migration speed",
	})
	if code != 201 {
		t.Fatalf("create team rating failed (%d): %s", code, string(raw))
	}
	var rtObj map[string]any
	_ = json.Unmarshal(rtResp.Data, &rtObj)
	code, _, _ = e.req("PATCH", fmt.Sprintf("/api/v1/ratings/%v", rtObj["id"]), ceoTok, map[string]any{
		"score":       4,
		"reason_type": "quality",
		"reason":      "Updated rating note",
	})
	if code != 200 {
		t.Fatalf("update rating within 24h failed: %d", code)
	}

	// Announcements
	code, annResp, _ := e.req("POST", "/api/v1/announcements", ceoTok, map[string]any{
		"title":     "Go Backend Live",
		"body":      "All endpoints migrated",
		"important": true,
	})
	if code != 201 {
		t.Fatalf("create announcement failed: %d", code)
	}
	var annObj map[string]any
	_ = json.Unmarshal(annResp.Data, &annObj)
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/announcements/%v/seen", annObj["id"]), empTok, nil)
	if code != 200 {
		t.Fatalf("mark announcement seen failed: %d", code)
	}

	// Admin dry_run import + audit check + health check matching frontend types
	code, impResp, _ := e.req("POST", "/api/v1/admin/users/import", ceoTok, map[string]any{
		"dry_run": true,
		"rows": []map[string]any{
			{"name": "Batch One", "email": "batch1@example.com", "role": "employee", "password": "Password#1234"},
		},
	})
	if code != 200 {
		t.Fatalf("dry_run import failed: %d", code)
	}
	var imp map[string]any
	_ = json.Unmarshal(impResp.Data, &imp)
	if impRows, _ := imp["rows"].([]any); len(impRows) != 1 || int(imp["skipped"].(float64)) != 0 {
		t.Fatalf("expected 1 valid import row with skipped=0, got %v", imp)
	}
	code, audResp, _ := e.req("GET", "/api/v1/audit", ceoTok, nil)
	if code != 200 {
		t.Fatalf("GET /audit failed: %d", code)
	}
	var audRows []map[string]any
	_ = json.Unmarshal(audResp.Data, &audRows)
	if len(audRows) == 0 {
		t.Fatalf("expected non-empty audit rows")
	}
	if tgt, ok := audRows[0]["target"].(map[string]any); !ok || tgt["type"] == "" {
		t.Fatalf("expected AuditEntry.target object on /audit, got %v", audRows[0])
	}
	audID := int64(audRows[0]["id"].(float64))
	code, audDetResp, _ := e.req("GET", fmt.Sprintf("/api/v1/audit/%d", audID), ceoTok, nil)
	if code != 200 {
		t.Fatalf("GET /audit/%d failed: %d", audID, code)
	}
	var audDet map[string]any
	_ = json.Unmarshal(audDetResp.Data, &audDet)
	if _, ok := audDet["target"].(map[string]any); !ok || audDet["related_by_actor"] == nil {
		t.Fatalf("expected top-level target + related_by_actor in AuditDetail, got %v", audDet)
	}

	_ = e.app.Notifications.Notify(context.Background(), []int64{ceoID}, "announcement_posted", notifications.About{ActorID: &pmID})
	code, _, _ = e.req("GET", "/api/v1/reports/overview", ceoTok, nil)
	if code != 200 {
		t.Fatalf("reports overview failed: %d", code)
	}
	code, hResp, _ := e.req("GET", "/api/v1/admin/health", ceoTok, nil)
	if code != 200 {
		t.Fatalf("admin health failed: %d", code)
	}
	var health map[string]any
	_ = json.Unmarshal(hResp.Data, &health)
	if _, ok := health["users_active"]; !ok || health["storage"] == nil {
		t.Fatalf("expected DataHealth users_active & storage fields, got %v", health)
	}
	code, _, _ = e.req("GET", "/api/v1/site/live", ceoTok, nil)
	if code != 200 {
		t.Fatalf("GET /site/live failed: %d", code)
	}
	code, _, _ = e.req("GET", "/api/v1/ceo/advanced", ceoTok, nil)
	if code != 200 {
		t.Fatalf("GET /ceo/advanced failed: %d", code)
	}
	_ = leadID
	_ = mon1ID
}

// Helper check so store.User is referenced cleanly if needed.
var _ = store.SystemRoles

func wsDialAndAuth(t *testing.T, srvURL, token string) (*websocket.Conn, error) {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(srvURL, "http") + "/api/v1/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		if err := conn.WriteJSON(map[string]any{"type": "auth", "token": token}); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func readWSEventsUntil(t *testing.T, conn *websocket.Conn, timeout time.Duration, pred func(ev map[string]any) bool) ([]map[string]any, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var seen []map[string]any
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var ev map[string]any
		if json.Unmarshal(raw, &ev) == nil {
			seen = append(seen, ev)
			if pred(ev) {
				return seen, true
			}
		}
	}
	return seen, false
}

func TestWebSocketLiveEventsAndAuthCloseCodes(t *testing.T) {
	e := newTestEnv(t)
	e.app.WS.AuthTimeout = 120 * time.Millisecond
	e.app.WS.PingInterval = 150 * time.Millisecond

	leadID := e.seedUser("Lead WS", "leadws@example.com", "lead", "Password#1234", nil, nil)
	teamID := e.seedTeam("WS Team", &leadID)
	ceoID := e.seedUser("CEO WS", "ceows@example.com", "ceo", "Password#1234", nil, nil)
	pmID := e.seedUser("PM WS", "pmws@example.com", "pm", "Password#1234", nil, nil)
	empID := e.seedUser("Emp WS", "empws@example.com", "employee", "Password#1234", &teamID, nil)
	devID := e.seedUser("Dev WS", "devws@example.com", "employee", "Password#1234", nil, nil)
	_, _ = e.db.ExecContext(context.Background(), `UPDATE users SET team_id = ? WHERE id = ?`, teamID, leadID)

	ceoTok := e.login("ceows@example.com", "Password#1234")
	pmTok := e.login("pmws@example.com", "Password#1234")
	empTok := e.login("empws@example.com", "Password#1234")
	_ = ceoID
	_ = pmID

	// 1. Invalid token must close with custom code 4401
	badConn, err := wsDialAndAuth(t, e.srv.URL, "999|invalid-secret")
	if err != nil {
		t.Fatalf("dial bad ws: %v", err)
	}
	_ = badConn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, badErr := badConn.ReadMessage()
	badConn.Close()
	var ce *websocket.CloseError
	if badErr == nil || !errors.As(badErr, &ce) || ce.Code != 4401 {
		t.Fatalf("expected WS close code 4401 for bad token, got: %v", badErr)
	}

	// 2. Missing auth frame within AuthTimeout must close
	noAuthConn, err := wsDialAndAuth(t, e.srv.URL, "")
	if err != nil {
		t.Fatalf("dial no-auth ws: %v", err)
	}
	_ = noAuthConn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, noAuthErr := noAuthConn.ReadMessage()
	noAuthConn.Close()
	if noAuthErr == nil {
		t.Fatalf("expected WS close when no auth sent within timeout")
	}

	// 3. Connect PM and Employee over WebSocket
	pmConn, err := wsDialAndAuth(t, e.srv.URL, pmTok)
	if err != nil {
		t.Fatalf("dial pm ws: %v", err)
	}
	defer pmConn.Close()

	empConn, err := wsDialAndAuth(t, e.srv.URL, empTok)
	if err != nil {
		t.Fatalf("dial emp ws: %v", err)
	}
	defer empConn.Close()

	// Wait briefly until both clients register cleanly
	deadline := time.Now().Add(time.Second)
	for e.app.WS.ConnectedCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.app.WS.ConnectedCount() != 2 {
		t.Fatalf("expected 2 connected WS clients, got %d", e.app.WS.ConnectedCount())
	}

	// Test Ping frame reaches client cleanly
	pingCh := make(chan struct{}, 2)
	empConn.SetPingHandler(func(appData string) error {
		select {
		case pingCh <- struct{}{}:
		default:
		}
		return empConn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})

	// 4. Create group chat -> chat_changed sent to members
	code, grpResp, raw := e.req("POST", "/api/v1/chats/groups", pmTok, map[string]any{
		"name":       "WS Room",
		"member_ids": []int64{empID, leadID, devID},
	})
	if code != 201 {
		t.Fatalf("create group failed (%d): %s", code, string(raw))
	}
	var grp map[string]any
	_ = json.Unmarshal(grpResp.Data, &grp)
	gkey := grp["key"].(string)

	if evs, ok := readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "chat_changed" && ev["chat"] == gkey
	}); !ok {
		t.Fatalf("emp expected chat_changed on group create, saw: %v", evs)
	}
	if evs, ok := readWSEventsUntil(t, pmConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "chat_changed" && ev["chat"] == gkey
	}); !ok {
		t.Fatalf("pm expected chat_changed on group create, saw: %v", evs)
	}

	// Wait > PingInterval so server queues a Ping frame, then confirm Ping handler fires on read
	time.Sleep(200 * time.Millisecond)

	// 5. Send message mentioning emp -> writer AND reader get chat_message, reader ALSO gets notification!
	code, _, raw = e.req("POST", fmt.Sprintf("/api/v1/chats/%s/messages", gkey), pmTok, map[string]any{
		"body":     "Ping @Emp WS",
		"mentions": []int64{empID},
	})
	if code != 201 {
		t.Fatalf("send chat msg failed (%d): %s", code, string(raw))
	}
	if evs, ok := readWSEventsUntil(t, pmConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "chat_message" && ev["chat"] == gkey
	}); !ok {
		t.Fatalf("writer (pm) expected chat_message event, saw: %v", evs)
	}

	gotChatMsg, gotNotif := false, false
	_, _ = readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		if ev["type"] == "chat_message" && ev["chat"] == gkey {
			gotChatMsg = true
		}
		if ev["type"] == "notification" {
			gotNotif = true
		}
		return gotChatMsg && gotNotif
	})
	if !gotChatMsg || !gotNotif {
		t.Fatalf("emp expected both chat_message (%v) and notification (%v)", gotChatMsg, gotNotif)
	}
	select {
	case <-pingCh:
	default:
		t.Fatalf("expected WebSocket ping frame on idle connection")
	}

	// 6. Mark read -> reader receives chat_read
	code, _, _ = e.req("POST", fmt.Sprintf("/api/v1/chats/%s/read", gkey), empTok, nil)
	if code != 200 {
		t.Fatalf("mark read failed: %d", code)
	}
	if evs, ok := readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "chat_read" && ev["chat"] == gkey
	}); !ok {
		t.Fatalf("emp expected chat_read event, saw: %v", evs)
	}

	// 7. Update group removing empID -> removed old member (emp) MUST still get chat_changed!
	code, _, raw = e.req("PATCH", fmt.Sprintf("/api/v1/chats/%s", gkey), pmTok, map[string]any{
		"member_ids": []int64{leadID, devID},
	})
	if code != 200 {
		t.Fatalf("update group failed (%d): %s", code, string(raw))
	}
	if evs, ok := readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "chat_changed" && ev["chat"] == gkey
	}); !ok {
		t.Fatalf("removed old member expected chat_changed event, saw: %v", evs)
	}

	// 8. Celebration -> team member gets celebration event
	code, _, _ = e.req("POST", "/api/v1/celebrations", ceoTok, map[string]any{
		"team_id": teamID,
		"kind":    "good",
		"message": "WS celebration!",
	})
	if code != 201 {
		t.Fatalf("celebration failed: %d", code)
	}
	if evs, ok := readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "celebration"
	}); !ok {
		t.Fatalf("emp expected celebration event, saw: %v", evs)
	}

	// 9. Advanced settings change -> everyone gets site_live
	code, advGet, _ := e.req("GET", "/api/v1/admin/advanced", ceoTok, nil)
	if code != 200 {
		t.Fatalf("get advanced failed: %d", code)
	}
	var advData map[string]any
	_ = json.Unmarshal(advGet.Data, &advData)
	code, _, raw = e.req("PUT", "/api/v1/admin/advanced", ceoTok, map[string]any{"rules": advData["rules"]})
	if code != 200 {
		t.Fatalf("save advanced failed (%d): %s", code, string(raw))
	}
	if evs, ok := readWSEventsUntil(t, empConn, time.Second, func(ev map[string]any) bool {
		return ev["type"] == "site_live"
	}); !ok {
		t.Fatalf("emp expected site_live event, saw: %v", evs)
	}

	// 10. Logout closes emp's live WS with code 4401!
	code, _, _ = e.req("POST", "/api/v1/auth/logout", empTok, nil)
	if code != 200 {
		t.Fatalf("logout failed: %d", code)
	}
	_ = empConn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, outErr := empConn.ReadMessage()
	var outClose *websocket.CloseError
	if outErr == nil || !errors.As(outErr, &outClose) || outClose.Code != 4401 {
		t.Fatalf("expected 4401 close on logout, got: %v", outErr)
	}
}
