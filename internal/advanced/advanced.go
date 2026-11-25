package advanced

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"followapi/internal/audit"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/i18n"
	"followapi/internal/store"
	"followapi/internal/support"
	"followapi/internal/ws"
)

const HistoryKept = 300

var FeatureKeys = []string{"chat", "group_chats", "voice_messages", "bugs", "reviews", "announcements", "requests", "github", "welcome"}
var Priorities = []string{"high", "medium", "low"}
var Severities = []string{"critical", "high", "medium", "low"}
var Approvers = []string{"pm", "lead", "either"}
var BaseJobs = []string{"pm", "monitor", "lead", "employee"}
var StaffRoles = []string{"pm", "monitor", "lead", "employee", "admin"}
var RoleColors = []string{"todo", "progress", "review", "testing", "done", "new"}

var NotificationKinds = []string{
	"task_assigned", "task_status", "task_comment", "task_due_changed",
	"task_due_soon", "task_overdue", "approval_requested", "approval_decided",
	"project_added", "project_pm", "project_plan", "announcement",
	"bug_reported", "bug_assigned", "bug_status", "bug_late",
	"chat_mention", "dependency_done", "review_final", "team_joined",
	"account_locked", "github_pr_merged", "note_sent", "note_answered",
	"team_rated", "person_rated", "birthday_soon", "birthday_today",
}

var RequiredNotificationKinds = map[string]bool{
	"task_assigned":      true,
	"task_overdue":       true,
	"approval_requested": true,
	"approval_decided":   true,
	"project_added":      true,
	"announcement":       true,
	"bug_assigned":       true,
	"note_sent":          true,
	"note_answered":      true,
}

var ActionDefs = map[string][]string{
	"create_project":    {"pm", "monitor"},
	"close_project":     {"pm"},
	"delete_task":       {"pm", "lead"},
	"reassign_task":     {"pm", "lead"},
	"change_due":        {"pm", "lead"},
	"approve_done":      {"pm", "lead"},
	"post_announcement": {"monitor", "admin"},
	"create_group_chat": {"pm", "monitor", "lead"},
	"see_scores":        {"pm", "monitor", "lead"},
	"write_reviews":     {"pm", "lead"},
	"give_ratings":      {"pm", "monitor"},
	"see_reports":       {"pm", "monitor", "lead"},
	"export_csv":        {"pm", "monitor", "lead", "admin"},
	"report_bugs":       {"pm", "monitor", "lead", "employee"},
	"bug_severity":      {"pm", "lead"},
}

var NavRoles = map[string][]string{
	"dashboard":     {"ceo", "pm", "monitor", "lead"},
	"my-tasks":      {"pm", "lead", "employee"},
	"projects":      {"ceo", "pm", "monitor", "lead", "employee"},
	"chat":          {"ceo", "pm", "monitor", "lead", "employee"},
	"bugs":          {"ceo", "pm", "monitor", "lead", "employee"},
	"quick-actions": {"monitor"},
	"announcements": {"ceo", "pm", "monitor", "lead", "employee", "admin"},
	"performance":   {"ceo", "pm", "monitor", "lead", "employee"},
	"teams":         {"ceo", "pm", "monitor"},
	"reports":       {"ceo", "pm", "monitor", "lead"},
	"monitoring":    {"monitor"},
	"trials":        {"monitor"},
	"inbox":         {"ceo"},
	"requests":      {"pm", "monitor", "lead", "employee", "admin"},
	"users":         {"monitor"},
	"admin":         {"admin"},
	"audit":         {"ceo", "admin"},
	"advanced":      {"ceo"},
}

var SettingRanges = map[string][2]int{
	"due_soon_days": {1, 7},
	"lock_after":    {3, 10},
	"lock_minutes":  {5, 120},
}

type Service struct {
	db           *dbx.DB
	audit        *audit.Service
	ws           *ws.Hub
	mu           sync.RWMutex
	cachedRules  map[string]any
	cacheExpires time.Time
}

