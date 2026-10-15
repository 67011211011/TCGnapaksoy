package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ค่าที่อนุญาตให้เลือกได้ (whitelist) — ไม่เชื่อค่าอิสระจากฟอร์ม
var (
	games      = []string{"Pokémon TCG", "One Piece Card Game", "Disney Lorcana", "Digimon Card Game", "อื่นๆ"}
	rarities   = []string{"C", "U", "R", "RR", "RRR", "SR", "AR", "SAR", "UR", "FUR", "PROMO"}
	conditions = []string{"NM", "LP", "MP", "HP"}
	emojis     = []string{"🐉", "🔥", "💧", "⚡", "🌿", "🌙", "👑", "🦊", "🐋", "💎"}
	durations  = []Duration{
		{5, "5 นาที (ไว้ทดสอบ)"}, {60, "1 ชั่วโมง"}, {1440, "1 วัน"}, {4320, "3 วัน"}, {10080, "7 วัน"},
	}
)

const (
	maxBid        = 10000000
	snipeWindow   = 300 // วินาที: ประมูลในช่วง 5 นาทีสุดท้าย → ขยายเวลาเป็น 5 นาทีนับจากนั้น
	maxStartPrice = 1000000
	maxIncrement  = 100000
)

type Duration struct {
	Minutes int64
	Label   string
}

type HomeData struct {
	Auctions []Auction
	Featured []Auction // การ์ดที่มีรูป 3 ใบแรก ใช้โชว์ใน hero
	Shops    []Shop
	Q        string
}

type AuctionView struct {
	Auction  Auction
	Bids     []Bid
	IsSeller bool
	CanEdit  bool
	Leading  bool
	MinBid   int64
}

type AuctionFormData struct {
	Heading      string
	Action       string
	CancelURL    string
	ShowDuration bool
	CurrentImage string // URL รูปปัจจุบัน (หน้าแก้ไข) ว่าง = ยังไม่มีรูป
	Games        []string
	Rarities     []string
	Conditions   []string
	Emojis       []string
	Durations    []Duration
}

type MeData struct {
	Selling []Auction
	Bidding []Auction
}

