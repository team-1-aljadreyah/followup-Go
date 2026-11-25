package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/audit"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/push"
	"followapi/internal/store"
	"followapi/internal/support"
	"followapi/internal/ws"

	"golang.org/x/crypto/bcrypt"
	"github.com/go-chi/chi/v5"
)

const (
	maxImportRows = 500
)

var importRoles = map[string]bool{"pm": true, "monitor": true, "lead": true, "employee": true}

var auditActionKind = map[string]string{
	"user_created": "account", "user_changed": "account", "user_stopped": "account",
	"user_restarted": "account", "user_unlocked": "account", "picture_removed": "account",
	"users_imported": "account",
	"team_created": "team", "team_changed": "team",
	"settings_changed": "settings",
	"login_failed": "signin", "login_locked": "signin", "session_ended": "signin", "sessions_ended": "signin",
	"project_created": "project", "project_pm_changed": "project", "sprint_closed": "project", "template_deleted": "project",
	"announcement_deleted": "announcement",
	"criteria_changed": "review", "criteria_approved": "review", "review_finalized": "review",
	"rating_changed": "review", "rating_deleted": "review",
}

var auditActionWords = map[string][2]string{
	"user_created":         {"أضاف مستخدماً", "added a user"},
	"user_changed":         {"عدّل مستخدماً", "changed a user"},
	"user_stopped":         {"أوقف حساباً", "stopped an account"},
	"user_restarted":       {"أعاد تشغيل حساب", "restarted an account"},
	"user_unlocked":        {"فتح حساباً مقفلاً", "unlocked an account"},
	"team_created":         {"أنشأ فريقاً", "created a team"},
	"team_changed":         {"عدّل فريقاً", "changed a team"},
	"settings_changed":     {"غيّر الإعدادات", "changed the settings"},
	"login_failed":         {"محاولة دخول خاطئة", "failed sign-in"},
	"login_locked":         {"قُفل حساب", "account locked"},
	"project_created":      {"أنشأ مشروعاً", "created a project"},
	"project_pm_changed":   {"غيّر مدير قسم", "changed a project's department manager"},
	"sprint_closed":        {"أنهى دورة", "ended a sprint"},
	"template_deleted":     {"حذف قالباً", "removed a template"},
	"announcement_deleted": {"حذف إعلاناً", "removed an announcement"},
	"criteria_changed":     {"غيّر معايير التقييم", "changed the review criteria"},
	"criteria_approved":    {"اعتمد معايير التقييم", "approved the review criteria"},
	"review_finalized":     {"اعتمد تقييم أداء", "made a review final"},
	"picture_removed":      {"أزال صورة شخصية", "removed a profile picture"},
	"session_ended":        {"أنهى جلسة دخول", "ended a sign-in session"},
	"sessions_ended":       {"أخرج حساباً من كل الأجهزة", "signed an account out everywhere"},
	"users_imported":       {"استورد حسابات", "imported accounts"},
	"rating_changed":       {"عدّل تقييماً سريعاً كتبه غيره", "changed someone else's quick rating"},
	"rating_deleted":       {"حذف تقييماً سريعاً", "removed a quick rating"},
}

func normalizeAuditAction(action string) string {
	switch action {
	case "user_updated":
		return "user_changed"
	case "team_updated":
		return "team_changed"
	case "settings_updated":
		return "settings_changed"
	case "session_revoked":
		return "session_ended"
	case "sessions_revoked":
		return "sessions_ended"
	default:
		return action
	}
}

func expandAuditActionAliases(action string) []string {
	switch normalizeAuditAction(action) {
	case "user_changed":
		return []string{"user_changed", "user_updated"}
	case "team_changed":
		return []string{"team_changed", "team_updated"}
	case "settings_changed":
		return []string{"settings_changed", "settings_updated"}
	case "session_ended":
		return []string{"session_ended", "session_revoked"}
	case "sessions_ended":
		return []string{"sessions_ended", "sessions_revoked"}
	default:
		return []string{action}
	}
}

type Service struct {
	db       *dbx.DB
	cfg      config.Config
	audit    *audit.Service
	notif    *notifications.Service
	advanced *advanced.Service
	push     *push.Service
	ws       *ws.Hub
}

func New(db *dbx.DB, cfg config.Config, auditSvc *audit.Service, notifSvc *notifications.Service, advSvc *advanced.Service, pushSvc *push.Service) *Service {
	return &Service{db: db, cfg: cfg, audit: auditSvc, notif: notifSvc, advanced: advSvc, push: pushSvc}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func (s *Service) HandleUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanListAdminUsers(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	list, err := s.loadAdminUsers(ctx, u)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, list)
}

func (s *Service) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanListAdminUsers(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	name := strings.TrimSpace(strVal(input["name"]))
	if name == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.name_required"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(strVal(input["email"])))
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, ".") {
		httpx.HandleErr(w, r, httpx.ErrValidation("email", "api.admin.email_invalid"))
		return
	}
	var cnt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE LOWER(email) = ?`, email).Scan(&cnt)
	if cnt > 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("email", "api.admin.email_taken"))
		return
	}

	role := strings.TrimSpace(strVal(input["role"]))
	var customRole *string
	if cr := strings.TrimSpace(strVal(input["custom_role"])); cr != "" {
		found := s.advanced.FindRoleByKey(ctx, cr)
		if found == nil {
			httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.custom_role_not_found"))
			return
		}
		customRole = &cr
		if base := strVal(found["base"]); base != "" {
			role = base
		}
	}
	if !store.UserHasRole(role, store.SystemRoles) {
		httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.role_invalid"))
		return
	}
	if u.Role == "monitor" && (role == "ceo" || role == "admin") {
		httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.monitor_role_forbidden"))
		return
	}

	title := strings.TrimSpace(strVal(input["title"]))
	if support.RuneLen(title) > 120 {
		httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.admin.title_max"))
		return
	}

	teamID, err := s.validateTeamID(ctx, input["team_id"])
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	birthDate := optStr(input["birth_date"])
	if birthDate != nil && *birthDate != "" {
		if msg := support.BirthDateProblem(*birthDate, support.BaghdadToday()); msg != "" {
			httpx.HandleErr(w, r, httpx.ErrValidation("birth_date", "api.admin.birth_date_"+msg))
			return
		}
	} else {
		birthDate = nil
	}

	pw := strVal(input["password"])
	if msgKey, params := support.PasswordProblem(pw, s.advanced.PasswordMin(ctx)); msgKey != "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("password", msgKey, params))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	nameEn := optStr(input["name_en"])
	phone := optStr(input["phone"])
	locale := strings.TrimSpace(strVal(input["preferred_locale"]))
	if locale != "en" {
		locale = "ar"
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())

	var newID int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO users (name, name_en, email, phone, role, custom_role, birth_date, preferred_locale, title, team_id, active, password, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
		`, name, nameEn, email, phone, role, customRole, birthDate, locale, title, teamID, string(hash), nowStr, nowStr)
		if err != nil {
			return err
		}
		newID, _ = res.LastInsertId()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)
		`, newID, nowStr, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	store.InvalidateUsersCache()
	if teamID != nil {
		var tname string
		var tnameEn sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM teams WHERE id = ?`, *teamID).Scan(&tname, &tnameEn)
		d := map[string]any{"team_name": tname, "note": tname}
		if tnameEn.Valid && tnameEn.String != "" {
			d["team_name_en"] = tnameEn.String
		}
		s.notif.Notify(ctx, []int64{newID}, "team_joined", notifications.About{ActorID: &u.ID, TeamID: teamID, Data: d})
	}

	changes := []audit.Change{
		{Field: "name", To: &name},
		{Field: "email", To: &email},
		{Field: "role", To: &role},
	}
	s.audit.Log(ctx, &u.ID, "user_created", "user", &newID, email, changes)

	created, err := s.loadSingleAdminUser(ctx, newID, u)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, created, http.StatusCreated)
}

