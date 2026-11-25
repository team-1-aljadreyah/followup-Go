package httpx

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"followapi/internal/config"
	"followapi/internal/i18n"
	"followapi/internal/store"
	"followapi/internal/support"
)

type ctxKey string

const (
	keyLocale    ctxKey = "locale"
	keyRequestID ctxKey = "request_id"
	keyClientIP  ctxKey = "client_ip"
	keyUserAgent ctxKey = "user_agent"
	keyUser      ctxKey = "user"
	keySessionID ctxKey = "session_id"
	keyTokenID   ctxKey = "token_id"
)

// APIError mirrors App\Exceptions\ApiException cleanly.
type APIError struct {
	Status  int
	Code    string
	Key     string
	Message string
	Params  map[string]any
	Errors  map[string]string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Key
}

func ErrUnauth(key ...string) *APIError {
	k := "api.unauthenticated"
	if len(key) > 0 && key[0] != "" {
		k = key[0]
	}
	return &APIError{Status: http.StatusUnauthorized, Code: "UNAUTHENTICATED", Key: k}
}

func ErrForbidden(key string, params ...map[string]any) *APIError {
	var p map[string]any
	if len(params) > 0 {
		p = params[0]
	}
	if key == "" {
		key = "api.forbidden"
	}
	return &APIError{Status: http.StatusForbidden, Code: "FORBIDDEN", Key: key, Params: p}
}

func ErrNotFound(key ...string) *APIError {
	k := "api.not_found"
	if len(key) > 0 && key[0] != "" {
		k = key[0]
	}
	return &APIError{Status: http.StatusNotFound, Code: "NOT_FOUND", Key: k}
}

func ErrValidation(field, key string, params ...map[string]any) *APIError {
	var p map[string]any
	if len(params) > 0 {
		p = params[0]
	}
	errs := map[string]string{}
	if field != "" {
		errs[field] = key
	}
	return &APIError{Status: http.StatusUnprocessableEntity, Code: "VALIDATION", Key: key, Params: p, Errors: errs}
}

func ErrValidationMap(messageKey string, fieldKeys map[string]string) *APIError {
	return &APIError{Status: http.StatusUnprocessableEntity, Code: "VALIDATION", Key: messageKey, Errors: fieldKeys}
}

func ErrConflict(key string) *APIError {
	return &APIError{Status: http.StatusConflict, Code: "BAD_STATE", Key: key}
}

func ErrRateLimited(key string, params ...map[string]any) *APIError {
	var p map[string]any
	if len(params) > 0 {
		p = params[0]
	}
	if key == "" {
		key = "api.rate_limited"
	}
	return &APIError{Status: http.StatusTooManyRequests, Code: "RATE_LIMITED", Key: key, Params: p}
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

func Success(w http.ResponseWriter, data any, status ...int) {
	code := http.StatusOK
	if len(status) > 0 && status[0] > 0 {
		code = status[0]
	}
	WriteJSON(w, code, map[string]any{
		"success": true,
		"data":    data,
	})
}

func HandleErr(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	locale := LocaleFrom(r.Context())
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if msg == "" && apiErr.Key != "" {
			msg = i18n.T(locale, apiErr.Key, apiErr.Params)
		}
		if msg == "" {
			msg = i18n.T(locale, "api.validation_failed")
		}
		payload := map[string]any{
			"success": false,
			"message": msg,
			"code":    apiErr.Code,
		}
		if len(apiErr.Errors) > 0 {
			translated := make(map[string]string, len(apiErr.Errors))
			for f, val := range apiErr.Errors {
				if strings.HasPrefix(val, "api.") {
					translated[f] = i18n.T(locale, val, apiErr.Params)
				} else {
					translated[f] = val
				}
			}
			payload["errors"] = translated
		}
		WriteJSON(w, apiErr.Status, payload)
		return
	}
	log.Printf("[ERROR] %s %s: %v", r.Method, r.URL.Path, err)
	WriteJSON(w, http.StatusInternalServerError, map[string]any{
		"success": false,
		"message": "Server Error",
		"code":    "SERVER_ERROR",
	})
}

func DecodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	return dec.Decode(dst)
}

func DecodeMap(r *http.Request) (map[string]any, error) {
	m := map[string]any{}
	if err := DecodeJSON(r, &m); err != nil {
		return nil, ErrValidation("payload", "api.validation_failed")
	}
	return m, nil
}

func URLInt64(r *http.Request, param string) (int64, error) {
	raw := chi.URLParam(r, param)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrNotFound()
	}
	return id, nil
}

// Context helpers

func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, keyLocale, locale)
}

func LocaleFrom(ctx context.Context) string {
	if v, ok := ctx.Value(keyLocale).(string); ok && (v == "ar" || v == "en") {
		return v
	}
	return "ar"
}

func WithRequestMeta(ctx context.Context, reqID, ip, ua string) context.Context {
	ctx = context.WithValue(ctx, keyRequestID, reqID)
	ctx = context.WithValue(ctx, keyClientIP, ip)
	ctx = context.WithValue(ctx, keyUserAgent, ua)
	return ctx
}

func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok && v != "" {
		return v
	}
	return "cli_" + support.RandomHex(5)
}

func ClientIPFrom(ctx context.Context) *string {
	if v, ok := ctx.Value(keyClientIP).(string); ok && v != "" {
		return &v
	}
	return nil
}

func UserAgentFrom(ctx context.Context) *string {
	if v, ok := ctx.Value(keyUserAgent).(string); ok && v != "" {
		return &v
	}
	return nil
}

