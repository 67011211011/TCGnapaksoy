package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionTTL = 12 * time.Hour

type ctxKey int

const sessKey ctxKey = 1

type Session struct {
	Hash string
	CSRF string
	User *User // nil = ยังไม่ล็อกอิน
}

func sess(r *http.Request) *Session {
	s, _ := r.Context().Value(sessKey).(*Session)
	return s
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// เก็บเฉพาะ hash ของ session token ในฐานข้อมูล ถ้า DB รั่ว ผู้โจมตีก็นำไปสวมรอยไม่ได้
func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (a *App) cookieName() string {
	if a.secureCookie {
		return "__Host-sid"
	}
	return "sid"
}

func (a *App) newSession(w http.ResponseWriter, userID int64) *Session {
	token := randToken(32) // 43 ตัวอักษร
	s := &Session{Hash: hashToken(token), CSRF: randToken(32)}
	var uid any
	if userID > 0 {
		uid = userID
	}
	_, err := a.db.Exec(`INSERT INTO sessions(token_hash,user_id,csrf,expires_at) VALUES(?,?,?,?)`,
		s.Hash, uid, s.CSRF, time.Now().Add(sessionTTL).Unix())
	if err != nil {
		log.Printf("create session: %v", err)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	if userID > 0 {
		s.User, _ = a.userByID(userID)
	}
	return s
}

func (a *App) destroySession(hash string) {
	_, _ = a.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hash)
}

func (a *App) loadSession(r *http.Request) *Session {
	c, err := r.Cookie(a.cookieName())
	if err != nil || len(c.Value) != 43 {
		return nil
	}
	h := hashToken(c.Value)
	var csrf string
	var uid sql.NullInt64
	var exp int64
	err = a.db.QueryRow(`SELECT csrf,user_id,expires_at FROM sessions WHERE token_hash=?`, h).Scan(&csrf, &uid, &exp)
	if err != nil || exp < time.Now().Unix() {
		return nil
	}
	s := &Session{Hash: h, CSRF: csrf}
	if uid.Valid {
		u, err := a.userByID(uid.Int64) // อ่าน role สดจาก DB ทุกครั้ง: ถูกลดสิทธิ์/ลบ มีผลทันที
		if err != nil {
			return nil
		}
		s.User = u
	}
	return s
}

// ---------- Middleware ----------

func isAsset(path string) bool {
	return strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/img/") || strings.HasPrefix(path, "/avatar/")
}

// เฉพาะ 3 เส้นทางนี้ที่รับการอัปโหลดรูป (multipart) ที่เหลือรับได้แค่ฟอร์มปกติ
func allowsMultipart(path string) bool {
	return path == "/auctions" || path == "/profile/avatar" ||
		(strings.HasPrefix(path, "/auctions/") && strings.HasSuffix(path, "/edit"))
}

const (
	maxFormBody   = 64 << 10 // ฟอร์มปกติ 64KB
	maxUploadBody = 3 << 20  // ฟอร์มที่แนบรูป 3MB (รูปไม่เกิน 2MB + ฟิลด์อื่น)
)

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if a.secureCookie {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if !isAsset(r.URL.Path) {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) sessionMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAsset(r.URL.Path) { // ไฟล์/รูป: ไม่สร้าง session (กันตารางบวมจากการยิงรูปซ้ำ ๆ)
			next.ServeHTTP(w, r)
			return
		}
		s := a.loadSession(r)
		if s == nil {
			s = a.newSession(w, 0)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessKey, s)))
	})
}

func (a *App) csrfMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// ชั้นที่ 1: Fetch Metadata — ปฏิเสธคำขอข้ามไซต์
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			a.errorPage(w, r, http.StatusForbidden, "คำขอไม่ถูกต้อง (cross-site)")
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			// อัปโหลดรูป: ต้องล็อกอินและอยู่ในเส้นทางที่อนุญาตเท่านั้น (คนแปลกหน้าส่งไฟล์ใหญ่มาไม่ได้)
			if s := sess(r); !allowsMultipart(r.URL.Path) || s == nil || s.User == nil {
				a.errorPage(w, r, http.StatusForbidden, "คำขอไม่ถูกต้อง")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				a.errorPage(w, r, http.StatusBadRequest, "ข้อมูลที่ส่งมาไม่ถูกต้อง หรือไฟล์ใหญ่เกินไป (รูปต้องไม่เกิน 2MB)")
				return
			}
		} else {
			// จำกัดขนาด body 64KB กันส่งข้อมูลมหาศาล
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
			if err := r.ParseForm(); err != nil {
				a.errorPage(w, r, http.StatusBadRequest, "ข้อมูลที่ส่งมาไม่ถูกต้องหรือมีขนาดใหญ่เกินไป")
				return
			}
		}
		// ชั้นที่ 2: CSRF token ผูกกับ session
		s := sess(r)
		tok := r.PostFormValue("csrf")
		if s == nil || subtle.ConstantTimeCompare([]byte(tok), []byte(s.CSRF)) != 1 {
			a.errorPage(w, r, http.StatusForbidden, "CSRF token ไม่ถูกต้อง กรุณากลับไปโหลดหน้าฟอร์มใหม่")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth: ต้องล็อกอินก่อน
func (a *App) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s := sess(r); s == nil || s.User == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// admin: ต้องเป็น role = admin (ตรวจฝั่งเซิร์ฟเวอร์ทุกคำขอ)
func (a *App) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := sess(r)
		if s == nil || s.User == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if s.User.Role != "admin" {
			a.errorPage(w, r, http.StatusForbidden, "คุณไม่มีสิทธิ์เข้าถึงหน้านี้ (ต้องเป็นผู้ดูแลระบบ)")
			return
		}
		h(w, r)
	}
}

// ---------- Rate limiter ----------

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

func (l *limiter) allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

func (l *limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-time.Hour)
	for k, v := range l.hits {
		if len(v) == 0 || v[len(v)-1].Before(cut) {
			delete(l.hits, k)
		}
	}
}

func (a *App) clientIP(r *http.Request) string {
	if a.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1]) // ค่าที่ proxy ของโฮสต์ต่อท้ายให้ ปลอมไม่ได้
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
