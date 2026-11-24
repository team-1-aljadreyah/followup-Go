package announcements

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/audit"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
	"followapi/internal/store"
	"followapi/internal/support"
)

type Service struct {
	db            *dbx.DB
	audit         *audit.Service
	notifications *notifications.Service
	advanced      *advanced.Service
}

func New(db *dbx.DB, auditSvc *audit.Service, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, audit: auditSvc, notifications: notifSvc, advanced: advSvc}
}

func (s *Service) HandleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)

	type annRow struct {
		id, authorID int64
		title, body  string
		important    bool
		createdAt    *time.Time
	}
	type readPair struct{ aid, uid int64 }

	var (
		allUsers  []*store.User
		list      []annRow
		annErr    error
		readPairs []readPair
	)

	loadAnns := func() {
		rows, err := s.db.QueryContext(ctx, `SELECT id, title, body, important, author_id, created_at FROM announcements ORDER BY important DESC, created_at DESC, id DESC`)
		if err != nil {
			annErr = err
			return
		}
		defer rows.Close()
		for rows.Next() {
			var a annRow
			var impInt int
			var crRaw any
			if rows.Scan(&a.id, &a.title, &a.body, &impInt, &a.authorID, &crRaw) == nil {
				a.important = impInt != 0
				a.createdAt = dbx.ParseTime(crRaw)
				list = append(list, a)
			}
		}
	}
	loadReads := func() {
		rRows, err := s.db.QueryContext(ctx, `SELECT announcement_id, user_id FROM announcement_reads`)
		if err != nil {
			return
		}
		defer rRows.Close()
		for rRows.Next() {
			var aid, uid int64
			if rRows.Scan(&aid, &uid) == nil {
				readPairs = append(readPairs, readPair{aid, uid})
			}
		}
	}

	if !s.db.IsSQLite() {
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); allUsers, _ = store.LoadAllUsers(ctx, s.db, "") }()
		go func() { defer wg.Done(); loadAnns() }()
		go func() { defer wg.Done(); loadReads() }()
		wg.Wait()
	} else {
		allUsers, _ = store.LoadAllUsers(ctx, s.db, "")
		loadAnns()
		loadReads()
	}
	if annErr != nil {
		httpx.HandleErr(w, r, annErr)
		return
	}

	activeSet := map[int64]bool{}
	for _, usr := range allUsers {
		if usr.Active {
			activeSet[usr.ID] = true
		}
	}
	audienceCount := len(activeSet)

	readsByAnn := map[int64]map[int64]bool{}
	for _, rp := range readPairs {
		if activeSet[rp.uid] {
			if readsByAnn[rp.aid] == nil {
				readsByAnn[rp.aid] = map[int64]bool{}
			}
			readsByAnn[rp.aid][rp.uid] = true
		}
	}

	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		rm := readsByAnn[a.id]
		out = append(out, map[string]any{
			"id":             a.id,
			"title":          a.title,
			"body":           a.body,
			"important":      a.important,
			"author_id":      a.authorID,
			"created_at":     store.IsoTime(a.createdAt),
			"seen":           rm != nil && rm[u.ID],
			"seen_count":     len(rm),
			"audience_count": audienceCount,
		})
	}
	httpx.Success(w, out)
}

func (s *Service) HandleShow(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	m, err := s.fetchHydrated(r.Context(), id, u.ID)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	httpx.Success(w, m)
}

func (s *Service) HandleCreate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	if err := s.advanced.RequireFeature(r.Context(), u, "announcements"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanPostAnnouncements(u) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.announcements.post_forbidden"))
		return
	}
	if err := s.advanced.RequireAction(r.Context(), u, "post_announcement"); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	body, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	title := strings.TrimSpace(strVal(body["title"]))
	text := strings.TrimSpace(strVal(body["body"]))
	if title == "" {
		httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.announcements.title_required"))
		return
	}
	if support.RuneLen(title) > 160 {
		httpx.HandleErr(w, r, httpx.ErrValidation("title", "api.announcements.title_max"))
		return
	}
	if support.RuneLen(text) > 5000 {
		httpx.HandleErr(w, r, httpx.ErrValidation("body", "api.announcements.body_max"))
		return
	}
	impInt := 0
	if b, _ := body["important"].(bool); b {
		impInt = 1
	}
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	var id int64
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO announcements (title, body, important, author_id, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, title, text, impInt, u.ID, nowStr)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		_, err = tx.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO announcement_reads (announcement_id, user_id, at) VALUES (?, ?, ?)`,
			id, u.ID, nowStr)
		return err
	})
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	activeIDs, _ := s.activeUserIDs(r.Context())
	s.notifications.Notify(r.Context(), activeIDs, "announcement", notifications.About{
		ActorID:        &u.ID,
		AnnouncementID: &id,
		Data:           map[string]any{"title": title, "important": impInt != 0},
	})

	m, _ := s.fetchHydrated(r.Context(), id, u.ID)
	httpx.Success(w, m, http.StatusCreated)
}

func (s *Service) HandleDelete(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var authorID int64
	var title string
	err = s.db.QueryRowContext(r.Context(), `SELECT author_id, title FROM announcements WHERE id = ?`, id).Scan(&authorID, &title)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.announcements.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanManageAnnouncement(u, authorID) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.announcements.delete_forbidden"))
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM announcements WHERE id = ?`, id); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	s.audit.Log(r.Context(), &u.ID, "announcement_deleted", "announcement", &id, title, nil)
	httpx.Success(w, nil)
}

