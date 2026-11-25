package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"golang.org/x/crypto/bcrypt"

	"followapi/internal/app"
	"followapi/internal/config"
	"followapi/internal/dbx"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	if cmd == "generate-vapid" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("VAPID_PUBLIC_KEY=%s\n", pub)
		fmt.Printf("VAPID_PRIVATE_KEY=%s\n", priv)
		return
	}

	cfg := config.Load(".env")
	db, err := dbx.Open(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database connection failed: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if db.IsSQLite() {
		if err := db.BootstrapSQLite(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "sqlite bootstrap failed: %v\n", err)
			os.Exit(1)
		}
	}

	application := app.New(cfg, db)
	ctx := context.Background()

	switch cmd {
	case "create-ceo":
		os.Exit(runCreateCEO(ctx, db, os.Args[2:]))
	case "set-password":
		os.Exit(runSetPassword(ctx, db, os.Args[2:]))
	case "send-daily-reminders":
		os.Exit(runDailyReminders(ctx, application, os.Args[2:]))
	case "deployment-check":
		os.Exit(runDeploymentCheck(ctx, cfg, db))
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("FollowUP Go CLI:")
	fmt.Println("  create-ceo              Create the initial CEO account")
	fmt.Println("  set-password            Reset a user's password and revoke active tokens/sessions")
	fmt.Println("  send-daily-reminders    Run daily due/overdue/bug/birthday reminders (--day=YYYY-MM-DD)")
	fmt.Println("  deployment-check        Verify database tables, storage writability, and config")
	fmt.Println("  generate-vapid          Generate an ES256 VAPID keypair for Web Push")
}

