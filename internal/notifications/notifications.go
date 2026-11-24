package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"followapi/internal/advanced"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/push"
	"followapi/internal/store"
	"followapi/internal/ws"
)

type About struct {
	ActorID        *int64
	ProjectID      *int64
	TaskID         *int64
	AnnouncementID *int64
	BugID          *int64
	ChatKey        *string
	TeamID         *int64
	Data           map[string]any
	DedupKey       *string
}

type Service struct {
	db       *dbx.DB
	advanced *advanced.Service
	push     *push.Service
	ws       *ws.Hub
}

func New(db *dbx.DB, adv *advanced.Service, pushSvc *push.Service) *Service {
	return &Service{db: db, advanced: adv, push: pushSvc}
}

func (s *Service) SetHub(h *ws.Hub) {
	s.ws = h
}

func (s *Service) Notify(ctx context.Context, recipientIDs []int64, kind string, about About) int {
	seen := map[int64]bool{}
	var unique []int64
	for _, id := range recipientIDs {
		if id <= 0 || seen[id] {
			continue
		}
		if about.ActorID != nil && id == *about.ActorID {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return 0
	}

	disabled := map[int64]bool{}
	rolesByUser := map[int64]string{}
	if !advanced.RequiredNotificationKinds[kind] {
		ph, args := dbx.InInt64(unique)
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, role FROM users WHERE id IN (%s)`, ph), args...)
		if err == nil {
			for rows.Next() {
				var id int64
				var role string
				if rows.Scan(&id, &role) == nil {
					rolesByUser[id] = role
				}
			}
			_ = rows.Close()
		}

		pargs := append(args, kind)
		prows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT user_id FROM notification_preferences WHERE user_id IN (%s) AND kind = ? AND enabled = 0`, ph), pargs...)
		if err == nil {
			for prows.Next() {
				var uid int64
				if prows.Scan(&uid) == nil {
					disabled[uid] = true
				}
			}
			_ = prows.Close()
		}
	}

	dataMap := about.Data
	if dataMap == nil {
		dataMap = map[string]any{}
	}
	dataBytes, _ := json.Marshal(dataMap)
	dataStr := string(dataBytes)
	nowStr := dbx.FormatDBTime(time.Now().UTC())

	created := 0
	var pushRecips []push.NotificationRecipient
	for _, uid := range unique {
		if role, ok := rolesByUser[uid]; ok && role != "" {
			if !s.advanced.JobGets(ctx, role, kind) {
				continue
			}
		}
		if disabled[uid] {
			continue
		}
		var dk *string
		if about.DedupKey != nil && *about.DedupKey != "" {
			val := *about.DedupKey
			if len(unique) > 1 {
				val = fmt.Sprintf("%s:u%d", val, uid)
			}
			dk = &val
		}

		res, err := s.db.ExecContext(ctx, `
			INSERT INTO notifications (user_id, kind, actor_id, project_id, task_id, announcement_id, bug_id, chat_key, team_id, data, dedup_key, at, `+"`read`"+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)
		`, uid, kind, about.ActorID, about.ProjectID, about.TaskID, about.AnnouncementID, about.BugID, about.ChatKey, about.TeamID, dataStr, dk, nowStr)
		if err != nil {
			if dk != nil && dbx.IsUniqueViolation(err) {
				continue
			}
			continue
		}
		notifID, _ := res.LastInsertId()
		created++
		pushRecips = append(pushRecips, push.NotificationRecipient{NotificationID: notifID, UserID: uid})
		s.ws.NotifyNotification(uid, notifID)
	}
	if len(pushRecips) > 0 && s.push != nil {
		s.push.QueueForNotificationsBatch(ctx, pushRecips, kind, about.ActorID, about.ProjectID, about.TaskID, about.AnnouncementID, about.BugID, about.TeamID, about.ChatKey, dataMap)
	}
	return created
}

func AboutProject(name string, nameEn *string) map[string]any {
	d := map[string]any{"project_name": name}
	if nameEn != nil && *nameEn != "" {
		d["project_name_en"] = *nameEn
	}
	return d
}

func AboutTask(code, title, projName string, projNameEn *string) map[string]any {
	d := AboutProject(projName, projNameEn)
	d["task_code"] = code
	d["task_title"] = title
	return d
}

func (s *Service) HandleIndex(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, user_id, kind, actor_id, project_id, task_id, announcement_id, bug_id, chat_key, team_id, data, at, `+"`read`"+`
		FROM notifications
		WHERE user_id = ?
		ORDER BY at DESC, id DESC
		LIMIT 100
	`, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()

	list := []map[string]any{}
	for rows.Next() {
		var id, userID int64
		var kind, dataRaw string
		var actorID, projectID, taskID, annID, bugID, teamID sql.NullInt64
		var chatKey sql.NullString
		var atRaw any
		var readInt int
		if err := rows.Scan(&id, &userID, &kind, &actorID, &projectID, &taskID, &annID, &bugID, &chatKey, &teamID, &dataRaw, &atRaw, &readInt); err != nil {
			httpx.HandleErr(w, r, err)
			return
		}
		dataObj := map[string]any{}
		if strings.TrimSpace(dataRaw) != "" {
			_ = json.Unmarshal([]byte(dataRaw), &dataObj)
		}
		item := map[string]any{
			"id":      id,
			"user_id": userID,
			"kind":    kind,
			"data":    dataObj,
			"at":      store.IsoTime(dbx.ParseTime(atRaw)),
			"read":    readInt != 0,
		}
		if actorID.Valid {
			item["actor_id"] = actorID.Int64
		}
		if projectID.Valid {
			item["project_id"] = projectID.Int64
		}
		if taskID.Valid {
			item["task_id"] = taskID.Int64
		}
		if annID.Valid {
			item["announcement_id"] = annID.Int64
		}
		if bugID.Valid {
			item["bug_id"] = bugID.Int64
		}
		if chatKey.Valid && chatKey.String != "" {
			item["chat_key"] = chatKey.String
		}
		if teamID.Valid {
			item["team_id"] = teamID.Int64
		}
		list = append(list, item)
	}
	httpx.Success(w, list)
}