func (s *Service) HandleImportUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	rawRows, _ := input["rows"].([]any)
	if len(rawRows) == 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("rows", "api.admin.import_empty"))
		return
	}
	if len(rawRows) > maxImportRows {
		httpx.HandleErr(w, r, httpx.ErrValidation("rows", "api.admin.import_limit", map[string]any{"max": maxImportRows}))
		return
	}
	dryRun, _ := input["dry_run"].(bool)

	existingEmails := map[string]bool{}
	erows, _ := s.db.QueryContext(ctx, `SELECT LOWER(email) FROM users`)
	if erows != nil {
		for erows.Next() {
			var em string
			if erows.Scan(&em) == nil {
				existingEmails[em] = true
			}
		}
		_ = erows.Close()
	}

	teamByName := map[string]int64{}
	trows, _ := s.db.QueryContext(ctx, `SELECT id, name, name_en FROM teams`)
	if trows != nil {
		for trows.Next() {
			var tid int64
			var tn string
			var ten sql.NullString
			if trows.Scan(&tid, &tn, &ten) == nil {
				teamByName[strings.ToLower(strings.TrimSpace(tn))] = tid
				if ten.Valid && strings.TrimSpace(ten.String) != "" {
					teamByName[strings.ToLower(strings.TrimSpace(ten.String))] = tid
				}
			}
		}
		_ = trows.Close()
	}

	minPW := s.advanced.PasswordMin(ctx)

	type preparedRow struct {
		line     int
		name     string
		nameEn   *string
		email    string
		phone    *string
		role     string
		title    string
		teamID   *int64
		teamName string
		pwHash   string
		problems []map[string]any
	}
	parsed := make([]preparedRow, 0, len(rawRows))
	firstLine := map[string]int{}
	validCount := 0

	for i, raw := range rawRows {
		rm, _ := raw.(map[string]any)
		if rm == nil {
			rm = map[string]any{}
		}
		pr := preparedRow{
			line:   i + 1,
			name:   strings.TrimSpace(strVal(rm["name"])),
			nameEn: optStr(rm["name_en"]),
			email:  strings.ToLower(strings.TrimSpace(strVal(rm["email"]))),
			phone:  optStr(rm["phone"]),
			title:  strings.TrimSpace(strVal(rm["title"])),
		}
		probs := []map[string]any{}
		if pr.name == "" {
			probs = append(probs, map[string]any{"field": "name", "problem": "name_missing"})
		} else if support.RuneLen(pr.name) > 120 {
			probs = append(probs, map[string]any{"field": "name", "problem": "name_too_long"})
		}

		_, mailErr := mail.ParseAddress(pr.email)
		validEmail := pr.email != "" && mailErr == nil && strings.Contains(pr.email, ".")
		if pr.email == "" {
			probs = append(probs, map[string]any{"field": "email", "problem": "email_missing"})
		} else if !validEmail {
			probs = append(probs, map[string]any{"field": "email", "problem": "email_bad"})
		} else if prev, dup := firstLine[pr.email]; dup {
			probs = append(probs, map[string]any{"field": "email", "problem": "email_twice", "other_line": prev})
		} else if existingEmails[pr.email] {
			probs = append(probs, map[string]any{"field": "email", "problem": "email_taken"})
		}
		if validEmail {
			if _, ok := firstLine[pr.email]; !ok {
				firstLine[pr.email] = pr.line
			}
		}

		roleRaw := strings.ToLower(strings.TrimSpace(strVal(rm["role"])))
		if roleRaw == "" {
			roleRaw = "employee"
		}
		pr.role = mapImportRole(roleRaw)
		if !store.UserHasRole(pr.role, store.SystemRoles) {
			probs = append(probs, map[string]any{"field": "role", "problem": "role_unknown"})
		} else if !importRoles[pr.role] {
			probs = append(probs, map[string]any{"field": "role", "problem": "role_not_allowed"})
		}

		if support.RuneLen(pr.title) > 120 {
			probs = append(probs, map[string]any{"field": "title", "problem": "title_too_long"})
		}

		if tn := strings.TrimSpace(strVal(rm["team"])); tn != "" {
			if tid, ok := teamByName[strings.ToLower(tn)]; ok {
				pr.teamID = &tid
				pr.teamName = tn
			} else {
				probs = append(probs, map[string]any{"field": "team", "problem": "team_unknown"})
			}
		}

		pw := strVal(rm["password"])
		if pw == "" {
			probs = append(probs, map[string]any{"field": "password", "problem": "password_missing"})
		} else if support.RuneLen(pw) < minPW {
			probs = append(probs, map[string]any{"field": "password", "problem": "password_short"})
		} else if !dryRun && len(probs) == 0 {
			if h, err := bcrypt.GenerateFromPassword([]byte(pw), 12); err == nil {
				pr.pwHash = string(h)
			}
		}

		pr.problems = probs
		if len(probs) == 0 {
			validCount++
		}
		parsed = append(parsed, pr)
	}

	checks := make([]map[string]any, 0, len(parsed))
	createdCount := 0
	if !dryRun {
		nowStr := dbx.FormatDBTime(time.Now().UTC())
		for _, pr := range parsed {
			rowObj := map[string]any{"line": pr.line, "problems": pr.problems}
			if len(pr.problems) > 0 {
				checks = append(checks, rowObj)
				continue
			}
			var nid int64
			err := s.db.Tx(ctx, func(tx *sql.Tx) error {
				res, err := tx.ExecContext(ctx, `
					INSERT INTO users (name, name_en, email, phone, role, preferred_locale, title, team_id, active, password, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, 'ar', ?, ?, 1, ?, ?, ?)
				`, pr.name, pr.nameEn, pr.email, pr.phone, pr.role, pr.title, pr.teamID, pr.pwHash, nowStr, nowStr)
				if err != nil {
					return err
				}
				nid, _ = res.LastInsertId()
				_, err = tx.ExecContext(ctx, `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, nid, nowStr, nowStr)
				return err
			})
			if err == nil {
				createdCount++
				rowObj["user_id"] = nid
				changes := []audit.Change{{Field: "role", To: &pr.role}, {Field: "source", To: audit.StrPtr("import")}}
				s.audit.Log(ctx, &u.ID, "user_created", "user", &nid, pr.name, changes)
				if pr.teamID != nil {
					s.notif.Notify(ctx, []int64{nid}, "team_joined", notifications.About{ActorID: &u.ID, TeamID: pr.teamID, Data: map[string]any{"note": pr.teamName}})
				}
			}
			checks = append(checks, rowObj)
		}
		skipped := len(checks) - createdCount
		cntStr, skStr := strconv.Itoa(createdCount), strconv.Itoa(skipped)
		s.audit.Log(ctx, &u.ID, "users_imported", "import", nil, fmt.Sprintf("%d / %d", createdCount, len(checks)), []audit.Change{
			{Field: "created", To: &cntStr},
			{Field: "skipped", To: &skStr},
		})
	} else {
		for _, pr := range parsed {
			checks = append(checks, map[string]any{"line": pr.line, "problems": pr.problems})
		}
	}
	if !dryRun && createdCount > 0 {
		store.InvalidateUsersCache()
	}

	skipped := len(checks) - createdCount
	if dryRun {
		skipped = len(checks) - validCount
	}
	httpx.Success(w, map[string]any{
		"dry_run": dryRun,
		"rows":    checks,
		"created": createdCount,
		"skipped": skipped,
	})
}

func (s *Service) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	target, err := store.FindUserByID(ctx, s.db, "", id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.user_not_found"))
		return
	}

	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	newName := target.Name
	if hasKey(input, "name") {
		newName = strings.TrimSpace(strVal(input["name"]))
		if newName == "" {
			httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.name_required"))
			return
		}
	}
	newNameEn := target.NameEn
	if hasKey(input, "name_en") {
		newNameEn = optStr(input["name_en"])
	}
	newPhone := target.Phone
	if hasKey(input, "phone") {
		newPhone = optStr(input["phone"])
	}
	newEmail := target.Email
	if hasKey(input, "email") {
		newEmail = strings.ToLower(strings.TrimSpace(strVal(input["email"])))
		if _, err := mail.ParseAddress(newEmail); err != nil || !strings.Contains(newEmail, ".") {
			httpx.HandleErr(w, r, httpx.ErrValidation("email", "api.admin.email_invalid"))
			return
		}
		var cnt int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE LOWER(email) = ? AND id != ?`, newEmail, target.ID).Scan(&cnt)
		if cnt > 0 {
			httpx.HandleErr(w, r, httpx.ErrValidation("email", "api.admin.email_taken"))
			return
		}
	}

	newRole := target.Role
	newCustomRole := target.CustomRole
	if hasKey(input, "custom_role") {
		cr := strings.TrimSpace(strVal(input["custom_role"]))
		if cr == "" {
			newCustomRole = nil
		} else {
			found := s.advanced.FindRoleByKey(ctx, cr)
			if found == nil {
				httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.custom_role_not_found"))
				return
			}
			newCustomRole = &cr
			if base := strVal(found["base"]); base != "" {
				newRole = base
			}
		}
	}
	if hasKey(input, "role") {
		rVal := strings.TrimSpace(strVal(input["role"]))
		if !store.UserHasRole(rVal, store.SystemRoles) {
			httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.role_invalid"))
			return
		}
		newRole = rVal
	}

	newActive := target.Active
	if hasKey(input, "active") {
		if b, ok := input["active"].(bool); ok {
			newActive = b
		}
	}

	if target.ID == u.ID && (newRole != target.Role || !newActive) {
		field := "role"
		if !newActive {
			field = "active"
		}
		httpx.HandleErr(w, r, httpx.ErrValidation(field, "api.admin.self_guard"))
		return
	}

	if target.Role == "admin" && target.Active && (newRole != "admin" || !newActive) {
		var cnt int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND active = 1`).Scan(&cnt)
		if cnt <= 1 {
			httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.last_admin"))
			return
		}
	}

	if target.Role == "pm" && (newRole != "pm" || !newActive) {
		rows, _ := s.db.QueryContext(ctx, `SELECT code FROM projects WHERE pm_id = ? AND state NOT IN ('completed','cancelled','done') ORDER BY id ASC`, target.ID)
		var codes []string
		if rows != nil {
			for rows.Next() {
				var c string
				if rows.Scan(&c) == nil {
					codes = append(codes, c)
				}
			}
			_ = rows.Close()
		}
		if len(codes) > 0 {
			httpx.HandleErr(w, r, httpx.ErrValidation("role", "api.admin.pm_open_projects", map[string]any{"codes": strings.Join(codes, ", ")}))
			return
		}
	}

	newTitle := target.Title
	if hasKey(input, "title") {
		newTitle = strings.TrimSpace(strVal(input["title"]))
		if support.RuneLen(newTitle) > 120 {
			httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.admin.title_max"))
			return
		}
	}

	newTeamID := target.TeamID
	if hasKey(input, "team_id") {
		newTeamID, err = s.validateTeamID(ctx, input["team_id"])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
	}

	newBirthDate := target.BirthDate
	if hasKey(input, "birth_date") {
		bd := optStr(input["birth_date"])
		if bd != nil && *bd != "" {
			if msg := support.BirthDateProblem(*bd, support.BaghdadToday()); msg != "" {
				httpx.HandleErr(w, r, httpx.ErrValidation("birth_date", "api.admin.birth_date_"+msg))
				return
			}
			newBirthDate = bd
		} else {
			newBirthDate = nil
		}
	}

	newHash := target.Password
	pwChanged := false
	if hasKey(input, "password") {
		pw := strVal(input["password"])
		if pw != "" {
			if msgKey, params := support.PasswordProblem(pw, s.advanced.PasswordMin(ctx)); msgKey != "" {
				httpx.HandleErr(w, r, httpx.ErrValidation("password", msgKey, params))
				return
			}
			h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
			if err != nil {
				httpx.HandleErr(w, r, err)
				return
			}
			newHash = string(h)
			pwChanged = true
		}
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	activeInt := 0
	if newActive {
		activeInt = 1
	}

	var changes []audit.Change
	if newName != target.Name {
		changes = append(changes, audit.Change{Field: "name", From: &target.Name, To: &newName})
	}
	if newEmail != target.Email {
		changes = append(changes, audit.Change{Field: "email", From: &target.Email, To: &newEmail})
	}
	if newRole != target.Role {
		changes = append(changes, audit.Change{Field: "role", From: &target.Role, To: &newRole})
	}
	if newActive != target.Active {
		f, t := strconv.FormatBool(target.Active), strconv.FormatBool(newActive)
		changes = append(changes, audit.Change{Field: "active", From: &f, To: &t})
	}
	if hasKey(input, "birth_date") && !eqStrPtr(newBirthDate, target.BirthDate) {
		changes = append(changes, audit.Change{Field: "birth_date", From: maskPtr(target.BirthDate), To: maskPtr(newBirthDate)})
	}
	if pwChanged {
		changes = append(changes, audit.Change{Field: "password", To: audit.StrPtr("updated")})
	}

	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE users SET name=?, name_en=?, email=?, phone=?, role=?, custom_role=?, birth_date=?, title=?, team_id=?, active=?, password=?, updated_at=?
			WHERE id=?
		`, newName, newNameEn, newEmail, newPhone, newRole, newCustomRole, newBirthDate, newTitle, newTeamID, activeInt, newHash, nowStr, target.ID)
		if err != nil {
			return err
		}
		if !newActive || pwChanged {
			reason := "everywhere"
			if !newActive {
				reason = "stopped"
			}
			_, _ = tx.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE tokenable_type = 'App\\Models\\User' AND tokenable_id = ?`, target.ID)
			_, _ = tx.ExecContext(ctx, `UPDATE auth_sessions SET ended_at = ?, ended_by = ?, end_reason = ? WHERE user_id = ? AND ended_at IS NULL`,
				nowStr, u.ID, reason, target.ID)
		}
		if unlock, _ := input["unlock"].(bool); unlock {
			_, _ = tx.ExecContext(ctx, `UPDATE login_security SET failed_logins = 0, locked_until = NULL, updated_at = ? WHERE user_id = ?`, nowStr, target.ID)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	if newTeamID != nil && (target.TeamID == nil || *newTeamID != *target.TeamID) {
		var tname string
		var tnameEn sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT name, name_en FROM teams WHERE id = ?`, *newTeamID).Scan(&tname, &tnameEn)
		d := map[string]any{"team_name": tname, "note": tname}
		if tnameEn.Valid && tnameEn.String != "" {
			d["team_name_en"] = tnameEn.String
		}
		s.notif.Notify(ctx, []int64{target.ID}, "team_joined", notifications.About{ActorID: &u.ID, TeamID: newTeamID, Data: d})
	}

	auditAction := "user_changed"
	if newActive != target.Active {
		if !newActive {
			auditAction = "user_stopped"
		} else {
			auditAction = "user_restarted"
		}
	}
	s.audit.Log(ctx, &u.ID, auditAction, "user", &target.ID, newName, changes)
	s.ws.InvalidateAuthUser(target.ID)
	store.InvalidateUsersCache()
	if !newActive || pwChanged {
		s.ws.CloseUnauthorizedUser(target.ID)
	}
	if newRole != target.Role || !eqStrPtr(newCustomRole, target.CustomRole) || newActive != target.Active || !eqInt64Ptr(newTeamID, target.TeamID) || newTitle != target.Title {
		s.ws.NotifySiteLive()
	}

	updated, err := s.loadSingleAdminUser(ctx, target.ID, u)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, updated)
}