func New(db *dbx.DB, auditSvc *audit.Service) *Service {
	return &Service{db: db, audit: auditSvc}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func (s *Service) InvalidateCache() {
	s.mu.Lock()
	s.cachedRules = nil
	s.cacheExpires = time.Time{}
	s.mu.Unlock()
}

func DefaultRules() map[string]any {
	notify := map[string]any{}
	for _, k := range NotificationKinds {
		notify[k] = append([]string{}, store.SystemRoles...)
	}
	features := map[string]any{}
	for _, f := range FeatureKeys {
		features[f] = true
	}
	perms := map[string]any{}
	for k, v := range ActionDefs {
		perms[k] = append([]string{}, v...)
	}
	return map[string]any{
		"work": map[string]any{
			"days":     []any{0, 1, 2, 3, 4},
			"from":     "08:00",
			"to":       "16:00",
			"holidays": []any{},
		},
		"due_days":    map[string]any{"high": 3, "medium": 7, "low": 14},
		"fix_days":    map[string]any{"critical": 1, "high": 3, "medium": 7, "low": 14},
		"tasks":       map[string]any{"approver": "either", "employee_reopen": true, "due_reason": false, "wip_limit": 0},
		"permissions": perms,
		"notify":      notify,
		"features":    features,
		"security":    map[string]any{"due_soon_days": 1, "lock_after": 5, "lock_minutes": 15, "password_min": 8},
		"names":       map[string]any{"task": "TSK", "project": "PRJ", "bug": "BUG"},
		"banner":      nil,
		"read_only":   map[string]any{"on": false, "message": ""},
		"roles":       []any{},
	}
}

func (s *Service) RulesOf(stored map[string]any, adminSecurity map[string]int) map[string]any {
	d := DefaultRules()
	if stored == nil {
		stored = map[string]any{}
	}
	out := map[string]any{
		"work":        mergeStringMap(d["work"].(map[string]any), asMap(stored["work"])),
		"due_days":    mergeStringMap(d["due_days"].(map[string]any), asMap(stored["due_days"])),
		"fix_days":    mergeStringMap(d["fix_days"].(map[string]any), asMap(stored["fix_days"])),
		"tasks":       mergeStringMap(d["tasks"].(map[string]any), asMap(stored["tasks"])),
		"permissions": mergeStringMap(d["permissions"].(map[string]any), asMap(stored["permissions"])),
		"notify":      mergeStringMap(d["notify"].(map[string]any), asMap(stored["notify"])),
		"features":    mergeStringMap(d["features"].(map[string]any), asMap(stored["features"])),
		"names":       mergeStringMap(d["names"].(map[string]any), asMap(stored["names"])),
		"read_only":   mergeStringMap(d["read_only"].(map[string]any), asMap(stored["read_only"])),
		"banner":      d["banner"],
		"roles":       d["roles"],
	}
	sec := mergeStringMap(d["security"].(map[string]any), asMap(stored["security"]))
	for k, v := range adminSecurity {
		sec[k] = v
	}
	out["security"] = sec
	if _, ok := stored["banner"]; ok {
		out["banner"] = stored["banner"]
	}
	if r, ok := stored["roles"].([]any); ok {
		out["roles"] = r
	}
	normalizeNumericMaps(out)
	return out
}

func (s *Service) CurrentRules(ctx context.Context) map[string]any {
	useCache := !s.db.IsSQLite()
	if useCache {
		s.mu.RLock()
		if s.cachedRules != nil && time.Now().Before(s.cacheExpires) {
			cloned := deepCloneMap(s.cachedRules)
			s.mu.RUnlock()
			return cloned
		}
		s.mu.RUnlock()
	}

	var rawRules string
	var appSetting store.AppSetting
	if useCache {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.db.QueryRowContext(ctx, `SELECT rules FROM advanced_settings WHERE id = 1`).Scan(&rawRules)
		}()
		go func() {
			defer wg.Done()
			appSetting, _ = store.LoadAppSetting(ctx, s.db)
		}()
		wg.Wait()
	} else {
		_ = s.db.QueryRowContext(ctx, `SELECT rules FROM advanced_settings WHERE id = 1`).Scan(&rawRules)
		appSetting, _ = store.LoadAppSetting(ctx, s.db)
	}

	var stored map[string]any
	if rawRules != "" {
		_ = json.Unmarshal([]byte(rawRules), &stored)
	}
	res := s.RulesOf(stored, map[string]int{
		"due_soon_days": appSetting.DueSoonDays,
		"lock_after":    appSetting.LockAfter,
		"lock_minutes":  appSetting.LockMinutes,
	})
	if useCache {
		s.mu.Lock()
		s.cachedRules = deepCloneMap(res)
		s.cacheExpires = time.Now().Add(120 * time.Second)
		s.mu.Unlock()
	}
	return res
}

func deepCloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCloneAny(item)
		}
		return out
	case []string:
		out := make([]string, len(x))
		copy(out, x)
		return out
	default:
		return v
	}
}

func deepCloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCloneAny(v)
	}
	return out
}

func (s *Service) PasswordMin(ctx context.Context) int {
	sec := asMap(s.CurrentRules(ctx)["security"])
	if v, ok := toInt(sec["password_min"]); ok && v >= 6 {
		return v
	}
	return 8
}

func (s *Service) FeatureIsOn(ctx context.Context, key string) bool {
	feats := asMap(s.CurrentRules(ctx)["features"])
	if v, ok := feats[key].(bool); ok {
		return v
	}
	return true
}

func (s *Service) RequireFeature(ctx context.Context, user *store.User, featureKey string) error {
	if user.IsCEO() || s.FeatureIsOn(ctx, featureKey) {
		return nil
	}
	return &httpx.APIError{Status: http.StatusForbidden, Code: "FEATURE_OFF", Key: "api.advanced.feature_off"}
}

func (s *Service) FindRoleByKey(ctx context.Context, key string) map[string]any {
	if key == "" {
		return nil
	}
	roles, _ := s.CurrentRules(ctx)["roles"].([]any)
	for _, r := range roles {
		rm := asMap(r)
		if strVal(rm["key"]) == key {
			return rm
		}
	}
	return nil
}

func (s *Service) RoleMay(ctx context.Context, user *store.User, action string) bool {
	if user.IsCEO() {
		return true
	}
	if user.CustomRole == nil || *user.CustomRole == "" {
		return true
	}
	role := s.FindRoleByKey(ctx, *user.CustomRole)
	if role == nil {
		return true
	}
	actions, _ := role["actions"].([]any)
	for _, a := range actions {
		if strVal(a) == action {
			return true
		}
	}
	return false
}

