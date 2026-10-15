package main

import (
	"database/sql"
	"strconv"
	"time"

	_ "modernc.org/sqlite" // SQLite แบบ pure Go ไม่ต้องใช้ CGO
)

type User struct {
	ID        int64
	Username  string
	Role      string
	CreatedAt int64
	AvatarVer int64 // 0 = ยังไม่มีรูปโปรไฟล์ (ใช้ตัวอักษรแทน)
}

// Shop: ผู้ขายที่มีการ์ดกำลังประมูล แสดงเป็น "ร้านค้า" ที่ท้ายหน้าแรก
type Shop struct {
	ID        int64
	Name      string
	AvatarVer int64
	Live      int
}

// Auction: ข้อมูลที่มี "เจ้าของ" (seller) — ราคาเก็บเป็นจำนวนเต็ม (บาท) ไม่ใช้ทศนิยม
type Auction struct {
	ID              int64
	SellerID        int64
	Seller          string
	Title           string
	Game            string
	SetCode         string
	Rarity          string
	Condition       string
	Description     string
	Emoji           string
	StartPrice      int64
	MinIncrement    int64
	CurrentBid      int64
	BidCount        int
	HighestBidderID int64
	Leader          string
	EndsAt          int64
	CreatedAt       int64
	Status          string // active | cancelled
	ImgVer          int64  // 0 = ไม่มีรูป (ใช้การ์ด CSS แทน)
	SellerAvatarVer int64

	// คำนวณจากเวลาฝั่งเซิร์ฟเวอร์ (ไม่เชื่อเวลาจากเบราว์เซอร์)
	HasImage  bool
	Live      bool
	Ended     bool
	Cancelled bool
	Remaining int64
}

func (x Auction) Price() int64 {
	if x.BidCount == 0 {
		return x.StartPrice
	}
	return x.CurrentBid
}

func (x Auction) MinBid() int64 {
	if x.BidCount == 0 {
		return x.StartPrice
	}
	return x.CurrentBid + x.MinIncrement
}