func (s *Service) HandleUnlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	target, err := store.FindUserByID(ctx, s.db, "", id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.user_not_found"))
		return
	}

	var lockedRaw any
	_ = s.db.QueryRowContext(ctx, `SELECT locked_until FROM login_security WHERE user_id = ?`, target.ID).Scan(&lockedRaw)
	lockedUntil := dbx.ParseTime(lockedRaw)
	if lockedUntil == nil || !lockedUntil.After(time.Now().UTC()) {
		httpx.HandleErr(w, r, httpx.ErrValidation("user", "api.admin.not_locked"))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `UPDATE login_security SET failed_logins = 0, locked_until = NULL, updated_at = ? WHERE user_id = ?`, nowStr, target.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(ctx, &u.ID, "user_unlocked", "user", &target.ID, target.Email, nil)

	res, err := s.loadSingleAdminUser(ctx, target.ID, u)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, res)
}

func (s *Service) HandleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.sessions_forbidden"))
		return
	}

	idleEdge := dbx.FormatDBTime(time.Now().UTC().AddDate(0, 0, -30))
	where := []string{"s.ended_at IS NULL", "s.last_seen_at > ?"}
	args := []any{idleEdge}
	if uidStr := strings.TrimSpace(r.URL.Query().Get("user_id")); uidStr != "" {
		if uid, err := strconv.ParseInt(uidStr, 10, 64); err == nil && uid > 0 {
			where = append(where, "s.user_id = ?")
			args = append(args, uid)
		}
	}

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT s.id, s.user_id, s.ip, s.user_agent, s.started_at, s.last_seen_at
		FROM auth_sessions s
		WHERE %s
		ORDER BY s.last_seen_at DESC
		LIMIT 200
	`, strings.Join(where, " AND ")), args...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	currentSID := httpx.SessionIDFrom(ctx)
	out := []map[string]any{}
	for rows.Next() {
		var sid string
		var uid int64
		var ip, ua sql.NullString
		var startedRaw, lastRaw any
		if rows.Scan(&sid, &uid, &ip, &ua, &startedRaw, &lastRaw) != nil {
			continue
		}
		out = append(out, store.CleanMap(map[string]any{
			"id":           sid,
			"user_id":      uid,
			"ip":           nullStrPtr(ip),
			"user_agent":   nullStrPtr(ua),
			"started_at":   support.FormatISO(dbx.ParseTime(startedRaw)),
			"last_seen_at": support.FormatISO(dbx.ParseTime(lastRaw)),
			"current":      currentSID != "" && sid == currentSID,
		}))
	}
	httpx.Success(w, out)
}

func (s *Service) HandleRevokeSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.sessions_forbidden"))
		return
	}
	sid := strings.TrimSpace(chi.URLParam(r, "id"))
	if sid == "" {
		sid = strings.TrimSpace(r.PathValue("id"))
	}

	var uid int64
	var tokenID sql.NullInt64
	var endedRaw any
	err := s.db.QueryRowContext(ctx, `SELECT user_id, token_id, ended_at FROM auth_sessions WHERE id = ?`, sid).Scan(&uid, &tokenID, &endedRaw)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.session_not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if endedRaw != nil && dbx.ParseTime(endedRaw) != nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("session", "api.admin.session_already_ended"))
		return
	}
	if sid == httpx.SessionIDFrom(ctx) {
		httpx.HandleErr(w, r, httpx.ErrValidation("session", "api.admin.session_current_self"))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE auth_sessions SET ended_at = ?, ended_by = ?, end_reason = 'admin' WHERE id = ?`, nowStr, u.ID, sid)
		if err != nil {
			return err
		}
		if tokenID.Valid {
			_, _ = tx.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE id = ? OR session_id = ?`, tokenID.Int64, sid)
		} else {
			_, _ = tx.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE session_id = ?`, sid)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if s.push != nil {
		s.push.DeleteForSessions(ctx, []string{sid})
	}
	s.ws.CloseUnauthorizedSessions([]string{sid})
	if tokenID.Valid {
		s.ws.CloseUnauthorizedToken(tokenID.Int64)
	}
	s.audit.Log(ctx, &u.ID, "session_ended", "user", &uid, sid, nil)
	httpx.Success(w, nil)
}