func (s *Service) RequireAction(ctx context.Context, user *store.User, action string) error {
	if s.RoleMay(ctx, user, action) {
		return nil
	}
	role := s.FindRoleByKey(ctx, *user.CustomRole)
	name := strVal(role["name"])
	if httpx.LocaleFrom(ctx) == "en" {
		if ne := strVal(role["name_en"]); ne != "" {
			name = ne
		}
	}
	return httpx.ErrForbidden("api.advanced.custom_role_forbidden", map[string]any{"role": name})
}

func (s *Service) CodePrefix(ctx context.Context, kind string) string {
	names := asMap(s.CurrentRules(ctx)["names"])
	if v := strVal(names[kind]); v != "" {
		return v
	}
	switch kind {
	case "project":
		return "PRJ"
	case "task":
		return "TSK"
	case "bug":
		return "BUG"
	default:
		return strings.ToUpper(kind)
	}
}

func (s *Service) JobGets(ctx context.Context, role, kind string) bool {
	if RequiredNotificationKinds[kind] {
		return true
	}
	notify := asMap(s.CurrentRules(ctx)["notify"])
	arr, ok := notify[kind].([]any)
	if !ok {
		return true
	}
	for _, v := range arr {
		if strVal(v) == role {
			return true
		}
	}
	return false
}

func (s *Service) ReadOnlyStatus(ctx context.Context) map[string]any {
	ro := asMap(s.CurrentRules(ctx)["read_only"])
	if b, _ := ro["on"].(bool); b {
		return ro
	}
	return nil
}

func (s *Service) TaskConfig(ctx context.Context) (approver string, employeeReopen, dueReason bool, wipLimit int) {
	t := asMap(s.CurrentRules(ctx)["tasks"])
	approver = strVal(t["approver"])
	if approver == "" {
		approver = "either"
	}
	if b, ok := t["employee_reopen"].(bool); ok {
		employeeReopen = b
	} else {
		employeeReopen = true
	}
	dueReason, _ = t["due_reason"].(bool)
	wipLimit, _ = toInt(t["wip_limit"])
	return
}

func (s *Service) LiveFor(ctx context.Context, user *store.User) map[string]any {
	rules := s.CurrentRules(ctx)
	var activeBanner any
	if bm := asMap(rules["banner"]); bm != nil {
		roles := toStringSlice(bm["roles"])
		roleMatch := len(roles) == 0 || store.UserHasRole(user.Role, roles)
		until := strVal(bm["until"])
		notExpired := true
		if until != "" {
			if parsed := dbx.ParseTimeString(until); parsed != nil {
				notExpired = parsed.After(time.Now().UTC())
			} else {
				notExpired = false
			}
		}
		if roleMatch && notExpired {
			activeBanner = bm
		}
	}
	me := map[string]any{"role": user.Role}
	if user.CustomRole != nil && *user.CustomRole != "" {
		me["custom_role"] = *user.CustomRole
	}
	sec := asMap(rules["security"])
	pwMin, _ := toInt(sec["password_min"])
	if pwMin == 0 {
		pwMin = 8
	}
	return map[string]any{
		"features":     rules["features"],
		"banner":       activeBanner,
		"read_only":    rules["read_only"],
		"roles":        rules["roles"],
		"me":           me,
		"password_min": pwMin,
	}
}

func (s *Service) HandleLive(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	httpx.Success(w, s.LiveFor(r.Context(), u))
}

func (s *Service) HandleGet(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.advanced.forbidden"))
		return
	}
	httpx.Success(w, s.answer(r.Context()))
}

