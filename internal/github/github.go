package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"followapi/internal/access"
	"followapi/internal/advanced"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/notifications"
)

const maxRepos = 5

var repoFormatRegex = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)

type Service struct {
	db       *dbx.DB
	cfg      config.Config
	notif    *notifications.Service
	advanced *advanced.Service
}

func New(db *dbx.DB, cfg config.Config, notifSvc *notifications.Service, advSvc *advanced.Service) *Service {
	return &Service{db: db, cfg: cfg, notif: notifSvc, advanced: advSvc}
}

func (s *Service) HandleProjectSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	pid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(ctx, s.db, u, pid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	enabled, repos := s.loadProjectSettings(ctx, pid)
	httpx.Success(w, map[string]any{
		"project_id": pid,
		"enabled":    enabled,
		"repos":      repos,
	})
}

func (s *Service) HandleSaveProjectSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	pid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	p, err := access.ProjectForOrFail(ctx, s.db, u, pid)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if !u.IsCEO() && p.PMID != u.ID {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.github.manage_forbidden"))
		return
	}

	input, err := httpx.DecodeMap(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	enabled := false
	if v, ok := input["enabled"].(bool); ok {
		enabled = v
	}

	var repos []string
	seen := map[string]bool{}
	if rawList, ok := input["repos"].([]any); ok {
		for _, item := range rawList {
			rs := strings.TrimSpace(fmt.Sprintf("%v", item))
			if rs == "" || item == nil {
				continue
			}
			low := strings.ToLower(rs)
			if seen[low] {
				continue
			}
			seen[low] = true
			repos = append(repos, rs)
		}
	}

	if len(repos) > maxRepos {
		httpx.HandleErr(w, r, httpx.ErrValidation("repos", "api.github.repos_max", map[string]any{"max": maxRepos}))
		return
	}
	for _, repo := range repos {
		if !repoFormatRegex.MatchString(repo) || strings.HasSuffix(strings.ToLower(repo), ".git") {
			httpx.HandleErr(w, r, httpx.ErrValidation("repos", "api.github.repo_invalid", map[string]any{"repo": repo}))
			return
		}
	}
	if enabled && len(repos) == 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("repos", "api.github.repos_required_when_enabled"))
		return
	}

	if repos == nil {
		repos = []string{}
	}
	reposJSON, _ := json.Marshal(repos)
	nowStr := dbx.FormatDBTime(time.Now().UTC())

	var existingID int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM project_github_settings WHERE project_id = ?`, pid).Scan(&existingID)
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	if err == sql.ErrNoRows {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO project_github_settings (project_id, enabled, repos, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, pid, enabledInt, string(reposJSON), nowStr, nowStr)
	} else if err == nil {
		_, err = s.db.ExecContext(ctx, `
			UPDATE project_github_settings SET enabled = ?, repos = ?, updated_at = ? WHERE project_id = ?
		`, enabledInt, string(reposJSON), nowStr, pid)
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	httpx.Success(w, map[string]any{
		"project_id": pid,
		"enabled":    enabled,
		"repos":      repos,
	})
}

func (s *Service) HandleTaskItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	tid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var pid int64
	var code string
	err = s.db.QueryRowContext(ctx, `SELECT project_id, code FROM tasks WHERE id = ?`, tid).Scan(&pid, &code)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.github.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(ctx, s.db, u, pid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	items := s.matchingItemsForCode(ctx, pid, code)
	httpx.Success(w, items)
}

func (s *Service) HandleBugItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u := httpx.UserFrom(ctx)
	bid, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	var pid int64
	var code string
	err = s.db.QueryRowContext(ctx, `SELECT project_id, code FROM bugs WHERE id = ?`, bid).Scan(&pid, &code)
	if err == sql.ErrNoRows {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.github.not_found"))
		return
	}
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if _, err := access.ProjectForOrFail(ctx, s.db, u, pid); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	items := s.matchingItemsForCode(ctx, pid, code)
	httpx.Success(w, items)
}

func (s *Service) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	secret := strings.TrimSpace(s.cfg.GithubWebhookSecret)
	if secret == "" {
		httpx.HandleErr(w, r, httpx.ErrUnauth("api.github.webhook_unauthorized"))
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	sigHeader := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
	if !verifyGithubSignature(secret, sigHeader, bodyBytes) {
		httpx.HandleErr(w, r, httpx.ErrUnauth("api.github.webhook_unauthorized"))
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	deliveryID := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	nowStr := dbx.FormatDBTime(time.Now().UTC())

	if deliveryID != "" {
		res, err := s.db.ExecContext(ctx, s.db.InsertIgnore()+` INTO github_webhook_deliveries (delivery_id, event, received_at) VALUES (?, ?, ?)`, deliveryID, event, nowStr)
		if err == nil {
			aff, _ := res.RowsAffected()
			if aff == 0 {
				httpx.Success(w, map[string]any{"ok": true, "duplicate": true})
				return
			}
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		httpx.Success(w, map[string]any{"ok": true, "ignored": true})
		return
	}

	repoObj, _ := payload["repository"].(map[string]any)
	repoName := strings.TrimSpace(strVal(repoObj["full_name"]))
	if repoName == "" {
		httpx.Success(w, map[string]any{"ok": true, "ignored": true})
		return
	}

	saved := 0
	switch event {
	case "pull_request":
		pr, _ := payload["pull_request"].(map[string]any)
		if pr != nil {
			num := int64Val(pr["number"])
			if num == 0 {
				num = int64Val(payload["number"])
			}
			extID := fmt.Sprintf("pr:%d", num)
			title := firstLine(strVal(pr["title"]))
			htmlURL := strVal(pr["html_url"])
			merged, _ := pr["merged"].(bool)
			state := strVal(pr["state"])
			if merged {
				state = "merged"
			}
			head, _ := pr["head"].(map[string]any)
			ref := strVal(head["ref"])
			prUser, _ := pr["user"].(map[string]any)
			author := strVal(prUser["login"])
			if author == "" {
				author = "github"
			}
			at := strVal(pr["merged_at"])
			if at == "" {
				at = strVal(pr["updated_at"])
			}
			if at == "" {
				at = nowStr
			}
			if strings.HasPrefix(htmlURL, "https://github.com/") && title != "" {
				if s.upsertItem(ctx, extID, "pr", repoName, ref, title, &state, author, at, htmlURL) {
					saved++
				}
			}
			action := strVal(payload["action"])
			if action == "closed" && merged {
				s.notifyMergedPR(ctx, repoName, title, extID)
			}
		}
	case "push":
		ref := strings.TrimPrefix(strVal(payload["ref"]), "refs/heads/")
		if commits, ok := payload["commits"].([]any); ok {
			for _, raw := range commits {
				c, _ := raw.(map[string]any)
				if c == nil {
					continue
				}
				sha := strVal(c["id"])
				msg := firstLine(strVal(c["message"]))
				url := strVal(c["url"])
				authorObj, _ := c["author"].(map[string]any)
				author := strVal(authorObj["username"])
				if author == "" {
					author = strVal(authorObj["name"])
				}
				if author == "" {
					author = "github"
				}
				at := strVal(c["timestamp"])
				if at == "" {
					at = nowStr
				}
				if sha != "" && msg != "" && strings.HasPrefix(url, "https://github.com/") {
					if s.upsertItem(ctx, "commit:"+sha, "commit", repoName, ref, msg, nil, author, at, url) {
						saved++
					}
				}
			}
		}
	}

	httpx.Success(w, map[string]any{"ok": true, "event": event, "saved": saved})
}

func (s *Service) loadProjectSettings(ctx context.Context, projectID int64) (bool, []string) {
	var enabledInt int
	var rawRepos string
	err := s.db.QueryRowContext(ctx, `SELECT enabled, repos FROM project_github_settings WHERE project_id = ?`, projectID).Scan(&enabledInt, &rawRepos)
	if err != nil {
		return false, []string{}
	}
	var repos []string
	if strings.TrimSpace(rawRepos) != "" {
		_ = json.Unmarshal([]byte(rawRepos), &repos)
	}
	if repos == nil {
		repos = []string{}
	}
	return enabledInt != 0, repos
}

func (s *Service) matchingItemsForCode(ctx context.Context, projectID int64, code string) []map[string]any {
	enabled, repos := s.loadProjectSettings(ctx, projectID)
	if !enabled || len(repos) == 0 || code == "" {
		return []map[string]any{}
	}
	lowRepos := make([]any, 0, len(repos))
	for _, repo := range repos {
		lowRepos = append(lowRepos, strings.ToLower(repo))
	}
	ph := dbx.Placeholders(len(lowRepos))
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT id, external_id, kind, repo, ref, title, state, author, at, url
		FROM github_items
		WHERE LOWER(repo) IN (%s)
		ORDER BY at DESC, id DESC
		LIMIT 200
	`, ph), lowRepos...)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()

	re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(code) + `($|[^0-9])`)
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var extID, kind, repo, ref, title, author, at, url string
		var state sql.NullString
		if rows.Scan(&id, &extID, &kind, &repo, &ref, &title, &state, &author, &at, &url) != nil {
			continue
		}
		if !strings.HasPrefix(url, "https://github.com/") {
			continue
		}
		if !re.MatchString(title) && !re.MatchString(ref) {
			continue
		}
		var statePtr *string
		if state.Valid && state.String != "" {
			st := state.String
			statePtr = &st
		}
		out = append(out, map[string]any{
			"id":          id,
			"external_id": extID,
			"kind":        kind,
			"repo":        repo,
			"ref":         ref,
			"title":       title,
			"state":       statePtr,
			"author":      author,
			"at":          at,
			"url":         url,
		})
	}
	return out
}