func WithAuth(ctx context.Context, user *store.User, sessionID string, tokenID int64) context.Context {
	ctx = context.WithValue(ctx, keyUser, user)
	ctx = context.WithValue(ctx, keySessionID, sessionID)
	ctx = context.WithValue(ctx, keyTokenID, tokenID)
	return ctx
}

func UserFrom(ctx context.Context) *store.User {
	if v, ok := ctx.Value(keyUser).(*store.User); ok {
		return v
	}
	return nil
}

func SessionIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(keySessionID).(string); ok {
		return v
	}
	return ""
}

func TokenIDFrom(ctx context.Context) int64 {
	if v, ok := ctx.Value(keyTokenID).(int64); ok {
		return v
	}
	return 0
}

// Middleware: RequestContext & Locale

var reqIDRegex = regexp.MustCompile(`^[a-zA-Z0-9._-]{4,60}$`)

var cloudflareCIDRs = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

func RequestContext(cfg config.Config) func(http.Handler) http.Handler {
	var trustedNets []*net.IPNet
	for _, c := range append(append([]string{}, cloudflareCIDRs...), cfg.TrustedProxyCIDRs...) {
		if _, netw, err := net.ParseCIDR(c); err == nil {
			trustedNets = append(trustedNets, netw)
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			locale := "ar"
			acceptLang := strings.ToLower(r.Header.Get("Accept-Language"))
			if strings.HasPrefix(acceptLang, "en") || (strings.Contains(acceptLang, "en") && !strings.HasPrefix(acceptLang, "ar")) {
				locale = "en"
			}
			reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
			if !reqIDRegex.MatchString(reqID) {
				reqID = "req_" + support.RandomHex(6)
			}

			ip := extractClientIP(r, trustedNets)
			ua := support.TruncateRunes(strings.TrimSpace(r.Header.Get("User-Agent")), 300)

			ctx := WithLocale(r.Context(), locale)
			ctx = WithRequestMeta(ctx, reqID, ip, ua)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	parsed := net.ParseIP(host)
	if parsed == nil {
		return ""
	}
	isTrusted := false
	for _, n := range trusted {
		if n.Contains(parsed) {
			isTrusted = true
			break
		}
	}
	if isTrusted {
		if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" && net.ParseIP(cf) != nil {
			return cf
		}
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" && net.ParseIP(first) != nil {
				return first
			}
		}
	}
	return parsed.String()
}

func OriginAllowedFn(cfg config.Config) func(string) bool {
	var patterns []*regexp.Regexp
	for _, p := range cfg.AllowedPatterns {
		p = strings.Trim(p, "/#")
		if re, err := regexp.Compile(p); err == nil {
			patterns = append(patterns, re)
		}
	}
	allowedSet := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowedSet[o] = true
	}
	return func(origin string) bool {
		if allowedSet[origin] {
			return true
		}
		for _, re := range patterns {
			if re.MatchString(origin) {
				return true
			}
		}
		return false
	}
}

func CORS(cfg config.Config) func(http.Handler) http.Handler {
	isAllowed := OriginAllowedFn(cfg)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && isAllowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, Accept-Language, X-Requested-With")
				w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
				w.Header().Set("Access-Control-Max-Age", "3600")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Simple fixed-window in-memory rate limiter matching Laravel throttle:login / throttle:api
type bucket struct {
	count int
	reset time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]bucket
	limit   int
	window  time.Duration
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string]bucket),
		limit:   limit,
		window:  window,
	}
}

func (rl *RateLimiter) Middleware(keyFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rl.limit <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			key := keyFn(r)
			now := time.Now()
			rl.mu.Lock()
			b, ok := rl.buckets[key]
			if !ok || now.After(b.reset) {
				b = bucket{count: 0, reset: now.Add(rl.window)}
			}
			b.count++
			rl.buckets[key] = b
			exceeded := b.count > rl.limit
			rl.mu.Unlock()

			if exceeded {
				HandleErr(w, r, ErrRateLimited("api.rate_limited"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[PANIC] %s %s: %v", r.Method, r.URL.Path, rec)
				WriteJSON(w, http.StatusInternalServerError, map[string]any{
					"success": false,
					"message": "Server Error",
					"code":    "SERVER_ERROR",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

var gzipWriterPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return gz
	},
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	decided     bool
	useGzip     bool
	wroteHeader bool
	status      int
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

func (g *gzipResponseWriter) shouldCompress(code int) bool {
	if code == http.StatusNoContent || code == http.StatusNotModified || (code >= 100 && code < 200) {
		return false
	}
	if g.Header().Get("Content-Encoding") != "" {
		return false
	}
	ct := strings.ToLower(g.Header().Get("Content-Type"))
	return strings.HasPrefix(ct, "application/json") ||
		strings.HasPrefix(ct, "text/") ||
		strings.HasPrefix(ct, "application/yaml")
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.status = code
	if !g.decided {
		g.decided = true
		g.useGzip = g.shouldCompress(code)
		if g.useGzip {
			g.Header().Del("Content-Length")
			g.Header().Set("Content-Encoding", "gzip")
			g.Header().Add("Vary", "Accept-Encoding")
			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(g.ResponseWriter)
			g.gz = gz
		}
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.useGzip && g.gz != nil {
		return g.gz.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

func (g *gzipResponseWriter) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(io.Discard)
		gzipWriterPool.Put(g.gz)
		g.gz = nil
	}
}

// Gzip compresses text, YAML, and JSON API responses when the client accepts gzip.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead ||
			strings.HasSuffix(r.URL.Path, "/ws") ||
			strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") ||
			r.Header.Get("Upgrade") != "" ||
			!strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, status: http.StatusOK}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}
