package main

import (
	"bytes"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const tooMany = "คุณทำรายการถี่เกินไป กรุณารอสักครู่แล้วลองใหม่"

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{3,30}$`)

// ข้อความ flash ใช้รหัสที่กำหนดไว้เท่านั้น (กัน content spoofing ผ่าน query string)
var flashes = map[string]string{
	"registered":  "สมัครสมาชิกสำเร็จ กรุณาเข้าสู่ระบบ",
	"loggedout":   "ออกจากระบบแล้ว",
	"created":     "ลงขายการ์ดเรียบร้อย เปิดประมูลแล้ว",
	"updated":     "แก้ไขข้อมูลการ์ดเรียบร้อย",
	"deleted":     "ลบเรียบร้อย",
	"bid":         "ประมูลสำเร็จ! ตอนนี้คุณเป็นผู้นำราคา",
	"cancelled":   "ยกเลิกการประมูลเรียบร้อย",
	"role":        "เปลี่ยนบทบาทเรียบร้อย",
	"userdeleted": "ลบผู้ใช้เรียบร้อย",
	"avatar":      "อัปเดตรูปโปรไฟล์เรียบร้อย",
	"avatardel":   "ลบรูปโปรไฟล์เรียบร้อย",
}

type Page struct {
	Title  string
	User   *User
	CSRF   string
	Flash  string
	Errors []string
	Form   map[string]string
	Data   any
}

func (a *App) page(r *http.Request, title string) *Page {
	p := &Page{Title: title, Form: map[string]string{}}
	if s := sess(r); s != nil {
		p.User = s.User
		p.CSRF = s.CSRF
	}
	if m, ok := flashes[r.URL.Query().Get("ok")]; ok {
		p.Flash = m
	}
	return p
}

func (a *App) render(w http.ResponseWriter, status int, name string, p *Page) {
	var buf bytes.Buffer
	if err := a.tpl.ExecuteTemplate(&buf, name, p); err != nil {
		log.Printf("template %s: %v", name, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (a *App) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	p := a.page(r, "เกิดข้อผิดพลาด")
	p.Data = msg
	a.render(w, status, "error", p)
}

func (a *App) serverError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("ERROR %s %s: %v", r.Method, r.URL.Path, err) // log รายละเอียดฝั่งเซิร์ฟเวอร์ ไม่แสดงให้ผู้ใช้เห็น
	a.errorPage(w, r, http.StatusInternalServerError, "เกิดข้อผิดพลาดภายในระบบ กรุณาลองใหม่อีกครั้ง")
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	a.errorPage(w, r, http.StatusNotFound, "ไม่พบหน้าที่ต้องการ")
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

// ---------- Template helpers ----------

func fmtBaht(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return "฿" + string(out)
}

type Avatar struct {
	ID   int64
	Name string
	Ver  int64
}

func makeAvatar(id int64, name string, ver int64) Avatar { return Avatar{ID: id, Name: name, Ver: ver} }

func initial(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return "?"
	}
	return strings.ToUpper(string(r[0]))
}

func condLabel(c string) string {
	switch c {
	case "NM":
		return "Near Mint (NM)"
	case "LP":
		return "Lightly Played (LP)"
	case "MP":
		return "Moderately Played (MP)"
	case "HP":
		return "Heavily Played (HP)"
	}
	return c
}

// ซ่อนชื่อผู้ใช้บางส่วน เช่น "napaksoy" → "na***" (ปกป้องความเป็นส่วนตัวของผู้ประมูล)
func maskName(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return "-"
	}
	if len(r) <= 2 {
		return string(r[:1]) + "***"
	}
	return string(r[:2]) + "***"
}

// ---------- Validation ----------

func validPassword(p string) string {
	if len(p) < 8 {
		return "รหัสผ่านต้องยาวอย่างน้อย 8 ตัวอักษร"
	}
	if len(p) > 72 {
		return "รหัสผ่านต้องไม่เกิน 72 ไบต์"
	}
	var letter, digit bool
	for _, c := range p {
		if unicode.IsLetter(c) {
			letter = true
		}
		if unicode.IsDigit(c) {
			digit = true
		}
	}
	if !letter || !digit {
		return "รหัสผ่านต้องมีทั้งตัวอักษรและตัวเลข"
	}
	return ""
}

func checkLen(field, v string, min, max int) string {
	n := utf8.RuneCountInString(v)
	if n < min || n > max {
		return field + " ต้องยาว " + strconv.Itoa(min) + "-" + strconv.Itoa(max) + " ตัวอักษร"
	}
	return ""
}

func clean(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

func collect(msgs ...string) []string {
	var out []string
	for _, m := range msgs {
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---------- Auth ----------

func (a *App) registerForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "register", a.page(r, "สมัครสมาชิก"))
}

func (a *App) registerPost(w http.ResponseWriter, r *http.Request) {
	if !a.limiter.allow("reg|"+a.clientIP(r), 5, time.Hour) {
		a.errorPage(w, r, http.StatusTooManyRequests, tooMany)
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	pw := r.PostFormValue("password")
	pw2 := r.PostFormValue("password2")

	var errs []string
	if !usernameRe.MatchString(username) {
		errs = append(errs, "ชื่อผู้ใช้ต้องเป็น a-z, 0-9 หรือ _ ยาว 3-30 ตัวอักษร")
	}
	errs = append(errs, collect(validPassword(pw))...)
	if pw != pw2 {
		errs = append(errs, "รหัสผ่านทั้งสองช่องไม่ตรงกัน")
	}

	fail := func(errs []string) {
		p := a.page(r, "สมัครสมาชิก")
		p.Errors = errs
		p.Form = map[string]string{"username": username}
		a.render(w, http.StatusUnprocessableEntity, "register", p)
	}
	if len(errs) > 0 {
		fail(errs)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// role ถูกกำหนดฝั่งเซิร์ฟเวอร์เป็น 'user' เสมอ (ไม่รับ role จากฟอร์ม กัน mass assignment)
	_, err = a.db.Exec(`INSERT INTO users(username,password_hash,role,created_at) VALUES(?,?,'user',?)`,
		username, string(hash), time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			fail([]string{"ชื่อผู้ใช้นี้ถูกใช้งานแล้ว"})
			return
		}
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/login?ok=registered", http.StatusSeeOther)
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	if s := sess(r); s != nil && s.User != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, http.StatusOK, "login", a.page(r, "เข้าสู่ระบบ"))
}

func (a *App) loginPost(w http.ResponseWriter, r *http.Request) {
	ip := a.clientIP(r)
	if !a.limiter.allow("login|"+ip, 15, 10*time.Minute) {
		a.errorPage(w, r, http.StatusTooManyRequests, tooMany)
		return
	}
	login := strings.TrimSpace(r.PostFormValue("login"))
	pw := r.PostFormValue("password")

	fail := func() {
		p := a.page(r, "เข้าสู่ระบบ")
		p.Errors = []string{"ชื่อผู้ใช้/รหัสผ่านไม่ถูกต้อง หรือบัญชีถูกล็อกชั่วคราว"} // ข้อความเดียวกันทุกกรณี
		p.Form = map[string]string{"login": login}
		a.render(w, http.StatusUnauthorized, "login", p)
	}
	if len(login) > 60 || len(pw) > 72 {
		fail()
		return
	}

	var id, locked int64
	var hash string
	var failed int
	err := a.db.QueryRow(`SELECT id,password_hash,failed_logins,locked_until FROM users WHERE username=?`,
		login).Scan(&id, &hash, &failed, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(a.dummyHash, []byte(pw)) // ใช้เวลาเท่ากับกรณีมีผู้ใช้
		fail()
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	now := time.Now().Unix()
	if locked > now {
		fail()
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) != nil {
		failed++
		if failed >= 5 { // ผิด 5 ครั้ง ล็อก 15 นาที
			_, _ = a.db.Exec(`UPDATE users SET failed_logins=0, locked_until=? WHERE id=?`, now+900, id)
			log.Printf("account locked: user_id=%d ip=%s", id, ip)
		} else {
			_, _ = a.db.Exec(`UPDATE users SET failed_logins=? WHERE id=?`, failed, id)
		}
		fail()
		return
	}
	_, _ = a.db.Exec(`UPDATE users SET failed_logins=0, locked_until=0 WHERE id=?`, id)

	// หมุน session ใหม่หลังล็อกอิน (กัน session fixation)
	a.destroySession(sess(r).Hash)
	a.newSession(w, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	a.destroySession(sess(r).Hash)
	a.newSession(w, 0)
	http.Redirect(w, r, "/login?ok=loggedout", http.StatusSeeOther)
}
