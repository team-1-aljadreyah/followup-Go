package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

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
)

type cachedAuth struct {
	user      store.User
	sid       string
	tokenID   int64
	expiresAt time.Time
}

type Service struct {
	db            *dbx.DB
	cfg           config.Config
	audit         *audit.Service
	notifications *notifications.Service
	push          *push.Service
	advanced      *advanced.Service
	ws            *ws.Hub

	cacheMu     sync.RWMutex
	tokenCache  map[string]cachedAuth
	lastTouched map[int64]time.Time
}

func New(db *dbx.DB, cfg config.Config, auditSvc *audit.Service, notifSvc *notifications.Service, pushSvc *push.Service, advSvc *advanced.Service) *Service {
	return &Service{
		db:            db,
		cfg:           cfg,
		audit:         auditSvc,
		notifications: notifSvc,
		push:          pushSvc,
		advanced:      advSvc,
		tokenCache:    make(map[string]cachedAuth),
		lastTouched:   make(map[int64]time.Time),
	}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), 10)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func CheckPassword(plain, hashed string) bool {
	if hashed == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plain)) == nil
}

func generatePlainToken() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 40)
	rnd := make([]byte, 40)
	_, _ = rand.Read(rnd)
	for i := range b {
		b[i] = chars[int(rnd[i])%len(chars)]
	}
	return string(b)
}

func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ValidateToken validates a raw Sanctum Bearer token (without "Bearer "), active user & active auth_session,
// throttles last_seen_at updates to 1m, and returns (user, sessionID, tokenID, nil).
func (s *Service) InvalidateUser(userID int64) {
	if userID <= 0 {
		return
	}
	s.cacheMu.Lock()
	for k, v := range s.tokenCache {
		if v.user.ID == userID {
			delete(s.tokenCache, k)
		}
	}
	s.cacheMu.Unlock()
}

func (s *Service) InvalidateToken(tokenID int64) {
	if tokenID <= 0 {
		return
	}
	s.cacheMu.Lock()
	for k, v := range s.tokenCache {
		if v.tokenID == tokenID {
			delete(s.tokenCache, k)
		}
	}
	s.cacheMu.Unlock()
}

func (s *Service) InvalidateSessions(sids []string) {
	if len(sids) == 0 {
		return
	}
	set := make(map[string]bool, len(sids))
	for _, sid := range sids {
		if sid != "" {
			set[sid] = true
		}
	}
	if len(set) == 0 {
		return
	}
	s.cacheMu.Lock()
	for k, v := range s.tokenCache {
		if set[v.sid] {
			delete(s.tokenCache, k)
		}
	}
	s.cacheMu.Unlock()
}

func (s *Service) shouldTouch(tokenID int64, now time.Time) bool {
	s.cacheMu.RLock()
	prev, ok := s.lastTouched[tokenID]
	s.cacheMu.RUnlock()
	if ok && now.Sub(prev) < time.Minute {
		return false
	}
	s.cacheMu.Lock()
	if len(s.lastTouched) > 4000 {
		s.lastTouched = make(map[int64]time.Time)
	}
	s.lastTouched[tokenID] = now
	s.cacheMu.Unlock()
	return true
}