func (s *Service) HandleHistory(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.advanced.forbidden"))
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, at, by_id, by_name, by_name_en, how, changes
		FROM advanced_settings_logs
		ORDER BY id DESC
		LIMIT ?
	`, HistoryKept)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	list := []map[string]any{}
	for rows.Next() {
		var id, byID int64
		var atRaw any
		var byName, changesRaw string
		var byNameEn, how sql.NullString
		if err := rows.Scan(&id, &atRaw, &byID, &byName, &byNameEn, &how, &changesRaw); err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		by := map[string]any{"id": byID, "name": byName}
		if byNameEn.Valid && byNameEn.String != "" {
			by["name_en"] = byNameEn.String
		}
		var changes any = []any{}
		_ = json.Unmarshal([]byte(changesRaw), &changes)
		entry := map[string]any{
			"id":      id,
			"at":      store.IsoTime(dbx.ParseTime(atRaw)),
			"by":      by,
			"changes": changes,
		}
		if how.Valid && how.String != "" {
			entry["how"] = how.String
		}
		list = append(list, entry)
	}
	httpx.Success(w, list)
}

func (s *Service) HandleSave(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.advanced.forbidden"))
		return
	}
	payload, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var how *string
	if hv, ok := payload["how"]; ok && hv != nil {
		hs, okStr := hv.(string)
		if !okStr || (hs != "import" && hs != "reset") {
			httpx.HandleErr(w, r, &httpx.APIError{Status: 422, Code: "VALIDATION", Key: "api.advanced.invalid_how"})
			return
		}
		how = &hs
	}

	var sent map[string]any
	if how == nil || *how != "reset" {
		m, ok := payload["rules"].(map[string]any)
		if !ok {
			httpx.HandleErr(w, r, &httpx.APIError{Status: 422, Code: "VALIDATION", Key: "api.advanced.invalid_payload"})
			return
		}
		sent = m
	}

	next := s.RulesOf(sent, nil)
	isAr := httpx.LocaleFrom(r.Context()) == "ar"
	problems := checkRules(next)
	if len(problems) > 0 {
		errs := map[string]string{}
		for k, pair := range problems {
			if isAr {
				errs[k] = pair[0]
			} else {
				errs[k] = pair[1]
			}
		}
		httpx.HandleErr(w, r, &httpx.APIError{Status: 422, Code: "VALIDATION", Key: "api.advanced.fix_errors", Errors: errs})
		return
	}

	before := s.CurrentRules(r.Context())
	changes := diffRules(before, next)
	if len(changes) == 0 {
		httpx.Success(w, s.answer(r.Context()))
		return
	}

	beforeRoles, _ := before["roles"].([]any)
	nextRoles, _ := next["roles"].([]any)
	for _, nr := range nextRoles {
		nm := asMap(nr)
		for _, pr := range beforeRoles {
			pm := asMap(pr)
			if strVal(pm["key"]) == strVal(nm["key"]) && strVal(pm["base"]) != strVal(nm["base"]) {
				msg := i18n.T(httpx.LocaleFrom(r.Context()), "api.advanced.base_immutable")
				httpx.HandleErr(w, r, &httpx.APIError{
					Status:  422,
					Code:    "VALIDATION",
					Message: msg,
					Errors:  map[string]string{"roles." + strVal(nm["key"]) + ".base": msg},
				})
				return
			}
		}
	}

	nextKeys := map[string]bool{}
	for _, nr := range nextRoles {
		nextKeys[strVal(asMap(nr)["key"])] = true
	}
	var goneRoles []map[string]any
	for _, pr := range beforeRoles {
		pm := asMap(pr)
		if !nextKeys[strVal(pm["key"])] {
			goneRoles = append(goneRoles, pm)
		}
	}

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if len(goneRoles) > 0 && how == nil {
			var inUse []string
			for _, gr := range goneRoles {
				var count int
				_ = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE active = 1 AND custom_role = ?`, strVal(gr["key"])).Scan(&count)
				if count > 0 {
					if isAr {
						inUse = append(inUse, fmt.Sprintf("«%s» (%d)", strVal(gr["name"]), count))
					} else {
						inUse = append(inUse, fmt.Sprintf("\"%s\" (%d)", strVal(gr["name_en"]), count))
					}
				}
			}
			if len(inUse) > 0 {
				return httpx.ErrConflict("api.advanced.role_in_use") // formatted in caller
			}
		}
		if len(goneRoles) > 0 {
			for _, gr := range goneRoles {
				_, err := tx.ExecContext(r.Context(), `UPDATE users SET custom_role = NULL WHERE custom_role = ?`, strVal(gr["key"]))
				if err != nil {
					return err
				}
			}
		}

		sec := asMap(next["security"])
		dueSoon, _ := toInt(sec["due_soon_days"])
		lockAfter, _ := toInt(sec["lock_after"])
		lockMinutes, _ := toInt(sec["lock_minutes"])
		pwMin, _ := toInt(sec["password_min"])
		nowStr := dbx.FormatDBTime(time.Now().UTC())

		if _, err := tx.ExecContext(r.Context(), `
			UPDATE app_settings SET due_soon_days = ?, lock_after = ?, lock_minutes = ?, updated_at = ? WHERE id = 1
		`, dueSoon, lockAfter, lockMinutes, nowStr); err != nil {
			return err
		}

		stored := changedRules(next)
		if pwMin != 8 {
			stored["security"] = sec
		} else {
			delete(stored, "security")
		}
		storedBytes, _ := json.Marshal(stored)

		var existingID int
		err := tx.QueryRowContext(r.Context(), `SELECT id FROM advanced_settings WHERE id = 1`).Scan(&existingID)
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(r.Context(), `INSERT INTO advanced_settings (id, rules, updated_by, updated_at) VALUES (1, ?, ?, ?)`,
				string(storedBytes), u.ID, nowStr)
		} else if err == nil {
			_, err = tx.ExecContext(r.Context(), `UPDATE advanced_settings SET rules = ?, updated_by = ?, updated_at = ? WHERE id = 1`,
				string(storedBytes), u.ID, nowStr)
		}
		if err != nil {
			return err
		}

		changesBytes, _ := json.Marshal(changes)
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO advanced_settings_logs (at, by_id, by_name, by_name_en, how, changes)
			VALUES (?, ?, ?, ?, ?, ?)
		`, nowStr, u.ID, u.Name, u.NameEn, how, string(changesBytes)); err != nil {
			return err
		}

		_, _ = tx.ExecContext(r.Context(), `
			DELETE FROM advanced_settings_logs WHERE id NOT IN (
				SELECT id FROM (SELECT id FROM advanced_settings_logs ORDER BY id DESC LIMIT ?) sub
			)
		`, HistoryKept)

		return nil
	})
	if err != nil {
		if err.Error() == "api.advanced.role_in_use" {
			var inUse []string
			for _, gr := range goneRoles {
				var count int
				_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE active = 1 AND custom_role = ?`, strVal(gr["key"])).Scan(&count)
				if count > 0 {
					if isAr {
						inUse = append(inUse, fmt.Sprintf("«%s» (%d)", strVal(gr["name"]), count))
					} else {
						inUse = append(inUse, fmt.Sprintf("\"%s\" (%d)", strVal(gr["name_en"]), count))
					}
				}
			}
			sep := ", "
			if isAr {
				sep = "، "
			}
			httpx.HandleErr(w, r, &httpx.APIError{Status: 409, Code: "BAD_STATE", Key: "api.advanced.role_in_use", Params: map[string]any{"list": strings.Join(inUse, sep)}})
			return
		}
		httpx.HandleErr(w, r, err)
		return
	}

	s.InvalidateCache()
	store.InvalidateUsersCache()

	var auditChanges []audit.Change
	for _, c := range changes {
		from := shortVal(c["from"])
		to := shortVal(c["to"])
		auditChanges = append(auditChanges, audit.Change{
			Field: strVal(c["key"]),
			From:  &from,
			To:    &to,
		})
	}
	s.audit.Log(r.Context(), &u.ID, "settings_changed", "settings", nil, "الإعدادات المتقدمة", auditChanges)
	s.ws.NotifySiteLive()
	httpx.Success(w, s.answer(r.Context()))
}