func (s *Service) HandleRevokeUserSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.sessions_forbidden"))
		return
	}
	uid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	target, err := store.FindUserByID(ctx, s.db, "", uid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.user_not_found"))
		return
	}

	currentSID := httpx.SessionIDFrom(ctx)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM auth_sessions WHERE user_id = ? AND ended_at IS NULL`, uid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var sids []string
	for rows.Next() {
		var sid string
		if rows.Scan(&sid) == nil && sid != currentSID {
			sids = append(sids, sid)
		}
	}
	_ = rows.Close()

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	if len(sids) > 0 {
		_ = s.db.Tx(ctx, func(tx *sql.Tx) error {
			for _, sid := range sids {
				_, _ = tx.ExecContext(ctx, `UPDATE auth_sessions SET ended_at=?, ended_by=?, end_reason='admin' WHERE id=?`, nowStr, u.ID, sid)
				_, _ = tx.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE session_id = ?`, sid)
			}
			return nil
		})
		if s.push != nil {
			s.push.DeleteForSessions(ctx, sids)
		}
		s.ws.CloseUnauthorizedSessions(sids)
	}
	cntStr := strconv.Itoa(len(sids))
	s.audit.Log(ctx, &u.ID, "sessions_ended", "user", &uid, target.Name, []audit.Change{{Field: "count", To: &cntStr}})
	httpx.Success(w, map[string]any{"ended": len(sids)})
}

