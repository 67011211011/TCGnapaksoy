package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

const bcryptCost = 12

type App struct {
	db           *sql.DB
	tpl          *template.Template
	limiter      *limiter
	dummyHash    []byte // ใช้เทียบรหัสผ่านตอนไม่พบผู้ใช้ เพื่อให้เวลาตอบสนองใกล้เคียงกัน (กัน timing/user enumeration)
	secureCookie bool
	trustProxy   bool
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fmtDate(ts int64) string {
	ict := time.FixedZone("ICT", 7*3600)
	return time.Unix(ts, 0).In(ict).Format("02/01/2006 15:04")
}

func main() {
	db, err := openDB(env("DB_PATH", "app.db"))
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	dummy, err := bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcryptCost)
	if err != nil {
		log.Fatal(err)
	}

	app := &App{
		db:           db,
		limiter:      newLimiter(),
		dummyHash:    dummy,
		secureCookie: env("COOKIE_SECURE", "false") == "true",
		trustProxy:   env("TRUST_PROXY", "false") == "true",
	}
	app.tpl = template.Must(template.New("").
		Funcs(template.FuncMap{"date": fmtDate, "baht": fmtBaht, "mask": maskName, "av": makeAvatar, "initial": initial, "cond": condLabel}).
		ParseFS(tplFS, "templates/*.html"))

	app.bootstrapAdmin()
	app.seedAuctions()
	go app.cleanupLoop()

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	go func() {
		log.Printf("listening on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	// สาธารณะ (ดูประมูลได้โดยไม่ต้องล็อกอิน)
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /about", a.about)
	mux.HandleFunc("GET /auctions/{id}", a.auctionView)
	mux.HandleFunc("GET /shop/{id}", a.shopPage)
	mux.HandleFunc("GET /img/card/{id}", a.cardImage)
	mux.HandleFunc("GET /avatar/{id}", a.avatarImage)
	mux.HandleFunc("GET /register", a.registerForm)
	mux.HandleFunc("POST /register", a.registerPost)
	mux.HandleFunc("GET /login", a.loginForm)
	mux.HandleFunc("POST /login", a.loginPost)
	mux.HandleFunc("POST /logout", a.logout)

	// ต้องล็อกอิน (role: user หรือ admin)
	mux.HandleFunc("GET /me", a.auth(a.mePage))
	mux.HandleFunc("GET /profile", a.auth(a.profilePage))
	mux.HandleFunc("POST /profile/avatar", a.auth(a.avatarUpload))
	mux.HandleFunc("POST /profile/avatar/delete", a.auth(a.avatarDelete))
	mux.HandleFunc("GET /sell", a.auth(a.auctionNewForm))
	mux.HandleFunc("POST /auctions", a.auth(a.auctionCreate))
	mux.HandleFunc("GET /auctions/{id}/edit", a.auth(a.auctionEditForm))
	mux.HandleFunc("POST /auctions/{id}/edit", a.auth(a.auctionUpdate))
	mux.HandleFunc("POST /auctions/{id}/delete", a.auth(a.auctionDelete))
	mux.HandleFunc("POST /auctions/{id}/bid", a.auth(a.bidCreate))

	// เฉพาะ admin
	mux.HandleFunc("GET /admin", a.admin(a.adminPage))
	mux.HandleFunc("POST /admin/auctions/{id}/cancel", a.admin(a.adminCancelAuction))
	mux.HandleFunc("POST /admin/auctions/{id}/delete", a.admin(a.adminDeleteAuction))
	mux.HandleFunc("POST /admin/users/{id}/role", a.admin(a.adminSetRole))
	mux.HandleFunc("POST /admin/users/{id}/delete", a.admin(a.adminDeleteUser))

	mux.HandleFunc("/", a.notFound)

	return a.securityHeaders(a.sessionMW(a.csrfMW(mux)))
}

func (a *App) bootstrapAdmin() {
	u, p := os.Getenv("ADMIN_USERNAME"), os.Getenv("ADMIN_PASSWORD")
	if u == "" || p == "" {
		log.Println("ไม่ได้ตั้ง ADMIN_USERNAME / ADMIN_PASSWORD — ข้ามการสร้างแอดมินเริ่มต้น")
		return
	}
	if !usernameRe.MatchString(u) {
		log.Fatal("ADMIN_USERNAME ต้องเป็น a-z, 0-9 หรือ _ ยาว 3-30 ตัวอักษร")
	}
	if validPassword(p) != "" {
		log.Println("คำเตือน: รหัสผ่านแอดมินอ่อนแอ (ไม่ผ่านนโยบายรหัสผ่าน) ใช้ทดสอบในเครื่องได้ แต่ห้ามใช้บนเซิร์ฟเวอร์จริง")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p), bcryptCost)
	if err != nil {
		log.Fatal(err)
	}
	_, err = a.db.Exec(`INSERT OR IGNORE INTO users(username,password_hash,role,created_at)
		VALUES(?,?,'admin',?)`, u, string(hash), time.Now().Unix())
	if err != nil {
		log.Fatalf("create admin: %v", err)
	}
}

func (a *App) cleanupLoop() {
	t := time.NewTicker(time.Hour)
	for range t.C {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
		a.limiter.gc()
	}
}