func (s *Service) answer(ctx context.Context) map[string]any {
	out := map[string]any{"rules": s.CurrentRules(ctx)}
	var atRaw any
	var byID int64
	var byName string
	err := s.db.QueryRowContext(ctx, `SELECT at, by_id, by_name FROM advanced_settings_logs ORDER BY id DESC LIMIT 1`).Scan(&atRaw, &byID, &byName)
	if err == nil {
		out["updated_at"] = store.IsoTime(dbx.ParseTime(atRaw))
		out["updated_by"] = map[string]any{"id": byID, "name": byName}
	}
	return out
}

// ReadOnlyMiddleware enforces read-only mode on mutating requests except CEO and own bookkeeping routes.
func (s *Service) ReadOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		u := httpx.UserFrom(r.Context())
		if u == nil || u.IsCEO() || isOwnBookkeepingRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		ro := s.ReadOnlyStatus(r.Context())
		if ro == nil {
			next.ServeHTTP(w, r)
			return
		}
		loc := httpx.LocaleFrom(r.Context())
		prefix := i18n.T(loc, "api.advanced.read_only")
		note := strVal(ro["message"])
		if loc == "en" && strVal(ro["message_en"]) != "" {
			note = strVal(ro["message_en"])
		}
		note = strings.TrimSpace(note)
		fullMsg := prefix
		if note != "" {
			fullMsg = prefix + " " + note
		}
		httpx.HandleErr(w, r, &httpx.APIError{Status: 423, Code: "READ_ONLY", Message: fullMsg})
	})
}

func isOwnBookkeepingRoute(path string) bool {
	p := strings.TrimRight(path, "/")
	if p == "/api/v1/auth/logout" ||
		p == "/api/v1/notifications/read-all" ||
		p == "/api/v1/notifications/preferences" ||
		p == "/api/v1/me/avatar" ||
		p == "/api/v1/me/push-subscriptions" {
		return true
	}
	if strings.HasPrefix(p, "/api/v1/notifications/") && strings.HasSuffix(p, "/read") {
		return true
	}
	if strings.HasPrefix(p, "/api/v1/chats/") && strings.HasSuffix(p, "/read") {
		return true
	}
	if strings.HasPrefix(p, "/api/v1/announcements/") && strings.HasSuffix(p, "/seen") {
		return true
	}
	if strings.HasPrefix(p, "/api/v1/monitoring/notes/") && strings.HasSuffix(p, "/seen") {
		return true
	}
	return false
}

// Validation helpers

func asMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func mergeStringMap(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		out[k] = v
	}
	return out
}

func normalizeNumericMaps(rules map[string]any) {
	for _, key := range []string{"due_days", "fix_days", "security", "tasks"} {
		if m := asMap(rules[key]); m != nil {
			for k, v := range m {
				if n, ok := toInt(v); ok {
					m[k] = n
				}
			}
		}
	}
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		if float64(int(t)) == t {
			return int(t), true
		}
	case json.Number:
		if n, err := strconv.Atoi(t.String()); err == nil {
			return n, true
		}
	}
	return 0, false
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if s, ok2 := v.([]string); ok2 {
			return s
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

var hhmmRegex = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)
var codePrefixRegex = regexp.MustCompile(`^[A-Z]{2,5}$`)
var roleKeyRegex = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)

