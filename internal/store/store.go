package store

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"followapi/internal/dbx"
	"followapi/internal/support"
)

var SystemRoles = []string{"ceo", "pm", "monitor", "lead", "employee", "admin"}

type User struct {
	ID              int64
	Name            string
	NameEn          *string
	Email           string
	Phone           *string
	Role            string
	CustomRole      *string
	BirthDate       *string // YYYY-MM-DD
	PreferredLocale string
	Title           string
	TeamID          *int64
	Active          bool
	Password        string
	CreatedAt       *time.Time
	UpdatedAt       *time.Time
	AvatarURL       *string
}

func (u *User) IsCEO() bool {
	return u.Role == "ceo"
}

func (u *User) EffectiveName(locale string) string {
	if locale == "en" && u.NameEn != nil && strings.TrimSpace(*u.NameEn) != "" {
		return *u.NameEn
	}
	return u.Name
}

type Team struct {
	ID        int64
	Name      string
	NameEn    *string
	LeadID    *int64
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

type AppSetting struct {
	ID          int
	DueSoonDays int
	LockAfter   int
	LockMinutes int
}

// UserJSON produces the exact JSON map shape of Laravel's UserResource::baseFields
// (preserving explicit nulls for name_en, phone, team_id and omitting other null keys).
func UserJSON(u *User, viewer *User) map[string]any {
	var birthDate *string
	var birthday *string
	if u.BirthDate != nil && *u.BirthDate != "" {
		bd := *u.BirthDate
		if len(bd) >= 10 {
			bd = bd[:10]
		}
		if len(bd) >= 10 {
			sub := bd[5:10]
			birthday = &sub
		}
		canSee := viewer == nil || viewer.IsCEO() || viewer.Role == "admin" || viewer.ID == u.ID
		if canSee {
			birthDate = &bd
		}
	}

	out := map[string]any{
		"id":      u.ID,
		"name":    u.Name,
		"name_en": u.NameEn,
		"email":   u.Email,
		"phone":   u.Phone,
		"role":    u.Role,
		"team_id": u.TeamID,
		"active":  u.Active,
	}
	if u.CustomRole != nil && *u.CustomRole != "" {
		out["custom_role"] = *u.CustomRole
	}
	if u.Title != "" {
		out["title"] = u.Title
	}
	if u.AvatarURL != nil && *u.AvatarURL != "" {
		out["avatar_url"] = *u.AvatarURL
	}
	if birthDate != nil {
		out["birth_date"] = *birthDate
	}
	if birthday != nil {
		out["birthday"] = *birthday
	}
	return out
}

func BuildAvatarURL(appURL string, userID int64, cacheKey, mime string) string {
	ext := "jpg"
	if mime == "image/webp" {
		ext = "webp"
	}
	return fmt.Sprintf("%s/api/v1/avatars/%d/%s.%s", strings.TrimRight(appURL, "/"), userID, cacheKey, ext)
}

func FindUserByID(ctx context.Context, db *dbx.DB, appURL string, id int64) (*User, error) {
	users, err := LoadUsersByIDs(ctx, db, appURL, []int64{id})
	if err != nil {
		return nil, err
	}
	u, ok := users[id]
	if !ok {
		return nil, nil
	}
	return u, nil
}

func FindUserByIdentifier(ctx context.Context, db *dbx.DB, appURL string, identifier string) (*User, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = ? OR (phone IS NOT NULL AND phone = ?) LIMIT 1`,
		identifier, identifier,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return FindUserByID(ctx, db, appURL, id)
}

type cachedUserRec struct {
	user     User
	cacheKey string
	mime     string
}

var (
	usersCacheMu      sync.RWMutex
	usersCacheList    []cachedUserRec
	usersCacheByID    map[int64]cachedUserRec
	usersCacheExpires time.Time
)

func InvalidateUsersCache() {
	usersCacheMu.Lock()
	usersCacheList = nil
	usersCacheByID = nil
	usersCacheExpires = time.Time{}
	usersCacheMu.Unlock()
}

func materializeUser(rec cachedUserRec, appURL string) *User {
	u := rec.user
	if appURL != "" && rec.cacheKey != "" {
		url := BuildAvatarURL(appURL, u.ID, rec.cacheKey, rec.mime)
		u.AvatarURL = &url
	} else {
		u.AvatarURL = nil
	}
	return &u
}

func loadUsersCache(ctx context.Context, db *dbx.DB) ([]cachedUserRec, map[int64]cachedUserRec, error) {
	usersCacheMu.RLock()
	if usersCacheList != nil && time.Now().Before(usersCacheExpires) {
		list := usersCacheList
		byID := usersCacheByID
		usersCacheMu.RUnlock()
		return list, byID, nil
	}
	usersCacheMu.RUnlock()

	rows, err := db.QueryContext(ctx, `
		SELECT u.id, u.name, u.name_en, u.email, u.phone, u.role, u.custom_role, u.birth_date,
		       u.preferred_locale, u.title, u.team_id, u.active, u.password, u.created_at, u.updated_at,
		       a.cache_key, a.mime
		FROM users u
		LEFT JOIN avatars a ON a.id = (
			SELECT MAX(a2.id) FROM avatars a2 WHERE a2.user_id = u.id
		)
		ORDER BY u.id ASC
	`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	recs, err := scanUserRecs(rows)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]cachedUserRec, len(recs))
	for _, r := range recs {
		byID[r.user.ID] = r
	}
	usersCacheMu.Lock()
	usersCacheList = recs
	usersCacheByID = byID
	usersCacheExpires = time.Now().Add(30 * time.Second)
	usersCacheMu.Unlock()
	return recs, byID, nil
}

func LoadAllUsers(ctx context.Context, db *dbx.DB, appURL string) ([]*User, error) {
	if !db.IsSQLite() {
		recs, _, err := loadUsersCache(ctx, db)
		if err != nil {
			return nil, err
		}
		out := make([]*User, 0, len(recs))
		for _, rec := range recs {
			out = append(out, materializeUser(rec, appURL))
		}
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT u.id, u.name, u.name_en, u.email, u.phone, u.role, u.custom_role, u.birth_date,
		       u.preferred_locale, u.title, u.team_id, u.active, u.password, u.created_at, u.updated_at,
		       a.cache_key, a.mime
		FROM users u
		LEFT JOIN avatars a ON a.id = (
			SELECT MAX(a2.id) FROM avatars a2 WHERE a2.user_id = u.id
		)
		ORDER BY u.id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsers(rows, appURL)
}

func LoadUsersByIDs(ctx context.Context, db *dbx.DB, appURL string, ids []int64) (map[int64]*User, error) {
	out := make(map[int64]*User)
	if len(ids) == 0 {
		return out, nil
	}
	if !db.IsSQLite() {
		_, byID, err := loadUsersCache(ctx, db)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			if rec, ok := byID[id]; ok {
				out[id] = materializeUser(rec, appURL)
			}
		}
		return out, nil
	}
	ph, args := dbx.InInt64(ids)
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT u.id, u.name, u.name_en, u.email, u.phone, u.role, u.custom_role, u.birth_date,
		       u.preferred_locale, u.title, u.team_id, u.active, u.password, u.created_at, u.updated_at,
		       a.cache_key, a.mime
		FROM users u
		LEFT JOIN avatars a ON a.id = (
			SELECT MAX(a2.id) FROM avatars a2 WHERE a2.user_id = u.id
		)
		WHERE u.id IN (%s)
	`, ph), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanUsers(rows, appURL)
	if err != nil {
		return nil, err
	}
	for _, u := range list {
		out[u.ID] = u
	}
	return out, nil
}