func (s *Service) HandleMarkRead(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var ownerID int64
	var readInt int
	err = s.db.QueryRowContext(r.Context(), `SELECT user_id, `+"`read`"+` FROM notifications WHERE id = ?`, id).Scan(&ownerID, &readInt)
	if err == sql.ErrNoRows || ownerID != u.ID {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.notifications.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if readInt == 0 {
		_, _ = s.db.ExecContext(r.Context(), `UPDATE notifications SET `+"`read`"+` = 1 WHERE id = ?`, id)
	}
	httpx.Success(w, nil)
}

func (s *Service) HandleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	_, err := s.db.ExecContext(r.Context(), `UPDATE notifications SET `+"`read`"+` = 1 WHERE user_id = ? AND `+"`read`"+` = 0`, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, nil)
}

func (s *Service) HandlePreferences(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	rows, err := s.db.QueryContext(r.Context(), `SELECT kind, enabled FROM notification_preferences WHERE user_id = ?`, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	defer rows.Close()
	prefs := map[string]bool{}
	for rows.Next() {
		var kind string
		var enabled int
		if rows.Scan(&kind, &enabled) == nil {
			prefs[kind] = enabled != 0
		}
	}
	httpx.Success(w, prefs)
}

func (s *Service) HandleSavePreferences(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	clean := map[string]bool{}
	for kind, rawVal := range input {
		if !store.UserHasRole(kind, advanced.NotificationKinds) {
			httpx.HandleErr(w, r, httpx.ErrValidation("prefs", "api.notifications.unknown_kind"))
			return
		}
		on, ok := rawVal.(bool)
		if !ok {
			httpx.HandleErr(w, r, httpx.ErrValidation("prefs", "api.notifications.invalid_value"))
			return
		}
		if !on && advanced.RequiredNotificationKinds[kind] {
			httpx.HandleErr(w, r, httpx.ErrValidation("prefs", "api.notifications.required_kind"))
			return
		}
		clean[kind] = on
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM notification_preferences WHERE user_id = ?`, u.ID); err != nil {
			return err
		}
		for kind, on := range clean {
			en := 0
			if on {
				en = 1
			}
			if _, err := tx.ExecContext(r.Context(), `
				INSERT INTO notification_preferences (user_id, kind, enabled, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?)
			`, u.ID, kind, en, nowStr, nowStr); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, clean)
}