func checkRules(r map[string]any) map[string][2]string {
	e := map[string][2]string{}
	w := asMap(r["work"])
	daysRaw, _ := w["days"].([]any)
	seenDay := map[int]bool{}
	badDays := false
	for _, d := range daysRaw {
		di, ok := toInt(d)
		if !ok || di < 0 || di > 6 || seenDay[di] {
			badDays = true
			break
		}
		seenDay[di] = true
	}
	if w["days"] == nil || badDays {
		e["work.days"] = [2]string{"أيام غير صحيحة", "Wrong days"}
	} else if len(daysRaw) == 0 {
		e["work.days"] = [2]string{"اختر يوم عمل واحداً على الأقل", "Choose at least one working day"}
	}

	from := strVal(w["from"])
	to := strVal(w["to"])
	if !hhmmRegex.MatchString(from) || !hhmmRegex.MatchString(to) || hhmmMin(from) >= hhmmMin(to) {
		e["work.hours"] = [2]string{"البداية قبل النهاية", "The start comes before the end"}
	}

	holidaysRaw, okH := w["holidays"].([]any)
	if !okH || len(holidaysRaw) > 60 {
		e["work.holidays"] = [2]string{"حتى 60 عطلة", "Up to 60 holidays"}
	} else {
		seenDates := map[string]bool{}
		for _, h := range holidaysRaw {
			hm := asMap(h)
			hd := strVal(hm["date"])
			hn := strings.TrimSpace(strVal(hm["name"]))
			hne := strVal(hm["name_en"])
			if !support.IsRealDay(hd) || hn == "" || support.RuneLen(hn) > 60 || support.RuneLen(hne) > 60 {
				e["work.holidays"] = [2]string{"لكل عطلة تاريخ واسم", "Each holiday needs a date and a name"}
				break
			}
			if seenDates[hd] {
				e["work.holidays"] = [2]string{"عطلتان في اليوم نفسه", "Two holidays on the same day"}
				break
			}
			seenDates[hd] = true
		}
	}

	dueDays := asMap(r["due_days"])
	for _, p := range Priorities {
		if v, ok := toInt(dueDays[p]); !ok || v < 1 || v > 90 {
			e["due_days."+p] = [2]string{"من 1 إلى 90", "1 to 90"}
		}
	}

	fixDays := asMap(r["fix_days"])
	for _, sv := range Severities {
		if v, ok := toInt(fixDays[sv]); !ok || v < 1 || v > 60 {
			e["fix_days."+sv] = [2]string{"من 1 إلى 60", "1 to 60"}
		}
	}
	sevLabels := map[string][2]string{
		"critical": {"حرجة", "critical"},
		"high":     {"عالية", "high"},
		"medium":   {"متوسطة", "medium"},
		"low":      {"منخفضة", "low"},
	}
	for i := 1; i < len(Severities); i++ {
		worse, lighter := Severities[i-1], Severities[i]
		_, ew := e["fix_days."+worse]
		_, el := e["fix_days."+lighter]
		vw, _ := toInt(fixDays[worse])
		vl, _ := toInt(fixDays[lighter])
		if !ew && !el && vl < vw {
			e["fix_days."+lighter] = [2]string{
				"لا يقل عن «" + sevLabels[worse][0] + "»",
				"Not less than " + sevLabels[worse][1],
			}
		}
	}

	tasks := asMap(r["tasks"])
	if !store.UserHasRole(strVal(tasks["approver"]), Approvers) {
		e["tasks.approver"] = [2]string{"اختر واحداً", "Choose one"}
	}
	if _, ok := tasks["employee_reopen"].(bool); !ok {
		e["tasks.employee_reopen"] = [2]string{"نعم أو لا", "Yes or no"}
	}
	if _, ok := tasks["due_reason"].(bool); !ok {
		e["tasks.due_reason"] = [2]string{"نعم أو لا", "Yes or no"}
	}
	if wip, ok := toInt(tasks["wip_limit"]); !ok || wip < 0 || wip > 50 {
		e["tasks.wip_limit"] = [2]string{"من 0 (بلا حد) إلى 50", "0 (no limit) to 50"}
	}

	perms := asMap(r["permissions"])
	for k, v := range perms {
		_, okAction := ActionDefs[k]
		roles := toStringSlice(v)
		if !okAction || v == nil || !subsetOf(roles, StaffRoles) {
			e["permissions."+k] = [2]string{"وظائف غير صحيحة", "Wrong jobs"}
		}
	}

	notify := asMap(r["notify"])
	for k, v := range notify {
		roles := toStringSlice(v)
		if !store.UserHasRole(k, NotificationKinds) || v == nil || !subsetOf(roles, store.SystemRoles) {
			e["notify."+k] = [2]string{"وظائف غير صحيحة", "Wrong jobs"}
		} else if RequiredNotificationKinds[k] && !subsetOf(store.SystemRoles, roles) {
			e["notify."+k] = [2]string{"إشعار مطلوب: يصل الجميع", "A required notification: it reaches everyone"}
		}
	}

	feats := asMap(r["features"])
	for k, v := range feats {
		if !store.UserHasRole(k, FeatureKeys) {
			e["features."+k] = [2]string{"تشغيل أو إيقاف", "On or off"}
		} else if _, ok := v.(bool); !ok {
			e["features."+k] = [2]string{"تشغيل أو إيقاف", "On or off"}
		}
	}

	sec := asMap(r["security"])
	for k, lim := range SettingRanges {
		if v, ok := toInt(sec[k]); !ok || v < lim[0] || v > lim[1] {
			e["security."+k] = [2]string{fmt.Sprintf("من %d إلى %d", lim[0], lim[1]), fmt.Sprintf("%d to %d", lim[0], lim[1])}
		}
	}
	if v, ok := toInt(sec["password_min"]); !ok || v < 6 || v > 32 {
		e["security.password_min"] = [2]string{"من 6 إلى 32", "6 to 32"}
	}

	names := asMap(r["names"])
	seenCodes := map[string]bool{}
	for _, ck := range []string{"task", "project", "bug"} {
		val := strVal(names[ck])
		if !codePrefixRegex.MatchString(val) {
			e["names."+ck] = [2]string{"من 2 إلى 5 أحرف إنجليزية كبيرة", "2 to 5 capital letters A to Z"}
		} else if seenCodes[val] {
			e["names."+ck] = [2]string{"مستعملة لنوع آخر", "Used by another kind"}
		} else {
			seenCodes[val] = true
		}
	}

	if r["banner"] != nil {
		b := asMap(r["banner"])
		bText := strVal(b["text"])
		bTextEn := strVal(b["text_en"])
		bRoles := toStringSlice(b["roles"])
		bTone := strVal(b["tone"])
		bUntil := strVal(b["until"])
		if strings.TrimSpace(bText) == "" || support.RuneLen(bText) > 300 {
			e["banner.text"] = [2]string{"اكتب نص الإعلان (حتى 300 حرف)", "Write the banner (up to 300 letters)"}
		}
		if support.RuneLen(bTextEn) > 300 {
			e["banner.text_en"] = [2]string{"حتى 300 حرف", "Up to 300 letters"}
		}
		if b == nil || b["roles"] == nil || !subsetOf(bRoles, store.SystemRoles) {
			e["banner.roles"] = [2]string{"وظائف غير صحيحة", "Wrong jobs"}
		}
		if bTone != "info" && bTone != "warning" {
			e["banner.tone"] = [2]string{"اختر لوناً", "Choose a colour"}
		}
		if bUntil != "" && dbx.ParseTimeString(bUntil) == nil {
			e["banner.until"] = [2]string{"تاريخ غير صحيح", "Not a valid date"}
		}
	}

	ro := asMap(r["read_only"])
	onVal, okBool := ro["on"].(bool)
	if !okBool {
		e["read_only.on"] = [2]string{"تشغيل أو إيقاف", "On or off"}
	}
	roMsg := strVal(ro["message"])
	roMsgEn := strVal(ro["message_en"])
	if onVal && strings.TrimSpace(roMsg) == "" {
		e["read_only.message"] = [2]string{"اكتب رسالة للموظفين", "Write a message for the staff"}
	}
	if support.RuneLen(roMsg) > 300 || support.RuneLen(roMsgEn) > 300 {
		e["read_only.message"] = [2]string{"حتى 300 حرف", "Up to 300 letters"}
	}

	checkRolesInto(r["roles"], e)
	return e
}