func (s *Service) upsertItem(ctx context.Context, extID, kind, repo, ref, title string, state *string, author, at, url string) bool {
	if len(title) > 255 {
		title = title[:255]
	}
	var existingID int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM github_items WHERE LOWER(repo) = LOWER(?) AND external_id = ?`, repo, extID).Scan(&existingID)
	if err == nil {
		_, _ = s.db.ExecContext(ctx, `UPDATE github_items SET kind=?, ref=?, title=?, state=?, author=?, at=?, url=? WHERE id=?`,
			kind, ref, title, state, author, at, url, existingID)
		return true
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO github_items (external_id, kind, repo, ref, title, state, author, at, url, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, extID, kind, repo, ref, title, state, author, at, url, dbx.FormatDBTime(time.Now().UTC()))
	return err == nil
}

func (s *Service) notifyMergedPR(ctx context.Context, repoName, title, extID string) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, enabled, repos FROM project_github_settings WHERE enabled = 1`)
	if err != nil {
		return
	}
	var matchingProjectIDs []int64
	for rows.Next() {
		var pid int64
		var en int
		var rawRepos string
		if rows.Scan(&pid, &en, &rawRepos) != nil {
			continue
		}
		var repos []string
		_ = json.Unmarshal([]byte(rawRepos), &repos)
		for _, r := range repos {
			if strings.EqualFold(r, repoName) {
				matchingProjectIDs = append(matchingProjectIDs, pid)
				break
			}
		}
	}
	_ = rows.Close()
	if len(matchingProjectIDs) == 0 {
		return
	}

	projs, err := access.LoadAllProjects(ctx, s.db)
	if err != nil {
		return
	}
	projMap := map[int64]*access.Project{}
	for _, p := range projs {
		projMap[p.ID] = p
	}

	ph, args := dbx.InInt64(matchingProjectIDs)
	trows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT id, project_id, code, title, assignee_id FROM tasks WHERE project_id IN (%s)`, ph), args...)
	if err != nil {
		return
	}
	type taskHit struct {
		tid, pid int64
		code     string
		title    string
		assignee sql.NullInt64
	}
	var hits []taskHit
	for trows.Next() {
		var h taskHit
		if trows.Scan(&h.tid, &h.pid, &h.code, &h.title, &h.assignee) == nil {
			hits = append(hits, h)
		}
	}
	_ = trows.Close()

	for _, h := range hits {
		re := regexp.MustCompile(`(?i)(^|[^A-Za-z0-9-])` + regexp.QuoteMeta(h.code) + `($|[^0-9])`)
		if !re.MatchString(title) {
			continue
		}
		p := projMap[h.pid]
		if p == nil {
			continue
		}
		recs := []int64{p.PMID}
		if h.assignee.Valid && h.assignee.Int64 > 0 && h.assignee.Int64 != p.PMID {
			recs = append(recs, h.assignee.Int64)
		}
		d := notifications.AboutTask(h.code, h.title, p.Name, p.NameEn)
		d["pr_title"] = title
		d["repo"] = repoName
		dk := fmt.Sprintf("gh_pr_merged:%s:%s:t%d", repoName, extID, h.tid)
		s.notif.Notify(ctx, recs, "github_pr_merged", notifications.About{
			ProjectID: &h.pid,
			TaskID:    &h.tid,
			Data:      d,
			DedupKey:  &dk,
		})
	}
}

func verifyGithubSignature(secret, header string, payload []byte) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	expectedHex := strings.TrimPrefix(header, "sha256=")
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hmac.Equal(mac.Sum(nil), expected)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return s
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func int64Val(v any) int64 {
	if f, ok := v.(float64); ok {
		return int64(f)
	}
	if i, ok := v.(int64); ok {
		return i
	}
	return 0
}