func (s *Service) HandleSecuritySummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}

	const failedDays = 7
	now := time.Now().UTC()
	since := dbx.FormatDBTime(now.AddDate(0, 0, -failedDays))
	nowStr := dbx.FormatDBTime(now)

	type accAgg struct {
		userID *int64
		label  string
		count  int
		lastAt string
		lastTS int64
		ips    []string
		ipSet  map[string]bool
	}
	type ipAgg struct {
		ip     string
		count  int
		lastAt string
		lastTS int64
		who    map[string]bool
	}
	accounts := map[string]*accAgg{}
	ips := map[string]*ipAgg{}
	failedTotal := 0

	frows, _ := s.db.QueryContext(ctx, `
		SELECT target_id, target_label, ip, at FROM audit_logs WHERE action = 'login_failed' AND at >= ? AND at <= ?
	`, since, nowStr)
	if frows != nil {
		for frows.Next() {
			var tid sql.NullInt64
			var lbl string
			var ip sql.NullString
			var atRaw any
			if frows.Scan(&tid, &lbl, &ip, &atRaw) != nil {
				continue
			}
			failedTotal++
			atT := dbx.ParseTime(atRaw)
			atIso := ""
			var atTS int64
			if atT != nil {
				if iso := support.FormatISO(atT); iso != nil {
					atIso = *iso
				}
				atTS = atT.Unix()
			}
			key := strings.ToLower(lbl)
			if tid.Valid {
				key = fmt.Sprintf("#%d", tid.Int64)
			}
			agg, ok := accounts[key]
			if !ok {
				var uidPtr *int64
				if tid.Valid {
					v := tid.Int64
					uidPtr = &v
				}
				agg = &accAgg{userID: uidPtr, label: lbl, lastAt: atIso, lastTS: atTS, ips: []string{}, ipSet: map[string]bool{}}
				accounts[key] = agg
			}
			agg.count++
			if atTS > agg.lastTS {
				agg.lastTS = atTS
				agg.lastAt = atIso
			}
			ipStr := "-"
			if ip.Valid && strings.TrimSpace(ip.String) != "" {
				ipStr = strings.TrimSpace(ip.String)
				if !agg.ipSet[ipStr] {
					agg.ipSet[ipStr] = true
					agg.ips = append(agg.ips, ipStr)
				}
			}
			iagg, ok := ips[ipStr]
			if !ok {
				iagg = &ipAgg{ip: ipStr, lastAt: atIso, lastTS: atTS, who: map[string]bool{}}
				ips[ipStr] = iagg
			}
			iagg.count++
			if atTS > iagg.lastTS {
				iagg.lastTS = atTS
				iagg.lastAt = atIso
			}
			iagg.who[key] = true
		}
		_ = frows.Close()
	}

	accList := make([]*accAgg, 0, len(accounts))
	for _, a := range accounts {
		accList = append(accList, a)
	}
	sort.Slice(accList, func(i, j int) bool {
		if accList[i].count != accList[j].count {
			return accList[i].count > accList[j].count
		}
		return accList[i].lastAt > accList[j].lastAt
	})
	byAccount := make([]map[string]any, 0, len(accList))
	for _, a := range accList {
		row := map[string]any{"label": a.label, "count": a.count, "last_at": a.lastAt, "ips": a.ips}
		if a.userID != nil {
			row["user_id"] = *a.userID
		}
		byAccount = append(byAccount, row)
	}

	ipList := make([]*ipAgg, 0, len(ips))
	for _, ip := range ips {
		ipList = append(ipList, ip)
	}
	sort.Slice(ipList, func(i, j int) bool {
		if ipList[i].count != ipList[j].count {
			return ipList[i].count > ipList[j].count
		}
		return ipList[i].lastAt > ipList[j].lastAt
	})
	byIP := make([]map[string]any, 0, len(ipList))
	for _, ip := range ipList {
		byIP = append(byIP, map[string]any{"ip": ip.ip, "count": ip.count, "last_at": ip.lastAt, "accounts": len(ip.who)})
	}

	locked := []map[string]any{}
	lrows, _ := s.db.QueryContext(ctx, `
		SELECT user_id, failed_logins, locked_until FROM login_security
		WHERE locked_until IS NOT NULL AND locked_until > ? ORDER BY locked_until ASC
	`, nowStr)
	if lrows != nil {
		for lrows.Next() {
			var uid int64
			var fails int
			var untilRaw any
			if lrows.Scan(&uid, &fails, &untilRaw) == nil {
				locked = append(locked, map[string]any{
					"user_id":      uid,
					"locked_until": support.FormatISO(dbx.ParseTime(untilRaw)),
					"failed":       fails,
				})
			}
		}
		_ = lrows.Close()
	}

	neverSignedIn := []map[string]any{}
	nrows, _ := s.db.QueryContext(ctx, `
		SELECT u.id, u.created_at
		FROM users u LEFT JOIN login_security ls ON ls.user_id = u.id
		WHERE u.active = 1 AND ls.last_login_at IS NULL
		ORDER BY u.created_at ASC, u.id ASC
	`)
	if nrows != nil {
		for nrows.Next() {
			var uid int64
			var crRaw any
			if nrows.Scan(&uid, &crRaw) == nil {
				row := map[string]any{"user_id": uid}
				if cr := support.FormatISO(dbx.ParseTime(crRaw)); cr != nil {
					row["created_at"] = *cr
				}
				neverSignedIn = append(neverSignedIn, row)
			}
		}
		_ = nrows.Close()
	}

	settings, _ := store.LoadAppSetting(ctx, s.db)
	httpx.Success(w, map[string]any{
		"days":            failedDays,
		"failed_total":    failedTotal,
		"by_account":      byAccount,
		"by_ip":           byIP,
		"locked":          locked,
		"never_signed_in": neverSignedIn,
		"lock_after":      settings.LockAfter,
		"lock_minutes":    settings.LockMinutes,
	})
}

func (s *Service) HandleTeams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, name_en, lead_id, created_at, updated_at FROM teams ORDER BY id ASC`)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	counts := map[int64]int{}
	crows, _ := s.db.QueryContext(ctx, `SELECT team_id, COUNT(*) FROM users WHERE active = 1 AND team_id IS NOT NULL GROUP BY team_id`)
	if crows != nil {
		for crows.Next() {
			var tid int64
			var c int
			if crows.Scan(&tid, &c) == nil {
				counts[tid] = c
			}
		}
		_ = crows.Close()
	}

	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var nameEn sql.NullString
		var leadID sql.NullInt64
		var cAt, uAt any
		if rows.Scan(&id, &name, &nameEn, &leadID, &cAt, &uAt) != nil {
			continue
		}
		out = append(out, map[string]any{
			"id":            id,
			"name":          name,
			"name_en":       nullStrPtr(nameEn),
			"lead_id":       nullIntPtr(leadID),
			"members_count": counts[id],
			"created_at":    support.FormatISO(dbx.ParseTime(cAt)),
			"updated_at":    support.FormatISO(dbx.ParseTime(uAt)),
		})
	}
	httpx.Success(w, out)
}

func (s *Service) HandleCreateTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	name := strings.TrimSpace(strVal(input["name"]))
	if name == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.team_name_required"))
		return
	}
	var dup int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM teams WHERE LOWER(name) = LOWER(?)`, name).Scan(&dup)
	if dup > 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.team_name_taken"))
		return
	}
	leadID, err := s.validateLeadID(ctx, input["lead_id"])
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	nameEn := optStr(input["name_en"])
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var newID int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO teams (name, name_en, lead_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			name, nameEn, leadID, nowStr, nowStr)
		if err != nil {
			return err
		}
		newID, _ = res.LastInsertId()
		if leadID != nil {
			_, err = tx.ExecContext(ctx, `UPDATE users SET team_id = ?, updated_at = ? WHERE id = ?`, newID, nowStr, *leadID)
		}
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(ctx, &u.ID, "team_created", "team", &newID, name, []audit.Change{{Field: "name", To: &name}})
	if leadID != nil {
		store.InvalidateUsersCache()
		d := map[string]any{"team_name": name, "note": name}
		if nameEn != nil && *nameEn != "" {
			d["team_name_en"] = *nameEn
		}
		s.notif.Notify(ctx, []int64{*leadID}, "team_joined", notifications.About{ActorID: &u.ID, TeamID: &newID, Data: d})
	}
	s.ws.NotifySiteLive()
	httpx.Success(w, map[string]any{
		"id":         newID,
		"name":       name,
		"name_en":    nameEn,
		"lead_id":    leadID,
		"created_at": support.FormatISO(dbx.ParseTimeString(nowStr)),
		"updated_at": support.FormatISO(dbx.ParseTimeString(nowStr)),
	}, http.StatusCreated)
}