func checkRolesInto(rawRoles any, e map[string][2]string) {
	roles, ok := rawRoles.([]any)
	if !ok {
		e["roles"] = [2]string{"أدوار غير صحيحة", "Wrong roles"}
		return
	}
	if len(roles) > 20 {
		e["roles"] = [2]string{"حتى 20 دوراً", "Up to 20 roles"}
	}
	jobNames := map[string]bool{
		"المدير التنفيذي": true, "ceo": true, "مدير القسم": true, "project manager": true,
		"مسؤول المتابعة والتقييم": true, "monitoring & evaluation": true, "قائد الفريق": true, "team leader": true,
		"موظف": true, "employee": true, "مسؤول النظام": true, "system admin": true,
	}
	seenKeys := map[string]bool{}
	seenNames := map[string]string{}
	for _, r := range roles {
		rm := asMap(r)
		if rm == nil {
			e["roles"] = [2]string{"أدوار غير صحيحة", "Wrong roles"}
			continue
		}
		key := strVal(rm["key"])
		at := func(f string) string { return "roles." + key + "." + f }
		if !roleKeyRegex.MatchString(key) || seenKeys[key] {
			e[at("key")] = [2]string{"مفتاح غير صحيح", "Wrong key"}
		}
		seenKeys[key] = true

		for _, field := range []string{"name", "name_en"} {
			val := strings.TrimSpace(strVal(rm[field]))
			folded := strings.ToLower(val)
			if val == "" || support.RuneLen(val) > 40 {
				e[at(field)] = [2]string{"اكتب الاسم (حتى 40 حرفاً)", "Write the name (up to 40 letters)"}
			} else if jobNames[folded] {
				e[at(field)] = [2]string{"اسم وظيفة موجودة", "That's the name of an existing job"}
			} else if prevKey, exists := seenNames[folded]; exists && prevKey != key {
				e[at(field)] = [2]string{"مستعمل لدور آخر", "Used by another role"}
			} else {
				seenNames[folded] = key
			}
		}
		if support.RuneLen(strVal(rm["description"])) > 200 {
			e[at("description")] = [2]string{"حتى 200 حرف", "Up to 200 letters"}
		}
		if !store.UserHasRole(strVal(rm["color"]), RoleColors) {
			e[at("color")] = [2]string{"اختر لوناً", "Choose a colour"}
		}
		base := strVal(rm["base"])
		if !store.UserHasRole(base, BaseJobs) {
			e[at("base")] = [2]string{"يبدأ مثل: مدير قسم أو متابعة أو قائد فريق أو موظف", "Starts like: a PM, monitoring, a team leader or an employee"}
			continue
		}
		actions := toStringSlice(rm["actions"])
		if rm["actions"] == nil {
			e[at("actions")] = [2]string{"لا يأخذ أكثر مما لوظيفته الأساسية", "Never more than its base job"}
		} else {
			for _, a := range actions {
				if !store.UserHasRole(base, ActionDefs[a]) {
					e[at("actions")] = [2]string{"لا يأخذ أكثر مما لوظيفته الأساسية", "Never more than its base job"}
					break
				}
			}
		}
		pages := toStringSlice(rm["pages"])
		if rm["pages"] == nil {
			e[at("pages")] = [2]string{"صفحات لا تراها وظيفته الأساسية", "Pages its base job doesn't see"}
		} else {
			for _, pg := range pages {
				if !store.UserHasRole(base, NavRoles[pg]) {
					e[at("pages")] = [2]string{"صفحات لا تراها وظيفته الأساسية", "Pages its base job doesn't see"}
					break
				}
			}
		}
	}
}

