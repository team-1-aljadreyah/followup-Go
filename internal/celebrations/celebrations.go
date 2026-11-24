package celebrations

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
	"followapi/internal/ws"
)

const RecentDays = 7

type Service struct {
	db *dbx.DB
	ws *ws.Hub
}

func New(db *dbx.DB) *Service {
	return &Service{db: db}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func (s *Service) HandleStore(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if !u.IsCEO() && u.Role != "monitor" {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.celebrations.forbidden"))
		return
	}
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	teamID := toInt64Val(input["team_id"])
	var teamName string
	var teamNameEn sql.NullString
	if teamID <= 0 || s.db.QueryRowContext(r.Context(), `SELECT name, name_en FROM teams WHERE id = ?`, teamID).Scan(&teamName, &teamNameEn) != nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("team_id", "api.celebrations.team_not_found"))
		return
	}
	kind := strVal(input["kind"])
	if kind != "good" && kind != "big" {
		httpx.HandleErr(w, r, httpx.ErrValidation("kind", "api.celebrations.kind_invalid"))
		return
	}
	var msgPtr *string
	if rawMsg, hasMsg := input["message"]; hasMsg && rawMsg != nil {
		trimmed := strings.TrimSpace(strVal(rawMsg))
		if support.RuneLen(trimmed) > 200 {
			httpx.HandleErr(w, r, httpx.ErrValidation("message", "api.celebrations.message_max", map[string]any{"max": 200}))
			return
		}
		if trimmed != "" {
			msgPtr = &trimmed
		}
	}

	rows, err := s.db.QueryContext(r.Context(), `SELECT id FROM users WHERE team_id = ? AND active = 1 AND id != ? ORDER BY id ASC`, teamID, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var recipIDs []int64
	for rows.Next() {
		var rid int64
		if rows.Scan(&rid) == nil {
			recipIDs = append(recipIDs, rid)
		}
	}
	_ = rows.Close()
	if len(recipIDs) == 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("team_id", "api.celebrations.team_empty"))
		return
	}

	now := time.Now().UTC()
	nowStr := dbx.FormatDBTime(now)
	var celID int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO celebrations (team_id, kind, message, created_by, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, teamID, kind, msgPtr, u.ID, nowStr)
		if err != nil {
			return err
		}
		celID, _ = res.LastInsertId()
		for _, rid := range recipIDs {
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO celebration_recipients (celebration_id, user_id, seen_at) VALUES (?, ?, NULL)`, celID, rid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	s.ws.NotifyCelebration(recipIDs, celID)

	var teamEnPtr *string
	if teamNameEn.Valid && teamNameEn.String != "" {
		teamEnPtr = &teamNameEn.String
	}
	out := formatCelebration(celID, teamID, teamName, teamEnPtr, kind, msgPtr, u.ID, u.Name, u.NameEn, u.Role, u.CustomRole, &now)
	out["sent_to"] = len(recipIDs)
	httpx.Success(w, out, http.StatusCreated)
}

func (s *Service) HandleMine(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	cutoff := dbx.FormatDBTime(time.Now().UTC().AddDate(0, 0, -RecentDays))

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT c.id, c.team_id, t.name, t.name_en, c.kind, c.message, c.created_by,
		       cr.name, cr.name_en, cr.role, cr.custom_role, c.created_at
		FROM celebrations c
		INNER JOIN celebration_recipients crp ON crp.celebration_id = c.id
		INNER JOIN teams t ON t.id = c.team_id
		INNER JOIN users cr ON cr.id = c.created_by
		WHERE crp.user_id = ? AND crp.seen_at IS NULL AND c.created_at >= ?
		ORDER BY c.created_at ASC, c.id ASC
	`, u.ID, cutoff)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var cid, tid, byID int64
		var tName, kind, byName, byRole string
		var tNameEn, msg, byNameEn, byCustom sql.NullString
		var crRaw any
		if rows.Scan(&cid, &tid, &tName, &tNameEn, &kind, &msg, &byID, &byName, &byNameEn, &byRole, &byCustom, &crRaw) == nil {
			out = append(out, formatCelebration(
				cid, tid, tName, nullStrPtr(tNameEn), kind, nullStrPtr(msg),
				byID, byName, nullStrPtr(byNameEn), byRole, nullStrPtr(byCustom), dbx.ParseTime(crRaw),
			))
		}
	}
	httpx.Success(w, out)
}

func (s *Service) HandleMarkSeen(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var rowID int64
	var seenRaw any
	err = s.db.QueryRowContext(r.Context(), `SELECT id, seen_at FROM celebration_recipients WHERE celebration_id = ? AND user_id = ? LIMIT 1`, id, u.ID).
		Scan(&rowID, &seenRaw)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.celebrations.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if dbx.ParseTime(seenRaw) == nil {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE celebration_recipients SET seen_at = ? WHERE id = ?`, dbx.FormatDBTime(time.Now().UTC()), rowID)
	}
	httpx.Success(w, nil)
}

func formatCelebration(id, teamID int64, teamName string, teamNameEn *string, kind string, msg *string, byID int64, byName string, byNameEn *string, byRole string, byCustom *string, createdAt *time.Time) map[string]any {
	return store.CleanMap(map[string]any{
		"id":             id,
		"team_id":        teamID,
		"team_name":      teamName,
		"team_name_en":   teamNameEn,
		"kind":           kind,
		"message":        msg,
		"by_id":          byID,
		"by_name":        byName,
		"by_name_en":     byNameEn,
		"by_role":        byRole,
		"by_custom_role": byCustom,
		"created_at":     store.IsoTime(createdAt),
	})
}

func nullStrPtr(s sql.NullString) *string {
	if s.Valid && s.String != "" {
		v := s.String
		return &v
	}
	return nil
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toInt64Val(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	}
	return 0
}