func scanUserRecs(rows *sql.Rows) ([]cachedUserRec, error) {
	var out []cachedUserRec
	for rows.Next() {
		var u User
		var nameEn, phone, customRole, cacheKey, mime sql.NullString
		var birthRaw, createdRaw, updatedRaw any
		var teamID sql.NullInt64
		var activeInt int
		if err := rows.Scan(
			&u.ID, &u.Name, &nameEn, &u.Email, &phone, &u.Role, &customRole, &birthRaw,
			&u.PreferredLocale, &u.Title, &teamID, &activeInt, &u.Password, &createdRaw, &updatedRaw,
			&cacheKey, &mime,
		); err != nil {
			return nil, err
		}
		if nameEn.Valid {
			v := nameEn.String
			u.NameEn = &v
		}
		if phone.Valid {
			v := phone.String
			u.Phone = &v
		}
		if customRole.Valid && customRole.String != "" {
			v := customRole.String
			u.CustomRole = &v
		}
		u.BirthDate = dbx.ParseDateOnly(birthRaw)
		if teamID.Valid {
			v := teamID.Int64
			u.TeamID = &v
		}
		u.Active = activeInt != 0
		u.CreatedAt = dbx.ParseTime(createdRaw)
		u.UpdatedAt = dbx.ParseTime(updatedRaw)
		out = append(out, cachedUserRec{
			user:     u,
			cacheKey: cacheKey.String,
			mime:     mime.String,
		})
	}
	return out, rows.Err()
}