func hhmmMin(t string) int {
	h, _ := strconv.Atoi(t[:2])
	m, _ := strconv.Atoi(t[3:5])
	return h*60 + m
}

func subsetOf(sub, sup []string) bool {
	set := make(map[string]bool, len(sup))
	for _, s := range sup {
		set[s] = true
	}
	for _, x := range sub {
		if !set[x] {
			return false
		}
	}
	return true
}

func changedRules(rules map[string]any) map[string]any {
	def := DefaultRules()
	out := map[string]any{}
	for k, dv := range def {
		b1, _ := json.Marshal(rules[k])
		b2, _ := json.Marshal(dv)
		if string(b1) != string(b2) {
			out[k] = rules[k]
		}
	}
	return out
}

func diffRules(before, after map[string]any) []map[string]any {
	def := DefaultRules()
	var out []map[string]any
	keys := make([]string, 0, len(def))
	for k := range def {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := before[k]
		b := after[k]
		if k == "roles" {
			was := map[string]map[string]any{}
			now := map[string]map[string]any{}
			if ar, ok := a.([]any); ok {
				for _, item := range ar {
					rm := asMap(item)
					was[strVal(rm["key"])] = rm
				}
			}
			if br, ok := b.([]any); ok {
				for _, item := range br {
					rm := asMap(item)
					now[strVal(rm["key"])] = rm
				}
			}
			allKeys := map[string]bool{}
			for rk := range was {
				allKeys[rk] = true
			}
			for rk := range now {
				allKeys[rk] = true
			}
			for rk := range allKeys {
				x := was[rk]
				y := now[rk]
				if jsonStr(normRole(x)) != jsonStr(normRole(y)) {
					out = append(out, map[string]any{"key": "roles." + rk, "from": x, "to": y})
				}
			}
		} else if am, okA := a.(map[string]any); okA && am != nil {
			bm := asMap(b)
			if bm == nil {
				out = append(out, map[string]any{"key": k, "from": am, "to": nil})
			} else {
				subs := map[string]bool{}
				for sk := range am {
					subs[sk] = true
				}
				for sk := range bm {
					subs[sk] = true
				}
				for sk := range subs {
					if jsonStr(normSet(am[sk])) != jsonStr(normSet(bm[sk])) {
						out = append(out, map[string]any{"key": k + "." + sk, "from": am[sk], "to": bm[sk]})
					}
				}
			}
		} else if jsonStr(a) != jsonStr(b) {
			out = append(out, map[string]any{"key": k, "from": a, "to": b})
		}
	}
	return out
}

func normSet(v any) any {
	switch arr := v.(type) {
	case []any:
		strs := make([]string, len(arr))
		for i, item := range arr {
			strs[i] = fmt.Sprint(item)
		}
		sort.Strings(strs)
		return strs
	case []string:
		strs := make([]string, len(arr))
		copy(strs, arr)
		sort.Strings(strs)
		return strs
	case []int:
		strs := make([]string, len(arr))
		for i, item := range arr {
			strs[i] = fmt.Sprint(item)
		}
		sort.Strings(strs)
		return strs
	}
	return v
}

func normRole(rm map[string]any) map[string]any {
	if rm == nil {
		return nil
	}
	cp := mergeStringMap(rm, nil)
	a := toStringSlice(cp["actions"])
	p := toStringSlice(cp["pages"])
	sort.Strings(a)
	sort.Strings(p)
	cp["actions"] = a
	cp["pages"] = p
	return cp
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func shortVal(v any) string {
	if m := asMap(v); m != nil && m["name"] != nil && m["base"] != nil {
		return support.TruncateRunes(strVal(m["name"]), 80)
	}
	if v == nil {
		return "-"
	}
	if s, ok := v.(string); ok {
		if support.RuneLen(s) > 80 {
			return support.TruncateRunes(s, 80) + "…"
		}
		return s
	}
	s := jsonStr(v)
	if support.RuneLen(s) > 80 {
		return support.TruncateRunes(s, 80) + "…"
	}
	return s
}