func (s *Service) HandleUpdateTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var curName string
	var curNameEn sql.NullString
	var curLead sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT name, name_en, lead_id FROM teams WHERE id = ?`, id).Scan(&curName, &curNameEn, &curLead)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.team_not_found_404"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	newName := curName
	if hasKey(input, "name") {
		newName = strings.TrimSpace(strVal(input["name"]))
		if newName == "" {
			httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.team_name_required"))
			return
		}
		var dup int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM teams WHERE LOWER(name) = LOWER(?) AND id != ?`, newName, id).Scan(&dup)
		if dup > 0 {
			httpx.HandleErr(w, r, httpx.ErrValidation("name", "api.admin.team_name_taken"))
			return
		}
	}
	newNameEn := nullStrPtr(curNameEn)
	if hasKey(input, "name_en") {
		newNameEn = optStr(input["name_en"])
	}
	curLeadPtr := nullIntPtr(curLead)
	newLead := curLeadPtr
	if hasKey(input, "lead_id") {
		newLead, err = s.validateLeadID(ctx, input["lead_id"])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE teams SET name=?, name_en=?, lead_id=?, updated_at=? WHERE id=?`, newName, newNameEn, newLead, nowStr, id)
		if err != nil {
			return err
		}
		if newLead != nil {
			_, err = tx.ExecContext(ctx, `UPDATE users SET team_id = ?, updated_at = ? WHERE id = ?`, id, nowStr, *newLead)
		}
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(ctx, &u.ID, "team_changed", "team", &id, newName, nil)
	if newLead != nil {
		store.InvalidateUsersCache()
		if !eqInt64Ptr(newLead, curLeadPtr) {
			d := map[string]any{"team_name": newName, "note": newName}
			if newNameEn != nil && *newNameEn != "" {
				d["team_name_en"] = *newNameEn
			}
			s.notif.Notify(ctx, []int64{*newLead}, "team_joined", notifications.About{ActorID: &u.ID, TeamID: &id, Data: d})
		}
	}
	s.ws.NotifySiteLive()
	httpx.Success(w, map[string]any{
		"id":      id,
		"name":    newName,
		"name_en": newNameEn,
		"lead_id": newLead,
	})
}

func (s *Service) HandleSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	st, err := store.LoadAppSetting(ctx, s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, map[string]any{
		"due_soon_days": st.DueSoonDays,
		"lock_after":    st.LockAfter,
		"lock_minutes":  st.LockMinutes,
	})
}

func (s *Service) HandleSaveSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}
	st, err := store.LoadAppSetting(ctx, s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	dueSoon, lockAfter, lockMins := st.DueSoonDays, st.LockAfter, st.LockMinutes
	if hasKey(input, "due_soon_days") {
		v, ok := toIntStrict(input["due_soon_days"])
		if !ok || v < 1 || v > 7 {
			httpx.HandleErr(w, r, httpx.ErrValidation("due_soon_days", "api.admin.setting_out_of_range"))
			return
		}
		dueSoon = v
	}
	if hasKey(input, "lock_after") {
		v, ok := toIntStrict(input["lock_after"])
		if !ok || v < 3 || v > 10 {
			httpx.HandleErr(w, r, httpx.ErrValidation("lock_after", "api.admin.setting_out_of_range"))
			return
		}
		lockAfter = v
	}
	if hasKey(input, "lock_minutes") {
		v, ok := toIntStrict(input["lock_minutes"])
		if !ok || v < 5 || v > 120 {
			httpx.HandleErr(w, r, httpx.ErrValidation("lock_minutes", "api.admin.setting_out_of_range"))
			return
		}
		lockMins = v
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `UPDATE app_settings SET due_soon_days=?, lock_after=?, lock_minutes=?, updated_at=? WHERE id=1`,
		dueSoon, lockAfter, lockMins, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.advanced.InvalidateCache()
	s.audit.Log(ctx, &u.ID, "settings_changed", "settings", nil, "الإعدادات", nil)
	httpx.Success(w, map[string]any{
		"due_soon_days": dueSoon,
		"lock_after":    lockAfter,
		"lock_minutes":  lockMins,
	})
}

func (s *Service) HandleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !access.CanAdminister(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.forbidden"))
		return
	}

	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var (
		attCount, attBytes, voiceCount, voiceBytes int64
		picCount, picBytes, auditCount             int64
		usersActive, usersStopped, usersLocked     int64
		projectsCount, tasksCount                  int64
	)
	_ = s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM attachments WHERE voice IS NULL AND mime NOT LIKE 'audio/%'),
			(SELECT COALESCE(SUM(size), 0) FROM attachments WHERE voice IS NULL AND mime NOT LIKE 'audio/%'),
			(SELECT COUNT(*) FROM attachments WHERE voice IS NOT NULL OR mime LIKE 'audio/%'),
			(SELECT COALESCE(SUM(size), 0) FROM attachments WHERE voice IS NOT NULL OR mime LIKE 'audio/%'),
			(SELECT COUNT(*) FROM avatars),
			(SELECT COALESCE(SUM(size), 0) FROM avatars),
			(SELECT COUNT(*) FROM audit_logs),
			(SELECT COUNT(*) FROM users WHERE active = 1),
			(SELECT COUNT(*) FROM users WHERE active = 0),
			(SELECT COUNT(*) FROM login_security WHERE locked_until IS NOT NULL AND locked_until > ?),
			(SELECT COUNT(*) FROM projects),
			(SELECT COUNT(*) FROM tasks)
	`, nowStr).Scan(
		&attCount, &attBytes, &voiceCount, &voiceBytes,
		&picCount, &picBytes, &auditCount,
		&usersActive, &usersStopped, &usersLocked,
		&projectsCount, &tasksCount,
	)
	filesCount := attCount + voiceCount
	filesBytes := attBytes + voiceBytes

	httpx.Success(w, map[string]any{
		"users_active":  usersActive,
		"users_stopped": usersStopped,
		"users_locked":  usersLocked,
		"projects":      projectsCount,
		"tasks":         tasksCount,
		"files":         filesCount,
		"files_bytes":   filesBytes,
		"audit_entries": auditCount,
		"storage": map[string]any{
			"attachments_bytes": attBytes,
			"attachments":       attCount,
			"pictures_bytes":    picBytes,
			"pictures":          picCount,
			"voice_bytes":       voiceBytes,
			"voice":             voiceCount,
			"audit_bytes":       auditCount * 512,
		},
		"storage_quota_bytes": int64(5 * 1024 * 1024 * 1024),
		"server_version":      "Go " + runtime.Version(),
	})
}