type Bid struct {
	ID           int64
	AuctionID    int64
	AuctionTitle string
	UserID       int64
	Username     string
	Amount       int64
	CreatedAt    int64
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS users(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT 'user' CHECK(role IN ('user','admin')),
		failed_logins INTEGER NOT NULL DEFAULT 0,
		locked_until INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions(
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
		csrf TEXT NOT NULL,
		expires_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS auctions(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		seller_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		title TEXT NOT NULL,
		game TEXT NOT NULL,
		set_code TEXT NOT NULL,
		rarity TEXT NOT NULL,
		card_condition TEXT NOT NULL,
		description TEXT NOT NULL,
		emoji TEXT NOT NULL,
		start_price INTEGER NOT NULL CHECK(start_price >= 1),
		min_increment INTEGER NOT NULL CHECK(min_increment >= 1),
		current_bid INTEGER NOT NULL DEFAULT 0,
		bid_count INTEGER NOT NULL DEFAULT 0,
		highest_bidder_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
		ends_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','cancelled')))`,
	`CREATE TABLE IF NOT EXISTS bids(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		auction_id INTEGER NOT NULL REFERENCES auctions(id) ON DELETE CASCADE,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		amount INTEGER NOT NULL,
		created_at INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS idx_bids_auction ON bids(auction_id)`,
	`CREATE TABLE IF NOT EXISTS auction_images(
		auction_id INTEGER PRIMARY KEY REFERENCES auctions(id) ON DELETE CASCADE,
		mime TEXT NOT NULL,
		data BLOB NOT NULL,
		updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS avatars(
		user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
		mime TEXT NOT NULL,
		data BLOB NOT NULL,
		updated_at INTEGER NOT NULL)`,
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// ---------- Users ----------

func (a *App) userByID(id int64) (*User, error) {
	u := &User{}
	err := a.db.QueryRow(`SELECT u.id,u.username,u.role,u.created_at,
		COALESCE((SELECT v.updated_at FROM avatars v WHERE v.user_id=u.id),0)
		FROM users u WHERE u.id=?`, id).Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt, &u.AvatarVer)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (a *App) listUsers() ([]User, error) {
	rows, err := a.db.Query(`SELECT id,username,role,created_at FROM users ORDER BY id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ร้านค้าแนะนำ: ผู้ขายที่มีการ์ดกำลังประมูลมากที่สุด
func (a *App) listShops(limit int) ([]Shop, error) {
	rows, err := a.db.Query(`SELECT u.id,u.username,
		COALESCE((SELECT v.updated_at FROM avatars v WHERE v.user_id=u.id),0), COUNT(a.id)
		FROM users u JOIN auctions a ON a.seller_id=u.id AND a.status='active' AND a.ends_at>?
		GROUP BY u.id ORDER BY COUNT(a.id) DESC, u.id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Shop
	for rows.Next() {
		var sh Shop
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.AvatarVer, &sh.Live); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

func (a *App) count(query string, args ...any) int {
	var n int
	_ = a.db.QueryRow(query, args...).Scan(&n)
	return n
}

// ---------- Auctions ----------

const auctionCols = `a.id,a.seller_id,u.username,a.title,a.game,a.set_code,a.rarity,a.card_condition,a.description,a.emoji,
	a.start_price,a.min_increment,a.current_bid,a.bid_count,COALESCE(a.highest_bidder_id,0),COALESCE(h.username,''),
	a.ends_at,a.created_at,a.status,
	COALESCE((SELECT i.updated_at FROM auction_images i WHERE i.auction_id=a.id),0),
	COALESCE((SELECT v.updated_at FROM avatars v WHERE v.user_id=a.seller_id),0)`

const auctionFrom = ` FROM auctions a JOIN users u ON u.id=a.seller_id LEFT JOIN users h ON h.id=a.highest_bidder_id`

type scanner interface{ Scan(dest ...any) error }

func scanAuction(s scanner, now int64) (Auction, error) {
	var x Auction
	err := s.Scan(&x.ID, &x.SellerID, &x.Seller, &x.Title, &x.Game, &x.SetCode, &x.Rarity, &x.Condition,
		&x.Description, &x.Emoji, &x.StartPrice, &x.MinIncrement, &x.CurrentBid, &x.BidCount,
		&x.HighestBidderID, &x.Leader, &x.EndsAt, &x.CreatedAt, &x.Status, &x.ImgVer, &x.SellerAvatarVer)
	if err != nil {
		return x, err
	}
	x.HasImage = x.ImgVer > 0
	x.Cancelled = x.Status == "cancelled"
	x.Live = x.Status == "active" && x.EndsAt > now
	x.Ended = x.Status == "active" && x.EndsAt <= now
	if x.Live {
		x.Remaining = x.EndsAt - now
	}
	return x, nil
}

// where / order มาจากโค้ดของเราเองเท่านั้น (ค่าคงที่) ส่วนค่าจากผู้ใช้ส่งผ่าน args เป็น placeholder เสมอ
func (a *App) queryAuctions(where string, args []any, order string, limit int) ([]Auction, error) {
	now := time.Now().Unix()
	q := `SELECT ` + auctionCols + auctionFrom + ` WHERE ` + where + ` ORDER BY ` + order + ` LIMIT ` + strconv.Itoa(limit)
	rows, err := a.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Auction
	for rows.Next() {
		x, err := scanAuction(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (a *App) getAuction(id int64) (*Auction, error) {
	list, err := a.queryAuctions(`a.id=?`, []any{id}, `a.id`, 1)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, sql.ErrNoRows
	}
	return &list[0], nil
}

// ---------- Bids ----------

// auctionID = 0 หมายถึงทุกประมูล (ใช้ในหน้า admin)
func (a *App) listBids(auctionID int64, limit int) ([]Bid, error) {
	rows, err := a.db.Query(`SELECT b.id,b.auction_id,t.title,b.user_id,u.username,b.amount,b.created_at
		FROM bids b JOIN users u ON u.id=b.user_id JOIN auctions t ON t.id=b.auction_id
		WHERE (?=0 OR b.auction_id=?) ORDER BY b.id DESC LIMIT ?`, auctionID, auctionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bid
	for rows.Next() {
		var b Bid
		if err := rows.Scan(&b.ID, &b.AuctionID, &b.AuctionTitle, &b.UserID, &b.Username, &b.Amount, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