type AdminData struct {
	Users                            []User
	Auctions                         []Auction
	Bids                             []Bid
	NUsers, NAuctions, NLive, NBids int
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ---------- Public pages ----------

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if rq := []rune(q); len(rq) > 50 {
		q = string(rq[:50])
	}
	where := `a.status='active' AND a.ends_at>?`
	args := []any{time.Now().Unix()}
	if q != "" {
		where += ` AND a.title LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscaper.Replace(q)+"%")
	}
	list, err := a.queryAuctions(where, args, `a.ends_at ASC`, 60)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	var featured []Auction
	for _, x := range list {
		if x.HasImage && len(featured) < 3 {
			featured = append(featured, x)
		}
	}
	shops, err := a.listShops(6)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	p := a.page(r, "หน้าแรก")
	p.Data = HomeData{Auctions: list, Featured: featured, Shops: shops, Q: q}
	a.render(w, http.StatusOK, "home", p)
}

func (a *App) about(w http.ResponseWriter, r *http.Request) {
	a.render(w, http.StatusOK, "about", a.page(r, "เกี่ยวกับระบบและวิธีใช้"))
}

func (a *App) auctionView(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	a.renderAuction(w, r, id, http.StatusOK, nil, nil)
}

func (a *App) renderAuction(w http.ResponseWriter, r *http.Request, id int64, status int, errs []string, form map[string]string) {
	x, err := a.getAuction(id)
	if errors.Is(err, sql.ErrNoRows) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	bids, err := a.listBids(id, 50)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	v := AuctionView{Auction: *x, Bids: bids, MinBid: x.MinBid()}
	if u := sess(r).User; u != nil {
		v.IsSeller = u.ID == x.SellerID
		v.Leading = x.BidCount > 0 && x.HighestBidderID == u.ID
		v.CanEdit = v.IsSeller && x.Live && x.BidCount == 0
	}
	p := a.page(r, x.Title)
	p.Errors = errs
	if form != nil {
		p.Form = form
	}
	p.Data = v
	a.render(w, status, "auction", p)
}

// ---------- Auction form (CRUD ที่มีเจ้าของ) ----------

type auctionInput struct {
	Title, Game, SetCode, Rarity, Condition, Description, Emoji string
	StartPrice, MinIncrement, Minutes                         int64
}

func parseIntField(s string, min, max int64) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

func parseAuctionForm(r *http.Request, withDuration bool) (auctionInput, map[string]string, []string) {
	f := map[string]string{
		"title":         clean(r.PostFormValue("title")),
		"set_code":      clean(r.PostFormValue("set_code")),
		"description":   clean(r.PostFormValue("description")),
		"game":          r.PostFormValue("game"),
		"rarity":        r.PostFormValue("rarity"),
		"condition":     r.PostFormValue("condition"),
		"emoji":         r.PostFormValue("emoji"),
		"start_price":   strings.TrimSpace(r.PostFormValue("start_price")),
		"min_increment": strings.TrimSpace(r.PostFormValue("min_increment")),
		"duration":      strings.TrimSpace(r.PostFormValue("duration")),
	}
	in := auctionInput{
		Title: f["title"], SetCode: f["set_code"], Description: f["description"],
		Game: f["game"], Rarity: f["rarity"], Condition: f["condition"], Emoji: f["emoji"],
	}
	errs := collect(
		checkLen("ชื่อการ์ด", in.Title, 1, 80),
		checkLen("รหัสการ์ด", in.SetCode, 0, 30),
		checkLen("รายละเอียด", in.Description, 0, 800),
	)
	if !contains(games, in.Game) {
		errs = append(errs, "หมวดหมู่ไม่ถูกต้อง")
	}
	if !contains(rarities, in.Rarity) {
		errs = append(errs, "ระดับความหายากไม่ถูกต้อง")
	}
	if !contains(conditions, in.Condition) {
		errs = append(errs, "สภาพการ์ดไม่ถูกต้อง")
	}
	if !contains(emojis, in.Emoji) {
		errs = append(errs, "ไอคอนการ์ดไม่ถูกต้อง")
	}
	if n, ok := parseIntField(f["start_price"], 1, maxStartPrice); ok {
		in.StartPrice = n
	} else {
		errs = append(errs, "ราคาเปิดต้องเป็นจำนวนเต็ม 1 - 1,000,000 บาท")
	}
	if n, ok := parseIntField(f["min_increment"], 1, maxIncrement); ok {
		in.MinIncrement = n
	} else {
		errs = append(errs, "ขั้นต่ำการประมูลต้องเป็นจำนวนเต็ม 1 - 100,000 บาท")
	}
	if withDuration {
		n, ok := parseIntField(f["duration"], 1, 100000)
		valid := false
		if ok {
			for _, d := range durations {
				if d.Minutes == n {
					valid = true
				}
			}
		}
		if !valid {
			errs = append(errs, "ระยะเวลาประมูลไม่ถูกต้อง")
		} else {
			in.Minutes = n
		}
	}
	return in, f, errs
}

func (a *App) renderAuctionForm(w http.ResponseWriter, r *http.Request, status int, heading, action, cancelURL string,
	showDuration bool, currentImage string, errs []string, form map[string]string) {
	p := a.page(r, heading)
	p.Errors = errs
	p.Form = form
	p.Data = AuctionFormData{
		Heading: heading, Action: action, CancelURL: cancelURL, ShowDuration: showDuration, CurrentImage: currentImage,
		Games: games, Rarities: rarities, Conditions: conditions, Emojis: emojis, Durations: durations,
	}
	a.render(w, status, "auction_form", p)
}

func (a *App) auctionNewForm(w http.ResponseWriter, r *http.Request) {
	a.renderAuctionForm(w, r, http.StatusOK, "ลงขายการ์ดใหม่", "/auctions", "/", true, "", nil, map[string]string{
		"game": games[0], "rarity": "RR", "condition": "NM", "emoji": "🐉",
		"start_price": "100", "min_increment": "10", "duration": "1440",
	})
}

func (a *App) auctionCreate(w http.ResponseWriter, r *http.Request) {
	u := sess(r).User
	if !a.limiter.allow("newauc|"+idStr(u.ID), 10, time.Hour) {
		a.errorPage(w, r, http.StatusTooManyRequests, tooMany)
		return
	}
	in, form, errs := parseAuctionForm(r, true)
	img, hasImg, imgErr := readUpload(r, "image", false)
	if imgErr != nil {
		errs = append(errs, imgErr.Error())
	}
	if len(errs) > 0 {
		a.renderAuctionForm(w, r, http.StatusUnprocessableEntity, "ลงขายการ์ดใหม่", "/auctions", "/", true, "", errs, form)
		return
	}
	now := time.Now().Unix()
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer tx.Rollback() // ไม่มีผลหลัง Commit
	// seller_id มาจาก session เสมอ ไม่รับจากฟอร์ม
	res, err := tx.Exec(`INSERT INTO auctions(seller_id,title,game,set_code,rarity,card_condition,description,emoji,
		start_price,min_increment,current_bid,bid_count,ends_at,created_at,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,0,0,?,?,'active')`,
		u.ID, in.Title, in.Game, in.SetCode, in.Rarity, in.Condition, in.Description, in.Emoji,
		in.StartPrice, in.MinIncrement, now+in.Minutes*60, now)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	id, _ := res.LastInsertId()
	if hasImg {
		if _, err := tx.Exec(`INSERT INTO auction_images(auction_id,mime,data,updated_at) VALUES(?,?,?,?)`,
			id, "image/jpeg", img, now); err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/auctions/"+idStr(id)+"?ok=created", http.StatusSeeOther)
}

// โหลดประมูลที่เป็นของผู้ใช้คนนี้เท่านั้น (ไม่ใช่เจ้าของ → 404 ไม่บอกว่ามีอยู่จริง)
func (a *App) ownedAuction(w http.ResponseWriter, r *http.Request) (*Auction, bool) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return nil, false
	}
	x, err := a.getAuction(id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && x.SellerID != sess(r).User.ID) {
		a.notFound(w, r)
		return nil, false
	}
	if err != nil {
		a.serverError(w, r, err)
		return nil, false
	}
	return x, true
}

func (a *App) auctionEditForm(w http.ResponseWriter, r *http.Request) {
	x, ok := a.ownedAuction(w, r)
	if !ok {
		return
	}
	if !x.Live || x.BidCount > 0 {
		a.errorPage(w, r, http.StatusConflict, "แก้ไขไม่ได้ เพราะมีผู้ประมูลแล้วหรือปิดประมูลแล้ว")
		return
	}
	a.renderAuctionForm(w, r, http.StatusOK, "แก้ไขการ์ด", "/auctions/"+idStr(x.ID)+"/edit", "/auctions/"+idStr(x.ID), false,
		currentImageURL(x), nil,
		map[string]string{
			"title": x.Title, "set_code": x.SetCode, "description": x.Description, "game": x.Game,
			"rarity": x.Rarity, "condition": x.Condition, "emoji": x.Emoji,
			"start_price": idStr(x.StartPrice), "min_increment": idStr(x.MinIncrement),
		})
}

func currentImageURL(x *Auction) string {
	if !x.HasImage {
		return ""
	}
	return "/img/card/" + idStr(x.ID) + "?v=" + idStr(x.ImgVer)
}

func (a *App) auctionUpdate(w http.ResponseWriter, r *http.Request) {
	x, ok := a.ownedAuction(w, r)
	if !ok {
		return
	}
	in, form, errs := parseAuctionForm(r, false)
	img, hasImg, imgErr := readUpload(r, "image", false)
	if imgErr != nil {
		errs = append(errs, imgErr.Error())
	}
	if len(errs) > 0 {
		a.renderAuctionForm(w, r, http.StatusUnprocessableEntity, "แก้ไขการ์ด", "/auctions/"+idStr(x.ID)+"/edit",
			"/auctions/"+idStr(x.ID), false, currentImageURL(x), errs, form)
		return
	}
	now := time.Now().Unix()
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer tx.Rollback()
	// เงื่อนไขใน SQL: เป็นเจ้าของ + ยังไม่มีผู้ประมูล + ยังเปิดอยู่ (กัน race กับคนที่กำลังประมูล)
	res, err := tx.Exec(`UPDATE auctions SET title=?,game=?,set_code=?,rarity=?,card_condition=?,description=?,emoji=?,
		start_price=?,min_increment=?
		WHERE id=? AND seller_id=? AND bid_count=0 AND status='active' AND ends_at>?`,
		in.Title, in.Game, in.SetCode, in.Rarity, in.Condition, in.Description, in.Emoji,
		in.StartPrice, in.MinIncrement, x.ID, sess(r).User.ID, now)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.errorPage(w, r, http.StatusConflict, "แก้ไขไม่ได้ เพราะมีผู้ประมูลแล้วหรือปิดประมูลแล้ว")
		return
	}
	if hasImg { // เปลี่ยนรูปได้เฉพาะตอนที่แก้ไขได้ (ยังไม่มีผู้ประมูล) กันสลับรูปหลอกคนที่ลงเงินไปแล้ว
		if _, err := tx.Exec(`INSERT INTO auction_images(auction_id,mime,data,updated_at) VALUES(?,?,?,?)
			ON CONFLICT(auction_id) DO UPDATE SET mime=excluded.mime, data=excluded.data, updated_at=excluded.updated_at`,
			x.ID, "image/jpeg", img, now); err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/auctions/"+idStr(x.ID)+"?ok=updated", http.StatusSeeOther)
}

func (a *App) auctionDelete(w http.ResponseWriter, r *http.Request) {
	x, ok := a.ownedAuction(w, r)
	if !ok {
		return
	}
	res, err := a.db.Exec(`DELETE FROM auctions WHERE id=? AND seller_id=? AND bid_count=0`, x.ID, sess(r).User.ID)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.errorPage(w, r, http.StatusConflict, "ลบไม่ได้ เพราะมีผู้ประมูลแล้ว")
		return
	}
	http.Redirect(w, r, "/me?ok=deleted", http.StatusSeeOther)
}

// ---------- Bidding (ฟอร์มที่ 2) ----------

func bidFailReason(x *Auction, uid int64) string {
	switch {
	case x.Cancelled:
		return "การประมูลนี้ถูกยกเลิกแล้ว"
	case !x.Live:
		return "การประมูลนี้ปิดแล้ว"
	case x.SellerID == uid:
		return "ผู้ขายไม่สามารถประมูลการ์ดของตัวเองได้"
	case x.BidCount > 0 && x.HighestBidderID == uid:
		return "คุณเป็นผู้ประมูลสูงสุดอยู่แล้ว"
	default:
		return "ราคาต้องไม่ต่ำกว่า " + fmtBaht(x.MinBid()) + " (อาจมีคนประมูลตัดหน้า กรุณาลองใหม่)"
	}
}

func (a *App) bidCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	u := sess(r).User
	if !a.limiter.allow("bid|"+idStr(u.ID), 30, time.Minute) {
		a.errorPage(w, r, http.StatusTooManyRequests, tooMany)
		return
	}
	rawAmount := r.PostFormValue("amount")
	amount, valid := parseIntField(rawAmount, 1, maxBid)
	if !valid {
		a.renderAuction(w, r, id, http.StatusUnprocessableEntity,
			[]string{"กรุณาใส่ราคาเป็นจำนวนเต็มบาท (1 - 10,000,000)"}, map[string]string{"amount": strings.TrimSpace(rawAmount)})
		return
	}

	now := time.Now().Unix()
	tx, err := a.db.Begin()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	defer tx.Rollback() // ไม่มีผลหลัง Commit

	// การตรวจกติกาทั้งหมดอยู่ใน SQL เดียว (atomic) → ต่อให้กดพร้อมกันหลายคนก็ไม่เกิดราคาซ้อนหรือประมูลหลังปิด
	res, err := tx.Exec(`UPDATE auctions SET current_bid=?, highest_bidder_id=?, bid_count=bid_count+1,
		ends_at = CASE WHEN ends_at < ? THEN ? ELSE ends_at END
		WHERE id=? AND status='active' AND ends_at>? AND seller_id<>?
		AND COALESCE(highest_bidder_id,0)<>?
		AND ? >= CASE WHEN bid_count=0 THEN start_price ELSE current_bid+min_increment END`,
		amount, u.ID, now+snipeWindow, now+snipeWindow, id, now, u.ID, u.ID, amount)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		x, err := a.getAuction(id)
		if errors.Is(err, sql.ErrNoRows) {
			a.notFound(w, r)
			return
		}
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		a.renderAuction(w, r, id, http.StatusConflict, []string{bidFailReason(x, u.ID)}, map[string]string{"amount": idStr(amount)})
		return
	}
	if _, err := tx.Exec(`INSERT INTO bids(auction_id,user_id,amount,created_at) VALUES(?,?,?,?)`, id, u.ID, amount, now); err != nil {
		a.serverError(w, r, err)
		return
	}
	if err := tx.Commit(); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/auctions/"+idStr(id)+"?ok=bid", http.StatusSeeOther)
}

// ---------- My page ----------

func (a *App) mePage(w http.ResponseWriter, r *http.Request) {
	u := sess(r).User
	selling, err := a.queryAuctions(`a.seller_id=?`, []any{u.ID}, `a.id DESC`, 100)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	bidding, err := a.queryAuctions(`a.id IN (SELECT auction_id FROM bids WHERE user_id=?)`, []any{u.ID}, `a.ends_at DESC`, 100)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	p := a.page(r, "ประมูลของฉัน")
	p.Data = MeData{Selling: selling, Bidding: bidding}
	a.render(w, http.StatusOK, "me", p)
}

// ---------- Admin ----------

func (a *App) adminPage(w http.ResponseWriter, r *http.Request) {
	users, err := a.listUsers()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	auctions, err := a.queryAuctions(`1=1`, nil, `a.id DESC`, 100)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	bids, err := a.listBids(0, 50)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	p := a.page(r, "ห้องควบคุมผู้ดูแลระบบ")
	p.Data = AdminData{
		Users: users, Auctions: auctions, Bids: bids,
		NUsers:    len(users),
		NAuctions: a.count(`SELECT COUNT(*) FROM auctions`),
		NLive:     a.count(`SELECT COUNT(*) FROM auctions WHERE status='active' AND ends_at>?`, time.Now().Unix()),
		NBids:     a.count(`SELECT COUNT(*) FROM bids`),
	}
	a.render(w, http.StatusOK, "admin", p)
}

func (a *App) adminCancelAuction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	res, err := a.db.Exec(`UPDATE auctions SET status='cancelled' WHERE id=?`, id)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.notFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin?ok=cancelled", http.StatusSeeOther)
}

func (a *App) adminDeleteAuction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	if _, err := a.db.Exec(`DELETE FROM auctions WHERE id=?`, id); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin?ok=deleted", http.StatusSeeOther)
}

func (a *App) adminSetRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	role := r.PostFormValue("role")
	if !ok || (role != "user" && role != "admin") {
		a.errorPage(w, r, http.StatusBadRequest, "ข้อมูลไม่ถูกต้อง")
		return
	}
	if id == sess(r).User.ID {
		a.errorPage(w, r, http.StatusBadRequest, "ไม่สามารถเปลี่ยนบทบาทของตัวเองได้")
		return
	}
	res, err := a.db.Exec(`UPDATE users SET role=? WHERE id=?`, role, id)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.notFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin?ok=role", http.StatusSeeOther)
}

func (a *App) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	if id == sess(r).User.ID {
		a.errorPage(w, r, http.StatusBadRequest, "ไม่สามารถลบบัญชีของตัวเองได้")
		return
	}
	res, err := a.db.Exec(`DELETE FROM users WHERE id=?`, id)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		a.notFound(w, r)
		return
	}
	http.Redirect(w, r, "/admin?ok=userdeleted", http.StatusSeeOther)
}