func (s *Service) HandleAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !u.IsCEO() && u.Role != "admin" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.audit_forbidden"))
		return
	}

	q := r.URL.Query()
	kindFilter := strings.TrimSpace(q.Get("kind"))
	actionFilter := strings.TrimSpace(q.Get("action"))
	actorIDStr := strings.TrimSpace(q.Get("actor_id"))
	ipFilter := strings.TrimSpace(q.Get("ip"))
	textQ := strings.TrimSpace(q.Get("q"))
	fromDay := strings.TrimSpace(q.Get("from"))
	toDay := strings.TrimSpace(q.Get("to"))

	var where []string
	var args []any

	if kindFilter != "" {
		var acts []string
		for k, v := range auditActionKind {
			if v == kindFilter {
				acts = append(acts, expandAuditActionAliases(k)...)
			}
		}
		if len(acts) == 0 {
			httpx.Success(w, []map[string]any{})
			return
		}
		where = append(where, fmt.Sprintf("a.action IN (%s)", dbx.Placeholders(len(acts))))
		for _, a := range acts {
			args = append(args, a)
		}
	}
	if actionFilter != "" {
		acts := expandAuditActionAliases(normalizeAuditAction(actionFilter))
		where = append(where, fmt.Sprintf("a.action IN (%s)", dbx.Placeholders(len(acts))))
		for _, a := range acts {
			args = append(args, a)
		}
	}
	if actorIDStr != "" {
		aid, err := strconv.ParseInt(actorIDStr, 10, 64)
		if err != nil || aid <= 0 {
			httpx.Success(w, []map[string]any{})
			return
		}
		where = append(where, "a.actor_id = ?")
		args = append(args, aid)
	}
	if ipFilter != "" {
		ipPrefix := strings.TrimSuffix(ipFilter, "*")
		if ipPrefix != "" {
			where = append(where, "a.ip LIKE ?")
			args = append(args, ipPrefix+"%")
		}
	}
	if fromDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", fromDay, support.WorkingLocation("Asia/Baghdad")); err == nil {
			where = append(where, "a.at >= ?")
			args = append(args, dbx.FormatDBTime(t.UTC()))
		}
	}
	if toDay != "" {
		if t, err := time.ParseInLocation("2006-01-02", toDay, support.WorkingLocation("Asia/Baghdad")); err == nil {
			where = append(where, "a.at < ?")
			args = append(args, dbx.FormatDBTime(t.AddDate(0, 0, 1).UTC()))
		}
	}

	sqlQuery := `
		SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
		       u.name, u.name_en, u.role, u.email
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_id`
	if len(where) > 0 {
		sqlQuery += " WHERE " + strings.Join(where, " AND ")
	}
	sqlQuery += " ORDER BY a.at DESC, a.id DESC"
	if textQ == "" && ipFilter == "" {
		sqlQuery += " LIMIT 200"
	}

	rows, err := s.db.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		raw, ok := scanAuditRow(rows)
		if !ok {
			continue
		}
		action := raw.action
		kind := auditActionKind[action]
		if kindFilter != "" && kind != kindFilter {
			continue
		}
		if actionFilter != "" && action != normalizeAuditAction(actionFilter) {
			continue
		}
		if actorIDStr != "" {
			aid, err := strconv.ParseInt(actorIDStr, 10, 64)
			if err != nil || raw.actorID == nil || *raw.actorID != aid {
				continue
			}
		}
		if ipFilter != "" && !support.IPMatches(strValPtr(raw.ip), ipFilter) {
			continue
		}
		if fromDay != "" && raw.baghdadDay != "" && raw.baghdadDay < fromDay {
			continue
		}
		if toDay != "" && raw.baghdadDay != "" && raw.baghdadDay > toDay {
			continue
		}
		if textQ != "" && !auditMatchesQuery(textQ, raw) {
			continue
		}
		out = append(out, raw.toJSON())
		if len(out) >= 200 {
			break
		}
	}
	httpx.Success(w, out)
}

func (s *Service) HandleAuditDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	if !u.IsCEO() && u.Role != "admin" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.admin.audit_forbidden"))
		return
	}
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
		       u.name, u.name_en, u.role, u.email
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.actor_id
		WHERE a.id = ?
	`, id)
	entry, ok := scanAuditRow(row)
	if !ok {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.admin.audit_not_found"))
		return
	}

	relatedActor := []map[string]any{}
	if entry.actorID != nil && *entry.actorID > 0 {
		arows, _ := s.db.QueryContext(ctx, `
			SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
			       u.name, u.name_en, u.role, u.email
			FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_id
			WHERE a.actor_id = ? AND a.id != ?
			ORDER BY a.at DESC, a.id DESC LIMIT 100
		`, *entry.actorID, id)
		if arows != nil {
			for arows.Next() {
				if e, ok := scanAuditRow(arows); ok && e.baghdadDay == entry.baghdadDay {
					relatedActor = append(relatedActor, e.toJSON())
					if len(relatedActor) >= 20 {
						break
					}
				}
			}
			_ = arows.Close()
		}
	}

	relatedTarget := []map[string]any{}
	var trows *sql.Rows
	if entry.targetID != nil {
		if entry.targetType == "user" || entry.targetType == "login" {
			otherType := "login"
			if entry.targetType == "login" {
				otherType = "user"
			}
			trows, _ = s.db.QueryContext(ctx, `
				SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
				       u.name, u.name_en, u.role, u.email
				FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_id
				WHERE a.id != ? AND ((a.target_type = ? AND a.target_id = ?) OR (a.target_type = ? AND a.target_id = ?))
				ORDER BY a.at DESC, a.id DESC LIMIT 20
			`, id, entry.targetType, *entry.targetID, otherType, *entry.targetID)
		} else {
			trows, _ = s.db.QueryContext(ctx, `
				SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
				       u.name, u.name_en, u.role, u.email
				FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_id
				WHERE a.id != ? AND a.target_type = ? AND a.target_id = ?
				ORDER BY a.at DESC, a.id DESC LIMIT 20
			`, id, entry.targetType, *entry.targetID)
		}
	} else {
		trows, _ = s.db.QueryContext(ctx, `
			SELECT a.id, a.actor_id, a.action, a.target_type, a.target_id, a.target_label, a.changes, a.at, a.ip, a.user_agent, a.session_id, a.request_id,
			       u.name, u.name_en, u.role, u.email
			FROM audit_logs a LEFT JOIN users u ON u.id = a.actor_id
			WHERE a.id != ? AND a.target_type = ? AND a.target_id IS NULL AND a.target_label = ?
			ORDER BY a.at DESC, a.id DESC LIMIT 20
		`, id, entry.targetType, entry.targetLabel)
	}
	if trows != nil {
		for trows.Next() {
			if e, ok := scanAuditRow(trows); ok {
				relatedTarget = append(relatedTarget, e.toJSON())
			}
		}
		_ = trows.Close()
	}

	out := entry.toJSON()
	out["related_by_actor"] = relatedActor
	out["related_by_target"] = relatedTarget
	httpx.Success(w, out)
}

func (s *Service) loadAdminUsers(ctx context.Context, viewer *store.User) ([]map[string]any, error) {
	var (
		users  []*store.User
		err    error
		secMap map[int64]secInfo
	)
	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			users, err = store.LoadAllUsers(ctx, s.db, s.cfg.AppURL)
		}()
		go func() {
			defer wg.Done()
			secMap = s.loadSecurityMap(ctx)
		}()
		wg.Wait()
	} else {
		users, err = store.LoadAllUsers(ctx, s.db, s.cfg.AppURL)
		if err == nil {
			secMap = s.loadSecurityMap(ctx)
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(users))
	for _, usr := range users {
		item := store.UserJSON(usr, viewer)
		if sec, ok := secMap[usr.ID]; ok {
			item["failed_logins"] = sec.failed
			if sec.lockedUntil != nil {
				item["locked_until"] = *sec.lockedUntil
			}
			if sec.lastLoginAt != nil {
				item["last_login_at"] = *sec.lastLoginAt
			}
		} else {
			item["failed_logins"] = 0
		}
		if cr := support.FormatISO(usr.CreatedAt); cr != nil {
			item["created_at"] = *cr
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *Service) loadSingleAdminUser(ctx context.Context, id int64, viewer *store.User) (map[string]any, error) {
	usr, err := store.FindUserByID(ctx, s.db, s.cfg.AppURL, id)
	if err != nil || usr == nil {
		return nil, err
	}
	secMap := s.loadSecurityMap(ctx)
	item := store.UserJSON(usr, viewer)
	if sec, ok := secMap[usr.ID]; ok {
		item["failed_logins"] = sec.failed
		if sec.lockedUntil != nil {
			item["locked_until"] = *sec.lockedUntil
		}
		if sec.lastLoginAt != nil {
			item["last_login_at"] = *sec.lastLoginAt
		}
	} else {
		item["failed_logins"] = 0
	}
	if cr := support.FormatISO(usr.CreatedAt); cr != nil {
		item["created_at"] = *cr
	}
	return item, nil
}

type secInfo struct {
	failed      int
	lockedUntil *string
	lastLoginAt *string
}

func (s *Service) loadSecurityMap(ctx context.Context) map[int64]secInfo {
	res := map[int64]secInfo{}
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, failed_logins, locked_until, last_login_at FROM login_security`)
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var failed int
		var lockedRaw, lastRaw any
		if rows.Scan(&uid, &failed, &lockedRaw, &lastRaw) == nil {
			res[uid] = secInfo{
				failed:      failed,
				lockedUntil: support.FormatISO(dbx.ParseTime(lockedRaw)),
				lastLoginAt: support.FormatISO(dbx.ParseTime(lastRaw)),
			}
		}
	}
	return res
}