func (s *Service) HandleMarkSeen(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var exists int
	if s.db.QueryRowContext(r.Context(), `SELECT 1 FROM announcements WHERE id = ?`, id).Scan(&exists) != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.announcements.not_found"))
		return
	}
	_, _ = s.db.ExecContext(r.Context(), s.db.InsertIgnore()+` INTO announcement_reads (announcement_id, user_id, at) VALUES (?, ?, ?)`,
		id, u.ID, dbx.FormatDBTime(time.Now().UTC()))
	httpx.Success(w, nil)
}

func (s *Service) HandleReads(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var authorID int64
	err = s.db.QueryRowContext(r.Context(), `SELECT author_id FROM announcements WHERE id = ?`, id).Scan(&authorID)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.announcements.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !access.CanManageAnnouncement(u, authorID) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.announcements.reads_forbidden"))
		return
	}

	activeIDs, _ := s.activeUserIDs(r.Context())
	seen := []map[string]any{}
	seenSet := map[int64]bool{}
	if len(activeIDs) > 0 {
		ph, args := dbx.InInt64(activeIDs)
		allArgs := append([]any{id}, args...)
		rows, err := s.db.QueryContext(r.Context(), fmt.Sprintf(`
			SELECT user_id, at FROM announcement_reads WHERE announcement_id = ? AND user_id IN (%s) ORDER BY at ASC, id ASC
		`, ph), allArgs...)
		if err == nil {
			for rows.Next() {
				var uid int64
				var atRaw any
				if rows.Scan(&uid, &atRaw) == nil {
					seenSet[uid] = true
					seen = append(seen, map[string]any{
						"user_id": uid,
						"at":      store.IsoTime(dbx.ParseTime(atRaw)),
					})
				}
			}
			_ = rows.Close()
		}
	}
	notSeen := []int64{}
	for _, aid := range activeIDs {
		if !seenSet[aid] {
			notSeen = append(notSeen, aid)
		}
	}
	httpx.Success(w, map[string]any{"seen": seen, "not_seen": notSeen})
}

func (s *Service) fetchHydrated(ctx context.Context, id, userID int64) (map[string]any, error) {
	var title, body string
	var impInt int
	var authorID int64
	var crRaw any
	err := s.db.QueryRowContext(ctx, `SELECT title, body, important, author_id, created_at FROM announcements WHERE id = ?`, id).
		Scan(&title, &body, &impInt, &authorID, &crRaw)
	if err == sql.ErrNoRows {
		return nil, httpx.ErrNotFound("api.announcements.not_found")
	}
	if err != nil {
		return nil, err
	}
	activeIDs, activeSet := s.activeUserIDs(ctx)
	seen := false
	seenCount := 0
	rows, _ := s.db.QueryContext(ctx, `SELECT user_id FROM announcement_reads WHERE announcement_id = ?`, id)
	if rows != nil {
		for rows.Next() {
			var uid int64
			if rows.Scan(&uid) == nil && activeSet[uid] {
				seenCount++
				if uid == userID {
					seen = true
				}
			}
		}
		_ = rows.Close()
	}
	return map[string]any{
		"id":             id,
		"title":          title,
		"body":           body,
		"important":      impInt != 0,
		"author_id":      authorID,
		"created_at":     store.IsoTime(dbx.ParseTime(crRaw)),
		"seen":           seen,
		"seen_count":     seenCount,
		"audience_count": len(activeIDs),
	}, nil
}

func (s *Service) activeUserIDs(ctx context.Context) ([]int64, map[int64]bool) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM users WHERE active = 1 ORDER BY id ASC`)
	if err != nil {
		return nil, map[int64]bool{}
	}
	defer rows.Close()
	var ids []int64
	set := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
			set[id] = true
		}
	}
	return ids, set
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
