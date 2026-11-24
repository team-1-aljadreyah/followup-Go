package audit

import (
	"context"
	"encoding/json"
	"time"

	"followapi/internal/dbx"
	"followapi/internal/httpx"
)

type Change struct {
	Field string  `json:"field"`
	From  *string `json:"from,omitempty"`
	To    *string `json:"to,omitempty"`
}

type Service struct {
	db *dbx.DB
}

func New(db *dbx.DB) *Service {
	return &Service{db: db}
}

func StrPtr(s string) *string {
	return &s
}

func (s *Service) Log(ctx context.Context, actorID *int64, action, targetType string, targetID *int64, targetLabel string, changes []Change) {
	var changesJSON *string
	if len(changes) > 0 {
		raw, err := json.Marshal(changes)
		if err == nil {
			str := string(raw)
			changesJSON = &str
		}
	}

	var sid *string
	if actorID != nil {
		if cur := httpx.SessionIDFrom(ctx); cur != "" {
			sid = &cur
		}
	}
	reqID := httpx.RequestIDFrom(ctx)

	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO audit_logs (actor_id, action, target_type, target_id, target_label, changes, at, ip, user_agent, session_id, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, actorID, action, targetType, targetID, targetLabel, changesJSON, dbx.FormatDBTime(time.Now().UTC()),
		httpx.ClientIPFrom(ctx), httpx.UserAgentFrom(ctx), sid, reqID)
}
