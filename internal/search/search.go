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
	projResults := []map[string]any{}
	for _, p := range projects {
		if !access.CanSeeProject(u, p) {
			continue
		}
		visibleProjs[p.ID] = p
		if len(projResults) < eachLimit && support.SearchMatches(q, p.Name, derefStr(p.NameEn), p.Code) {
			item := map[string]any{
				"id":    p.ID,
				"code":  p.Code,
				"name":  p.Name,
				"state": p.State,
			}
			if p.NameEn != nil && *p.NameEn != "" {
				item["name_en"] = *p.NameEn
			}
			projResults = append(projResults, item)
		}
	}

	// Search tasks across visible projects
	taskResults := []map[string]any{}
	if len(visibleProjs) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, code, title, status FROM tasks ORDER BY id DESC`)
		if err == nil {
			for rows.Next() {
				if len(taskResults) >= eachLimit {
					break
				}
				var id, pid int64
				var code, title, status string
				if rows.Scan(&id, &pid, &code, &title, &status) != nil {
					continue
				}
				p, ok := visibleProjs[pid]
				if !ok {
					continue
				}
				if support.SearchMatches(q, title, code) {
					taskResults = append(taskResults, map[string]any{
						"id":         id,
						"project_id": pid,
						"code":       code,
						"title":      title,
						"status":     status,
						"context":    p.Name,
					})
				}
			}
			_ = rows.Close()
		}
	}

	// Search bugs across visible projects
	bugResults := []map[string]any{}
	if len(visibleProjs) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, code, title, status FROM bugs ORDER BY id DESC`)
		if err == nil {
			for rows.Next() {
				if len(bugResults) >= eachLimit {
					break
				}
				var id, pid int64
				var code, title, status string
				if rows.Scan(&id, &pid, &code, &title, &status) != nil {
					continue
				}
				p, ok := visibleProjs[pid]
				if !ok {
					continue
				}
				if support.SearchMatches(q, title, code) {
					bugResults = append(bugResults, map[string]any{
						"id":         id,
						"project_id": pid,
						"code":       code,
						"title":      title,
						"status":     status,
						"context":    p.Name,
					})
				}
			}
			_ = rows.Close()
		}
	}

	// Search announcements
	annResults := []map[string]any{}
	arows, err := s.db.QueryContext(ctx, `SELECT id, title, body, important, created_at FROM announcements ORDER BY important DESC, id DESC`)
	if err == nil {
		for arows.Next() {
			if len(annResults) >= eachLimit {
				break
			}
			var id int64
			var title, body string
			var important int
			var createdAtRaw any
			if arows.Scan(&id, &title, &body, &important, &createdAtRaw) != nil {
				continue
			}
			if support.SearchMatches(q, title, body) {
				annResults = append(annResults, map[string]any{
					"id":         id,
					"title":      title,
					"important":  important != 0,
					"created_at": support.FormatISO(dbx.ParseTime(createdAtRaw)),
				})
			}
		}
		_ = arows.Close()
	}

	// Search active users
	peopleResults := []map[string]any{}
	users, err := store.LoadAllUsers(ctx, s.db, "")
	if err == nil {
		for _, cand := range users {
			if !cand.Active {
				continue
			}
			if len(peopleResults) >= eachLimit {
				break
			}
			if support.SearchMatches(q, cand.Name, derefStr(cand.NameEn), cand.Email, cand.Title) {
				item := map[string]any{
					"id":      cand.ID,
					"name":    cand.Name,
					"name_en": cand.NameEn,
					"role":    cand.Role,
					"team_id": cand.TeamID,
				}
				if cand.Title != "" {
					item["context"] = cand.Title
				}
				peopleResults = append(peopleResults, item)
			}
		}
	}

	httpx.Success(w, map[string]any{
		"q":             q,
		"projects":      projResults,
		"tasks":         taskResults,
		"bugs":          bugResults,
		"announcements": annResults,
		"people":        peopleResults,
	})
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