func (s *Service) touchUsageAsync(tokenID int64, sid string, updateToken, updateSession bool, now time.Time) {
	if !updateToken && !updateSession {
		return
	}
	nowStr := dbx.FormatDBTime(now)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if updateSession && sid != "" {
			_, _ = s.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at = ? WHERE id = ?`, nowStr, sid)
		}
		if updateToken {
			_, _ = s.db.ExecContext(ctx, `UPDATE personal_access_tokens SET last_used_at = ? WHERE id = ?`, nowStr, tokenID)
		}
	}()
}

func (s *Service) ValidateToken(ctx context.Context, bearer string) (*store.User, string, int64, error) {
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return nil, "", 0, httpx.ErrUnauth()
	}

	var tokenID int64
	var tokenHash, plainSecret string
	if idx := strings.IndexByte(bearer, '|'); idx > 0 {
		parsedID, err := strconv.ParseInt(bearer[:idx], 10, 64)
		if err != nil || parsedID <= 0 {
			return nil, "", 0, httpx.ErrUnauth()
		}
		tokenID = parsedID
		plainSecret = bearer[idx+1:]
	} else {
		plainSecret = bearer
	}
	tokenHash = hashToken(plainSecret)

	useCache := !s.db.IsSQLite()
	cacheKey := strconv.FormatInt(tokenID, 10) + "|" + tokenHash
	now := time.Now().UTC()
	if useCache {
		s.cacheMu.RLock()
		entry, ok := s.tokenCache[cacheKey]
		s.cacheMu.RUnlock()
		if ok && time.Now().Before(entry.expiresAt) {
			if s.shouldTouch(entry.tokenID, now) {
				s.touchUsageAsync(entry.tokenID, entry.sid, true, entry.sid != "", now)
			}
			uCopy := entry.user
			return &uCopy, entry.sid, entry.tokenID, nil
		}
	}

	whereClause := "t.token = ?"
	var arg any = tokenHash
	if tokenID > 0 {
		whereClause = "t.id = ?"
		arg = tokenID
	}

	var (
		rowID, userID                             int64
		storedHash                                string
		sessionID                                 sql.NullString
		expiresAtRaw, lastUsedRaw                 any
		endedAtRaw, lastSeenRaw                   any
		u                                         store.User
		nameEn, phone, customRole, cacheKeyAv, mime sql.NullString
		birthRaw, createdRaw, updatedRaw          any
		teamID                                    sql.NullInt64
		activeInt                                 int
	)
	query := fmt.Sprintf(`
		SELECT t.id, t.tokenable_id, t.token, t.session_id, t.expires_at, t.last_used_at,
		       u.id, u.name, u.name_en, u.email, u.phone, u.role, u.custom_role, u.birth_date,
		       u.preferred_locale, u.title, u.team_id, u.active, u.password, u.created_at, u.updated_at,
		       a.cache_key, a.mime,
		       s.ended_at, s.last_seen_at
		FROM personal_access_tokens t
		JOIN users u ON u.id = t.tokenable_id
		LEFT JOIN auth_sessions s ON s.id = t.session_id
		LEFT JOIN avatars a ON a.id = (SELECT MAX(a2.id) FROM avatars a2 WHERE a2.user_id = u.id)
		WHERE %s AND t.tokenable_type = 'App\\Models\\User'
	`, whereClause)

	err := s.db.QueryRowContext(ctx, query, arg).Scan(
		&rowID, &userID, &storedHash, &sessionID, &expiresAtRaw, &lastUsedRaw,
		&u.ID, &u.Name, &nameEn, &u.Email, &phone, &u.Role, &customRole, &birthRaw,
		&u.PreferredLocale, &u.Title, &teamID, &activeInt, &u.Password, &createdRaw, &updatedRaw,
		&cacheKeyAv, &mime,
		&endedAtRaw, &lastSeenRaw,
	)
	if err != nil || subtle.ConstantTimeCompare([]byte(storedHash), []byte(tokenHash)) != 1 {
		return nil, "", 0, httpx.ErrUnauth()
	}
	if exp := dbx.ParseTime(expiresAtRaw); exp != nil && now.After(*exp) {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE id = ?`, rowID)
		return nil, "", 0, httpx.ErrUnauth()
	}
	if activeInt == 0 {
		return nil, "", 0, httpx.ErrUnauth()
	}
	sid := ""
	if sessionID.Valid && sessionID.String != "" {
		sid = sessionID.String
		if dbx.ParseTime(endedAtRaw) != nil {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE id = ?`, rowID)
			return nil, "", 0, httpx.ErrUnauth()
		}
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
	u.Active = true
	u.CreatedAt = dbx.ParseTime(createdRaw)
	u.UpdatedAt = dbx.ParseTime(updatedRaw)
	if cacheKeyAv.Valid && cacheKeyAv.String != "" {
		url := store.BuildAvatarURL(s.cfg.AppURL, u.ID, cacheKeyAv.String, mime.String)
		u.AvatarURL = &url
	}

	if useCache {
		s.cacheMu.Lock()
		if len(s.tokenCache) > 2000 {
			s.tokenCache = make(map[string]cachedAuth)
		}
		s.tokenCache[cacheKey] = cachedAuth{
			user:      u,
			sid:       sid,
			tokenID:   rowID,
			expiresAt: time.Now().Add(60 * time.Second),
		}
		s.cacheMu.Unlock()

		lastSeen := dbx.ParseTime(lastSeenRaw)
		lastUsed := dbx.ParseTime(lastUsedRaw)
		needSession := sid != "" && (lastSeen == nil || lastSeen.Before(now.Add(-time.Minute)))
		needToken := lastUsed == nil || lastUsed.Before(now.Add(-time.Minute))
		if (needSession || needToken) && s.shouldTouch(rowID, now) {
			s.touchUsageAsync(rowID, sid, needToken, needSession, now)
		}
		uCopy := u
		return &uCopy, sid, rowID, nil
	}

	if sid != "" {
		lastSeen := dbx.ParseTime(lastSeenRaw)
		if lastSeen == nil || lastSeen.Before(now.Add(-time.Minute)) {
			_, _ = s.db.ExecContext(ctx, `UPDATE auth_sessions SET last_seen_at = ? WHERE id = ?`, dbx.FormatDBTime(now), sid)
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE personal_access_tokens SET last_used_at = ? WHERE id = ?`, dbx.FormatDBTime(now), rowID)
	return &u, sid, rowID, nil
}