func (s *Service) validateTeamID(ctx context.Context, raw any) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	if str, ok := raw.(string); ok && strings.TrimSpace(str) == "" {
		return nil, nil
	}
	if n, ok := toIntStrict(raw); ok && n > 0 {
		id := int64(n)
		var cnt int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM teams WHERE id = ?`, id).Scan(&cnt)
		if cnt == 0 {
			return nil, httpx.ErrValidation("team_id", "api.admin.team_not_found")
		}
		return &id, nil
	}
	return nil, httpx.ErrValidation("team_id", "api.admin.team_not_found")
}

func (s *Service) validateLeadID(ctx context.Context, raw any) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	n, ok := toIntStrict(raw)
	id := int64(n)
	if !ok || id <= 0 {
		return nil, httpx.ErrValidation("lead_id", "api.admin.team_lead_invalid")
	}
	var role string
	var active int
	err := s.db.QueryRowContext(ctx, `SELECT role, active FROM users WHERE id = ?`, id).Scan(&role, &active)
	if err != nil || active == 0 || role != "lead" {
		return nil, httpx.ErrValidation("lead_id", "api.admin.team_lead_invalid")
	}
	return &id, nil
}

type scanner interface{ Scan(dest ...any) error }

type auditEntryRow struct {
	id           int64
	actorID      *int64
	actorName    *string
	actorNameEn  *string
	actorEmail   *string
	action       string
	targetType   string
	targetID     *int64
	targetLabel  string
	changes      []any
	changesWords []string
	atISO        *string
	baghdadDay   string
	ip           *string
	userAgent    *string
	sessionID    *string
	requestID    *string
}

func (e auditEntryRow) toJSON() map[string]any {
	target := store.CleanMap(map[string]any{
		"type":  e.targetType,
		"id":    e.targetID,
		"label": e.targetLabel,
	})
	var changesVal any = nil
	if len(e.changes) > 0 {
		changesVal = e.changes
	}
	return store.CleanMap(map[string]any{
		"id":         e.id,
		"actor_id":   e.actorID,
		"action":     e.action,
		"target":     target,
		"changes":    changesVal,
		"at":         e.atISO,
		"ip":         e.ip,
		"user_agent": e.userAgent,
		"session_id": e.sessionID,
		"request_id": e.requestID,
	})
}

func scanAuditRow(sc scanner) (auditEntryRow, bool) {
	var id int64
	var actorID, targetID sql.NullInt64
	var action, targetType, targetLabel string
	var changesRaw, ip, ua, sid, reqID, aname, anameEn, arole, aemail sql.NullString
	var atRaw any
	if err := sc.Scan(&id, &actorID, &action, &targetType, &targetID, &targetLabel, &changesRaw, &atRaw, &ip, &ua, &sid, &reqID, &aname, &anameEn, &arole, &aemail); err != nil {
		return auditEntryRow{}, false
	}
	var changes []any
	var changesWords []string
	if changesRaw.Valid && strings.TrimSpace(changesRaw.String) != "" {
		_ = json.Unmarshal([]byte(changesRaw.String), &changes)
		for _, item := range changes {
			if m, ok := item.(map[string]any); ok {
				for _, ck := range []string{"field", "from", "to"} {
					if v, ok := m[ck]; ok && v != nil {
						if s := fmt.Sprint(v); s != "" {
							changesWords = append(changesWords, s)
						}
					}
				}
			}
		}
	}
	t := dbx.ParseTime(atRaw)
	bDay := ""
	if t != nil {
		bDay = support.ToBaghdadDate(*t)
	}
	return auditEntryRow{
		id:           id,
		actorID:      nullIntPtr(actorID),
		actorName:    nullStrPtr(aname),
		actorNameEn:  nullStrPtr(anameEn),
		actorEmail:   nullStrPtr(aemail),
		action:       normalizeAuditAction(action),
		targetType:   targetType,
		targetID:     nullIntPtr(targetID),
		targetLabel:  targetLabel,
		changes:      changes,
		changesWords: changesWords,
		atISO:        support.FormatISO(t),
		baghdadDay:   bDay,
		ip:           nullStrPtr(ip),
		userAgent:    nullStrPtr(ua),
		sessionID:    nullStrPtr(sid),
		requestID:    nullStrPtr(reqID),
	}, true
}

func auditMatchesQuery(q string, raw auditEntryRow) bool {
	fields := []string{
		raw.targetLabel,
		strValPtr(raw.actorName),
		strValPtr(raw.actorNameEn),
		strValPtr(raw.actorEmail),
		strValPtr(raw.ip),
		raw.action,
	}
	if w, ok := auditActionWords[raw.action]; ok {
		fields = append(fields, w[0], w[1])
	}
	fields = append(fields, raw.changesWords...)
	return support.SearchMatches(q, fields...)
}

func strValPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func mapImportRole(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pm", "project manager", "department manager", "مدير مشروع", "مدير القسم":
		return "pm"
	case "monitor", "monitoring & evaluation", "monitoring and evaluation", "المتابعة", "مسؤول المتابعة والتقييم":
		return "monitor"
	case "lead", "team leader", "team lead", "قائد فريق", "قائد الفريق":
		return "lead"
	case "employee", "موظف":
		return "employee"
	case "ceo", "المدير التنفيذي":
		return "ceo"
	case "admin", "system admin", "مسؤول النظام":
		return "admin"
	default:
		return raw
	}
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }
func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func optStr(v any) *string {
	if v == nil {
		return nil
	}
	s := strings.TrimSpace(strVal(v))
	if s == "" {
		return nil
	}
	return &s
}
func nullStrPtr(s sql.NullString) *string {
	if !s.Valid || s.String == "" {
		return nil
	}
	v := s.String
	return &v
}
func nullIntPtr(i sql.NullInt64) *int64 {
	if !i.Valid {
		return nil
	}
	v := i.Int64
	return &v
}
func eqStrPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
func eqInt64Ptr(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
func maskPtr(p *string) *string {
	if p == nil || *p == "" {
		return nil
	}
	v := "***"
	return &v
}
func toIntStrict(v any) (int, bool) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
		if f, err := x.Float64(); err == nil && float64(int(f)) == f {
			return int(f), true
		}
	case float64:
		if float64(int(x)) == x {
			return int(x), true
		}
	case int:
		return x, true
	case int64:
		return int(x), true
	}
	return 0, false
}