func scanUsers(rows *sql.Rows, appURL string) ([]*User, error) {
	var out []*User
	for rows.Next() {
		var u User
		var nameEn, phone, customRole, cacheKey, mime sql.NullString
		var birthRaw, createdRaw, updatedRaw any
		var teamID sql.NullInt64
		var activeInt int
		if err := rows.Scan(
			&u.ID, &u.Name, &nameEn, &u.Email, &phone, &u.Role, &customRole, &birthRaw,
			&u.PreferredLocale, &u.Title, &teamID, &activeInt, &u.Password, &createdRaw, &updatedRaw,
			&cacheKey, &mime,
		); err != nil {
			return nil, err
		}
		if nameEn.Valid {
			v := nameEn.String
			u.NameEn = &v
		}
		if phone.Valid {
			v := phone.String
			u.Phone = &v
		}
		if customRole.Valid && customRole.String != "" {
			v := customRole.String
			u.CustomRole = &v
		}
		u.BirthDate = dbx.ParseDateOnly(birthRaw)
		if teamID.Valid {
			v := teamID.Int64
			u.TeamID = &v
		}
		u.Active = activeInt != 0
		u.CreatedAt = dbx.ParseTime(createdRaw)
		u.UpdatedAt = dbx.ParseTime(updatedRaw)
		if cacheKey.Valid && cacheKey.String != "" {
			url := BuildAvatarURL(appURL, u.ID, cacheKey.String, mime.String)
			u.AvatarURL = &url
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func LoadAppSetting(ctx context.Context, db *dbx.DB) (AppSetting, error) {
	s := AppSetting{ID: 1, DueSoonDays: 1, LockAfter: 5, LockMinutes: 15}
	err := db.QueryRowContext(ctx, `SELECT due_soon_days, lock_after, lock_minutes FROM app_settings WHERE id = 1`).
		Scan(&s.DueSoonDays, &s.LockAfter, &s.LockMinutes)
	if err == sql.ErrNoRows {
		nowStr := dbx.FormatDBTime(time.Now().UTC())
		_, _ = db.ExecContext(ctx, db.InsertIgnore()+` INTO app_settings (id, due_soon_days, lock_after, lock_minutes, created_at, updated_at) VALUES (1, 1, 5, 15, ?, ?)`, nowStr, nowStr)
		return s, nil
	}
	return s, err
}

func ActiveAdminIDs(ctx context.Context, db *dbx.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM users WHERE role = 'admin' AND active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func UserHasRole(role string, roles []string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func CleanMap(m map[string]any, keepNullKeys ...string) map[string]any {
	keep := make(map[string]bool, len(keepNullKeys))
	for _, k := range keepNullKeys {
		keep[k] = true
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if isNilOrTypedNilPtr(v) && !keep[k] {
			continue
		}
		out[k] = v
	}
	return out
}

func isNilOrTypedNilPtr(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Ptr && rv.IsNil()
}

func IsoTime(t *time.Time) any {
	p := support.FormatISO(t)
	if p == nil {
		return nil
	}
	return *p
}
