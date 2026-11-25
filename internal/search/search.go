package search

import (
	"net/http"
	"strings"

	"followapi/internal/access"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
)

const (
	eachLimit = 8
	minLen    = 2
	maxLen    = 80
)

type Service struct {
	db *dbx.DB
}

func New(db *dbx.DB) *Service {
	return &Service{db: db}
}

func (s *Service) HandleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	qLen := support.RuneLen(q)
	if qLen < minLen {
		httpx.HandleErr(w, r, httpx.ErrValidation("q", "api.search.q_min"))
		return
	}
	if qLen > maxLen {
		httpx.HandleErr(w, r, httpx.ErrValidation("q", "api.search.q_max"))
		return
	}

	projects, err := access.LoadAllProjects(ctx, s.db)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	visibleProjs := map[int64]*access.Project{}
	hits := []map[string]any{}
	projCount := 0
	for _, p := range projects {
		if !access.CanSeeProject(u, p) {
			continue
		}
		visibleProjs[p.ID] = p
		if projCount < eachLimit && support.SearchMatches(q, p.Name, derefStr(p.NameEn), p.Code) {
			item := map[string]any{
				"kind":  "project",
				"id":    p.ID,
				"title": p.Name,
				"code":  p.Code,
			}
			if p.NameEn != nil && *p.NameEn != "" {
				item["title_en"] = *p.NameEn
			}
			hits = append(hits, item)
			projCount++
		}
	}

	// Search tasks across visible projects
	taskCount := 0
	if len(visibleProjs) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, code, title FROM tasks ORDER BY id DESC`)
		if err == nil {
			for rows.Next() {
				if taskCount >= eachLimit {
					break
				}
				var id, pid int64
				var code, title string
				if rows.Scan(&id, &pid, &code, &title) != nil {
					continue
				}
				p, ok := visibleProjs[pid]
				if !ok {
					continue
				}
				if support.SearchMatches(q, title, code) {
					item := map[string]any{
						"kind":    "task",
						"id":      id,
						"title":   title,
						"code":    code,
						"context": p.Name,
					}
					if p.NameEn != nil && *p.NameEn != "" {
						item["context_en"] = *p.NameEn
					}
					hits = append(hits, item)
					taskCount++
				}
			}
			_ = rows.Close()
		}
	}

	// Search bugs across visible projects
	bugCount := 0
	if len(visibleProjs) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, code, title FROM bugs ORDER BY id DESC`)
		if err == nil {
			for rows.Next() {
				if bugCount >= eachLimit {
					break
				}
				var id, pid int64
				var code, title string
				if rows.Scan(&id, &pid, &code, &title) != nil {
					continue
				}
				p, ok := visibleProjs[pid]
				if !ok {
					continue
				}
				if support.SearchMatches(q, title, code) {
					item := map[string]any{
						"kind":    "bug",
						"id":      id,
						"title":   title,
						"code":    code,
						"context": p.Name,
					}
					if p.NameEn != nil && *p.NameEn != "" {
						item["context_en"] = *p.NameEn
					}
					hits = append(hits, item)
					bugCount++
				}
			}
			_ = rows.Close()
		}
	}

	// Search announcements
	annCount := 0
	arows, err := s.db.QueryContext(ctx, `SELECT id, title, body FROM announcements ORDER BY important DESC, id DESC`)
	if err == nil {
		for arows.Next() {
			if annCount >= eachLimit {
				break
			}
			var id int64
			var title, body string
			if arows.Scan(&id, &title, &body) != nil {
				continue
			}
			if support.SearchMatches(q, title, body) {
				hits = append(hits, map[string]any{
					"kind":  "announcement",
					"id":    id,
					"title": title,
				})
				annCount++
			}
		}
		_ = arows.Close()
	}

	// Search active users
	peopleCount := 0
	users, err := store.LoadAllUsers(ctx, s.db, "")
	if err == nil {
		for _, cand := range users {
			if !cand.Active {
				continue
			}
			if peopleCount >= eachLimit {
				break
			}
			if support.SearchMatches(q, cand.Name, derefStr(cand.NameEn), cand.Email, cand.Title) {
				item := map[string]any{
					"kind":  "person",
					"id":    cand.ID,
					"title": cand.Name,
					"role":  cand.Role,
				}
				if cand.NameEn != nil && *cand.NameEn != "" {
					item["title_en"] = *cand.NameEn
				}
				if cand.Title != "" {
					item["context"] = cand.Title
				}
				if cand.TeamID != nil {
					item["team_id"] = *cand.TeamID
				}
				hits = append(hits, item)
				peopleCount++
			}
		}
	}

	httpx.Success(w, map[string]any{
		"q":    q,
		"hits": hits,
	})
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