// Middleware authenticates Bearer Sanctum tokens, validates active user & active auth_session,
// throttles last_seen_at updates to 1m, and remembers user's preferred_locale.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawAuth := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(strings.ToLower(rawAuth), "bearer ") {
			httpx.HandleErr(w, r, httpx.ErrUnauth())
			return
		}
		user, sid, rowID, err := s.ValidateToken(r.Context(), rawAuth[7:])
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}

		locale := httpx.LocaleFrom(r.Context())
		if (locale == "ar" || locale == "en") && user.PreferredLocale != locale {
			user.PreferredLocale = locale
			s.InvalidateUser(user.ID)
			store.InvalidateUsersCache()
			if !s.db.IsSQLite() {
				uid := user.ID
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, _ = s.db.ExecContext(ctx, `UPDATE users SET preferred_locale = ? WHERE id = ?`, locale, uid)
				}()
			} else {
				_, _ = s.db.ExecContext(r.Context(), `UPDATE users SET preferred_locale = ? WHERE id = ?`, locale, user.ID)
			}
		}

		ctx := httpx.WithAuth(r.Context(), user, sid, rowID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Service) HandleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	id := strings.ToLower(strings.TrimSpace(strVal(body["identifier"])))
	pw := strVal(body["password"])
	if id == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("identifier", "api.auth.identifier_required"))
		return
	}
	if pw == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("password", "api.auth.password_required"))
		return
	}

	user, err := store.FindUserByIdentifier(r.Context(), s.db, s.cfg.AppURL, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	settings, err := store.LoadAppSetting(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowUtc := time.Now().UTC()

	if user != nil {
		var lockedRaw any
		err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
			var uid int64
			qerr := tx.QueryRowContext(r.Context(), `SELECT user_id, locked_until FROM login_security WHERE user_id = ?`+s.db.ForUpdate(), user.ID).
				Scan(&uid, &lockedRaw)
			if qerr == sql.ErrNoRows {
				nowStr := dbx.FormatDBTime(nowUtc)
				_, ierr := tx.ExecContext(r.Context(), `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, user.ID, nowStr, nowStr)
				return ierr
			}
			return qerr
		})
		if err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		if lu := dbx.ParseTime(lockedRaw); lu != nil && lu.After(nowUtc) {
			until := lu.In(support.WorkingLocation(s.cfg.WorkingTimezone)).Format("15:04")
			httpx.HandleErr(w, r, httpx.ErrRateLimited("api.auth.account_locked", map[string]any{"until": until}))
			return
		}
	}

	if user == nil || !CheckPassword(pw, user.Password) {
		var uidPtr *int64
		if user != nil {
			uidPtr = &user.ID
		}
		s.audit.Log(r.Context(), nil, "login_failed", "login", uidPtr, support.TruncateRunes(id, 120), nil)

		if user != nil {
			becameLocked := false
			_ = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
				var failed int
				var lockedRaw any
				if err := tx.QueryRowContext(r.Context(), `SELECT failed_logins, locked_until FROM login_security WHERE user_id = ?`+s.db.ForUpdate(), user.ID).
					Scan(&failed, &lockedRaw); err != nil {
					return err
				}
				failed++
				var lockedUntilStr *string
				if failed >= settings.LockAfter {
					sStr := dbx.FormatDBTime(nowUtc.Add(time.Duration(settings.LockMinutes) * time.Minute))
					lockedUntilStr = &sStr
					becameLocked = true
				}
				_, err := tx.ExecContext(r.Context(), `
					UPDATE login_security SET failed_logins = ?, locked_until = ?, updated_at = ? WHERE user_id = ?
				`, failed, lockedUntilStr, dbx.FormatDBTime(nowUtc), user.ID)
				return err
			})
			if becameLocked {
				mins := strconv.Itoa(settings.LockMinutes)
				s.audit.Log(r.Context(), nil, "login_locked", "user", &user.ID, user.Name, []audit.Change{{Field: "minutes", To: &mins}})
				admins, _ := store.ActiveAdminIDs(r.Context(), s.db)
				s.notifications.Notify(r.Context(), admins, "account_locked", notifications.About{
					Data: map[string]any{"note": user.Name},
				})
			}
		}

		httpx.HandleErr(w, r, &httpx.APIError{Status: http.StatusUnprocessableEntity, Code: "VALIDATION", Key: "api.auth.invalid_credentials"})
		return
	}

	if !user.Active {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.auth.account_stopped"))
		return
	}

	nowStr := dbx.FormatDBTime(nowUtc)
	if s.db.IsSQLite() {
		_, _ = s.db.ExecContext(r.Context(), `
			INSERT INTO login_security (user_id, failed_logins, locked_until, last_login_at, created_at, updated_at)
			VALUES (?, 0, NULL, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET failed_logins = 0, locked_until = NULL, last_login_at = excluded.last_login_at, updated_at = excluded.updated_at
		`, user.ID, nowStr, nowStr, nowStr)
	} else {
		_, _ = s.db.ExecContext(r.Context(), `
			INSERT INTO login_security (user_id, failed_logins, locked_until, last_login_at, created_at, updated_at)
			VALUES (?, 0, NULL, ?, ?, ?)
			ON DUPLICATE KEY UPDATE failed_logins = 0, locked_until = NULL, last_login_at = VALUES(last_login_at), updated_at = VALUES(updated_at)
		`, user.ID, nowStr, nowStr, nowStr)
	}

	plain := generatePlainToken()
	tokenHash := hashToken(plain)
	sessionID := "s_" + support.RandomHex(8)

	res, err := s.db.ExecContext(r.Context(), `
		INSERT INTO personal_access_tokens (tokenable_type, tokenable_id, name, token, abilities, session_id, created_at, updated_at)
		VALUES ('App\\Models\\User', ?, 'api', ?, '["*"]', ?, ?, ?)
	`, user.ID, tokenHash, sessionID, nowStr, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	tokenID, _ := res.LastInsertId()

	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO auth_sessions (id, user_id, token_id, ip, user_agent, started_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, sessionID, user.ID, tokenID, httpx.ClientIPFrom(r.Context()), httpx.UserAgentFrom(r.Context()), nowStr, nowStr)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	httpx.Success(w, map[string]any{
		"token": fmt.Sprintf("%d|%s", tokenID, plain),
		"user":  store.UserJSON(user, user),
	})
}

func (s *Service) HandleLogout(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	sid := httpx.SessionIDFrom(r.Context())
	tid := httpx.TokenIDFrom(r.Context())
	nowStr := dbx.FormatDBTime(time.Now().UTC())

	if sid != "" {
		_, _ = s.db.ExecContext(r.Context(), `
			UPDATE auth_sessions SET ended_at = ?, ended_by = ?, end_reason = 'signed_out'
			WHERE id = ? AND ended_at IS NULL
		`, nowStr, u.ID, sid)
		s.push.DeleteForSessions(r.Context(), []string{sid})
		s.ws.CloseUnauthorizedSessions([]string{sid})
	}
	if tid > 0 {
		_, _ = s.db.ExecContext(r.Context(), `DELETE FROM personal_access_tokens WHERE id = ?`, tid)
		s.InvalidateToken(tid)
		s.ws.CloseUnauthorizedToken(tid)
	}
	httpx.Success(w, nil)
}

func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	httpx.Success(w, store.UserJSON(u, u))
}

func (s *Service) HandleUsers(w http.ResponseWriter, r *http.Request) {
	viewer := httpx.UserFrom(r.Context())
	users, err := store.LoadAllUsers(r.Context(), s.db, s.cfg.AppURL)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		out = append(out, store.UserJSON(u, viewer))
	}
	httpx.Success(w, out)
}

func (s *Service) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	currentPw := strVal(body["current_password"])
	newPw := strVal(body["password"])

	settings, err := store.LoadAppSetting(r.Context(), s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	nowUtc := time.Now().UTC()

	var failed int
	var lockedRaw any
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		qerr := tx.QueryRowContext(r.Context(), `SELECT failed_logins, locked_until FROM login_security WHERE user_id = ?`+s.db.ForUpdate(), u.ID).
			Scan(&failed, &lockedRaw)
		if qerr == sql.ErrNoRows {
			nowStr := dbx.FormatDBTime(nowUtc)
			_, ierr := tx.ExecContext(r.Context(), `INSERT INTO login_security (user_id, failed_logins, created_at, updated_at) VALUES (?, 0, ?, ?)`, u.ID, nowStr, nowStr)
			return ierr
		}
		return qerr
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if lu := dbx.ParseTime(lockedRaw); lu != nil && lu.After(nowUtc) {
		until := lu.In(support.WorkingLocation(s.cfg.WorkingTimezone)).Format("15:04")
		httpx.HandleErr(w, r, httpx.ErrRateLimited("api.auth.account_locked", map[string]any{"until": until}))
		return
	}

	if currentPw == "" || !CheckPassword(currentPw, u.Password) {
		s.audit.Log(r.Context(), nil, "login_failed", "login", &u.ID, u.Email, nil)
		becameLocked := false
		_ = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
			var curFailed int
			_ = tx.QueryRowContext(r.Context(), `SELECT failed_logins FROM login_security WHERE user_id = ?`+s.db.ForUpdate(), u.ID).Scan(&curFailed)
			curFailed++
			var lockStr *string
			if curFailed >= settings.LockAfter {
				v := dbx.FormatDBTime(nowUtc.Add(time.Duration(settings.LockMinutes) * time.Minute))
				lockStr = &v
				becameLocked = true
			}
			_, err := tx.ExecContext(r.Context(), `UPDATE login_security SET failed_logins = ?, locked_until = ?, updated_at = ? WHERE user_id = ?`,
				curFailed, lockStr, dbx.FormatDBTime(nowUtc), u.ID)
			return err
		})
		if becameLocked {
			mins := strconv.Itoa(settings.LockMinutes)
			s.audit.Log(r.Context(), nil, "login_locked", "user", &u.ID, u.Name, []audit.Change{{Field: "minutes", To: &mins}})
			admins, _ := store.ActiveAdminIDs(r.Context(), s.db)
			s.notifications.Notify(r.Context(), admins, "account_locked", notifications.About{
				Data: map[string]any{"note": u.Name},
			})
		}
		httpx.HandleErr(w, r, httpx.ErrValidation("current_password", "api.password.wrong_current"))
		return
	}

	if failed > 0 {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE login_security SET failed_logins = 0, updated_at = ? WHERE user_id = ?`, dbx.FormatDBTime(nowUtc), u.ID)
	}

	if pKey, pParams := support.PasswordProblem(newPw, s.advanced.PasswordMin(r.Context())); pKey != "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("password", pKey, pParams))
		return
	}
	if newPw == currentPw {
		httpx.HandleErr(w, r, httpx.ErrValidation("password", "api.password.same_as_current"))
		return
	}

	hashed, err := HashPassword(newPw)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	curSid := httpx.SessionIDFrom(r.Context())
	nowStr := dbx.FormatDBTime(nowUtc)
	var endedSids []string

	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `UPDATE users SET password = ?, updated_at = ? WHERE id = ?`, hashed, nowStr, u.ID); err != nil {
			return err
		}
		query := `SELECT id, token_id FROM auth_sessions WHERE user_id = ? AND ended_at IS NULL`
		args := []any{u.ID}
		if curSid != "" {
			query += ` AND id != ?`
			args = append(args, curSid)
		}
		rows, err := tx.QueryContext(r.Context(), query, args...)
		if err != nil {
			return err
		}
		var tids []int64
		for rows.Next() {
			var sid string
			var tid sql.NullInt64
			if rows.Scan(&sid, &tid) == nil {
				endedSids = append(endedSids, sid)
				if tid.Valid {
					tids = append(tids, tid.Int64)
				}
			}
		}
		_ = rows.Close()

		if len(endedSids) > 0 {
			ph := dbx.Placeholders(len(endedSids))
			sArgs := []any{nowStr, u.ID}
			for _, sid := range endedSids {
				sArgs = append(sArgs, sid)
			}
			_, _ = tx.ExecContext(r.Context(), fmt.Sprintf(`
				UPDATE auth_sessions SET ended_at = ?, ended_by = ?, end_reason = 'everywhere' WHERE id IN (%s)
			`, ph), sArgs...)
		}
		if len(tids) > 0 {
			ph, tArgs := dbx.InInt64(tids)
			_, _ = tx.ExecContext(r.Context(), fmt.Sprintf(`DELETE FROM personal_access_tokens WHERE id IN (%s)`, ph), tArgs...)
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	s.push.DeleteForSessions(r.Context(), endedSids)
	s.InvalidateUser(u.ID)
	s.ws.CloseUnauthorizedSessions(endedSids)
	s.audit.Log(r.Context(), &u.ID, "user_changed", "user", &u.ID, u.Name, []audit.Change{{Field: "password"}})
	httpx.Success(w, nil)
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