func runCreateCEO(ctx context.Context, db *dbx.DB, args []string) int {
	fs := flag.NewFlagSet("create-ceo", flag.ExitOnError)
	emailFlag := fs.String("email", "", "CEO email")
	nameFlag := fs.String("name", "", "CEO Arabic/primary name")
	nameEnFlag := fs.String("name-en", "", "CEO English name")
	titleFlag := fs.String("title", "", "CEO job title")
	pwFlag := fs.String("password", "", "Password (min 12 chars)")
	_ = fs.Parse(args)

	reader := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
		fmt.Print(prompt)
		if def != "" {
			fmt.Printf(" [%s]", def)
		}
		fmt.Print(": ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	email := strings.ToLower(strings.TrimSpace(*emailFlag))
	if email == "" {
		email = strings.ToLower(ask("CEO email address", ""))
	}
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, ".") {
		fmt.Fprintln(os.Stderr, "Invalid email address.")
		return 1
	}

	var cnt int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE LOWER(email) = ?`, email).Scan(&cnt)
	if cnt > 0 {
		fmt.Fprintln(os.Stderr, "A user with this email already exists.")
		return 1
	}

	name := strings.TrimSpace(*nameFlag)
	if name == "" {
		name = ask("CEO full name (Arabic or primary)", "المدير التنفيذي")
	}
	nameEn := strings.TrimSpace(*nameEnFlag)
	if nameEn == "" {
		nameEn = ask("CEO English name (optional)", "Chief Executive Officer")
	}
	title := strings.TrimSpace(*titleFlag)
	if title == "" {
		title = ask("Job title", "المدير التنفيذي")
	}

	pw := *pwFlag
	if pw == "" {
		pw = ask("Password (minimum 12 characters)", "")
		confirm := ask("Confirm password", "")
		if pw != confirm {
			fmt.Fprintln(os.Stderr, "Passwords do not match.")
			return 1
		}
	}
	if len([]rune(pw)) < 12 {
		fmt.Fprintln(os.Stderr, "Password must be at least 12 characters long.")
		return 1
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Hash failed: %v\n", err)
		return 1
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var nameEnPtr *string
	if nameEn != "" {
		nameEnPtr = &nameEn
	}

	var id int64
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (name, name_en, email, role, preferred_locale, title, active, password, created_at, updated_at)
			VALUES (?, ?, ?, 'ceo', 'ar', ?, 1, ?, ?, ?)
		`, name, nameEnPtr, email, title, string(hash), nowStr, nowStr)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		_, err = tx.ExecContext(ctx, `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, id, nowStr, nowStr)
		return err
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Create failed: %v\n", err)
		return 1
	}

	fmt.Printf("CEO account created successfully (ID: %d, Email: %s).\n", id, email)
	return 0
}

func runSetPassword(ctx context.Context, db *dbx.DB, args []string) int {
	fs := flag.NewFlagSet("set-password", flag.ExitOnError)
	emailFlag := fs.String("email", "", "User email")
	pwFlag := fs.String("password", "", "New password (min 12 chars)")
	_ = fs.Parse(args)

	email := strings.ToLower(strings.TrimSpace(*emailFlag))
	if email == "" && fs.NArg() > 0 {
		email = strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	}
	if email == "" {
		fmt.Fprintln(os.Stderr, "Usage: set-password <email> [--password=...] ")
		return 1
	}

	var uid int64
	err := db.QueryRowContext(ctx, `SELECT id FROM users WHERE LOWER(email) = ?`, email).Scan(&uid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "User not found.")
		return 1
	}

	pw := *pwFlag
	if pw == "" {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("New password (minimum 12 characters): ")
		p1, _ := reader.ReadString('\n')
		fmt.Print("Confirm new password: ")
		p2, _ := reader.ReadString('\n')
		pw = strings.TrimSpace(p1)
		if pw != strings.TrimSpace(p2) {
			fmt.Fprintln(os.Stderr, "Passwords do not match.")
			return 1
		}
	}
	if len([]rune(pw)) < 12 {
		fmt.Fprintln(os.Stderr, "Password must be at least 12 characters long.")
		return 1
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Hash failed: %v\n", err)
		return 1
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password = ?, updated_at = ? WHERE id = ?`, string(hash), nowStr, uid); err != nil {
			return err
		}
		_, _ = tx.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE tokenable_type = 'App\\Models\\User' AND tokenable_id = ?`, uid)
		_, _ = tx.ExecContext(ctx, `UPDATE auth_sessions SET ended_at = ?, end_reason = 'everywhere' WHERE user_id = ? AND ended_at IS NULL`, nowStr, uid)
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
		return 1
	}
	fmt.Printf("Password updated and existing tokens revoked for %s.\n", email)
	return 0
}

func runDailyReminders(ctx context.Context, application *app.App, args []string) int {
	fs := flag.NewFlagSet("send-daily-reminders", flag.ExitOnError)
	day := fs.String("day", "", "Override Baghdad date (YYYY-MM-DD)")
	_ = fs.Parse(args)

	application.Push.StartWorker()
	defer application.Push.StopWorker()

	stats, err := application.Reminders.Send(ctx, *day)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	b, _ := json.MarshalIndent(stats, "", "  ")
	fmt.Println(string(b))
	return 0
}

func runDeploymentCheck(ctx context.Context, cfg config.Config, db *dbx.DB) int {
	ok := true
	check := func(label string, passed bool, detail string) {
		mark := "PASS"
		if !passed {
			mark = "FAIL"
			ok = false
		}
		fmt.Printf("[%s] %s (%s)\n", mark, label, detail)
	}

	err := db.PingContext(ctx)
	check("Database connection", err == nil, fmt.Sprintf("%s@%s/%s", cfg.DBConnection, cfg.DBHost, cfg.DBDatabase))

	requiredTables := []string{
		"users", "teams", "personal_access_tokens", "auth_sessions", "login_security",
		"app_settings", "advanced_settings", "projects", "project_members", "project_statuses",
		"project_phases", "sprints", "tasks", "task_dependencies", "task_approvals", "bugs",
		"attachments", "avatars", "announcements", "chat_groups", "direct_conversations",
		"messages", "chat_reads", "notifications", "push_subscriptions", "performance_reviews",
		"monitor_notes", "ratings", "celebrations", "trial_teams", "audit_logs",
	}
	missing := 0
	for _, tbl := range requiredTables {
		var dummy int
		if qerr := db.QueryRowContext(ctx, fmt.Sprintf("SELECT 1 FROM %s LIMIT 1", tbl)).Scan(&dummy); qerr != nil && qerr != sql.ErrNoRows {
			missing++
			fmt.Printf("  missing/unreadable table: %s (%v)\n", tbl, qerr)
		}
	}
	check("Required tables", missing == 0, fmt.Sprintf("%d checked, %d missing", len(requiredTables), missing))

	for _, sub := range []string{"attachments", "avatars", "tmp"} {
		dir := filepath.Join(cfg.StorageRoot, sub)
		merr := os.MkdirAll(dir, 0o755)
		probe := filepath.Join(dir, ".write-test")
		werr := os.WriteFile(probe, []byte("ok"), 0o644)
		_ = os.Remove(probe)
		check("Storage "+sub, merr == nil && werr == nil, dir)
	}

	vapidOK := cfg.VAPIDPublicKey != "" && cfg.VAPIDPrivateKey != ""
	check("VAPID Web Push", true, fmt.Sprintf("configured=%v", vapidOK))

	var ceoCnt int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'ceo' AND active = 1`).Scan(&ceoCnt)
	check("Active CEO account", ceoCnt > 0, fmt.Sprintf("count=%d", ceoCnt))

	var critCnt int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_criteria WHERE id = 1`).Scan(&critCnt)
	check("Performance criteria row 1", critCnt > 0, fmt.Sprintf("count=%d", critCnt))

	schedOn := strings.EqualFold(os.Getenv("ENABLE_DAILY_SCHEDULER"), "true") || os.Getenv("ENABLE_DAILY_SCHEDULER") == "1"
	check("Daily reminder scheduler env", schedOn, fmt.Sprintf("ENABLE_DAILY_SCHEDULER=%v", schedOn))

	if !ok {
		return 1
	}
	return 0
}
