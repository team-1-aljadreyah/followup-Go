package avatars

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"followapi/internal/access"
	"followapi/internal/audit"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/httpx"
	"followapi/internal/store"
	"followapi/internal/support"
)

const (
	MaxUploadBytes = 200 * 1024
	MaxSide        = 256
)

type Service struct {
	db    *dbx.DB
	cfg   config.Config
	audit *audit.Service
}

func New(db *dbx.DB, cfg config.Config, auditSvc *audit.Service) *Service {
	return &Service{db: db, cfg: cfg, audit: auditSvc}
}

func (s *Service) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	raw, err := extractAvatarUpload(r)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if len(raw) == 0 {
		httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.profile.file_empty"))
		return
	}
	if len(raw) > MaxUploadBytes {
		httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.profile.file_too_large", map[string]any{"kb": MaxUploadBytes / 1024}))
		return
	}
	if !detectMagicImage(raw) {
		httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.profile.file_invalid_type"))
		return
	}

	encoded, width, height, err := reencodeStripMetadata(raw)
	if err != nil {
		httpx.HandleErr(w, r, httpx.ErrValidation("file", "api.profile.file_invalid_type"))
		return
	}

	cacheKey := support.RandomHex(16)
	diskRel := fmt.Sprintf("avatars/%d/%s.jpg", u.ID, cacheKey)
	fullPath := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(diskRel))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if err := os.WriteFile(fullPath, encoded, 0o644); err != nil {
		httpx.HandleErr(w, r, err)
		return
	}

	var oldPaths []string
	nowStr := dbx.FormatDBTime(time.Now().UTC())
	err = s.db.Tx(r.Context(), func(tx *sql.Tx) error {
		rows, _ := tx.QueryContext(r.Context(), `SELECT disk_path FROM avatars WHERE user_id = ?`, u.ID)
		if rows != nil {
			for rows.Next() {
				var p string
				if rows.Scan(&p) == nil {
					oldPaths = append(oldPaths, p)
				}
			}
			_ = rows.Close()
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM avatars WHERE user_id = ?`, u.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `
			INSERT INTO avatars (user_id, disk_path, mime, size, width, height, cache_key, created_at)
			VALUES (?, ?, 'image/jpeg', ?, ?, ?, ?, ?)
		`, u.ID, diskRel, len(encoded), width, height, cacheKey, nowStr)
		return err
	})
	if err != nil {
		_ = os.Remove(fullPath)
		httpx.HandleErr(w, r, err)
		return
	}
	for _, old := range oldPaths {
		if old != "" && old != diskRel {
			_ = os.Remove(filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(old)))
		}
	}

	store.InvalidateUsersCache()
	updated, _ := store.FindUserByID(r.Context(), s.db, s.cfg.AppURL, u.ID)
	httpx.Success(w, store.UserJSON(updated, updated))
}

func (s *Service) HandleDestroyOwn(w http.ResponseWriter, r *http.Request) {
	u := httpx.UserFrom(r.Context())
	s.deleteUserAvatars(r.Context(), u.ID)
	store.InvalidateUsersCache()
	updated, _ := store.FindUserByID(r.Context(), s.db, s.cfg.AppURL, u.ID)
	httpx.Success(w, store.UserJSON(updated, updated))
}

func (s *Service) HandleDestroyUser(w http.ResponseWriter, r *http.Request) {
	actor := httpx.UserFrom(r.Context())
	id, err := httpx.URLInt64(r, "id")
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	target, err := store.FindUserByID(r.Context(), s.db, s.cfg.AppURL, id)
	if err != nil {
		httpx.HandleErr(w, r, err)
		return
	}
	if target == nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound("api.profile.user_not_found"))
		return
	}
	if !access.CanAdminister(actor) {
		httpx.HandleErr(w, r, httpx.ErrForbidden("api.profile.remove_forbidden"))
		return
	}
	had := s.deleteUserAvatars(r.Context(), target.ID)
	if had && target.ID != actor.ID {
		s.audit.Log(r.Context(), &actor.ID, "picture_removed", "user", &target.ID, target.Name, nil)
	}
	updated, _ := store.FindUserByID(r.Context(), s.db, s.cfg.AppURL, target.ID)
	httpx.Success(w, store.UserJSON(updated, actor))
}

var cacheFileRegex = regexp.MustCompile(`^([a-f0-9]{32})\.(webp|jpg)$`)

func (s *Service) HandleShow(w http.ResponseWriter, r *http.Request) {
	uid, err := httpx.URLInt64(r, "user")
	if err != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound())
		return
	}
	file := chi.URLParam(r, "file")
	m := cacheFileRegex.FindStringSubmatch(file)
	if m == nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound())
		return
	}
	cacheKey := m[1]

	var diskPath, mimeType string
	err = s.db.QueryRowContext(r.Context(), `SELECT disk_path, mime FROM avatars WHERE user_id = ? AND cache_key = ? LIMIT 1`, uid, cacheKey).
		Scan(&diskPath, &mimeType)
	if err != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound())
		return
	}
	fullPath := filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(diskPath))
	if _, statErr := os.Stat(fullPath); statErr != nil {
		httpx.HandleErr(w, r, httpx.ErrNotFound())
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, fullPath)
}

func (s *Service) deleteUserAvatars(ctx context.Context, userID int64) bool {
	rows, err := s.db.QueryContext(ctx, `SELECT disk_path FROM avatars WHERE user_id = ?`, userID)
	if err != nil {
		return false
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	_ = rows.Close()
	if len(paths) == 0 {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM avatars WHERE user_id = ?`, userID)
	store.InvalidateUsersCache()
	for _, p := range paths {
		if p != "" {
			_ = os.Remove(filepath.Join(s.cfg.StorageRoot, filepath.FromSlash(p)))
		}
	}
	return true
}

func extractAvatarUpload(r *http.Request) ([]byte, error) {
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err == nil && strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		mr := multipart.NewReader(io.LimitReader(r.Body, MaxUploadBytes+64*1024), params["boundary"])
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				return nil, perr
			}
			if part.FormName() == "file" {
				return io.ReadAll(io.LimitReader(part, MaxUploadBytes+1024))
			}
		}
		return nil, nil
	}
	return io.ReadAll(io.LimitReader(r.Body, MaxUploadBytes+1024))
}

func detectMagicImage(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	if bytes.HasPrefix(b, []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return true
	}
	if bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}) {
		return true
	}
	if string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return true
	}
	return false
}

func reencodeStripMetadata(raw []byte) ([]byte, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, err
	}
	b := src.Bounds()
	srcW, srcH := b.Dx(), b.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, 0, 0, fmt.Errorf("empty image")
	}
	maxDim := srcW
	if srcH > maxDim {
		maxDim = srcH
	}
	scale := math.Min(1.0, float64(MaxSide)/float64(maxDim))
	dstW := int(math.Round(float64(srcW) * scale))
	if dstW < 1 {
		dstW = 1
	}
	dstH := int(math.Round(float64(srcH) * scale))
	if dstH < 1 {
		dstH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	scaled := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	xdraw.CatmullRom.Scale(scaled, scaled.Bounds(), src, b, xdraw.Over, nil)
	draw.Draw(dst, dst.Bounds(), scaled, image.Point{}, draw.Over)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), dstW, dstH, nil
}
