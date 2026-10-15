package main

import (
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// สร้างร้านค้าตัวอย่าง + การ์ดตัวอย่าง (ใช้รูปที่อยู่ใน static/samples) เมื่อยังไม่มีรายการประมูลเลย
// บัญชีร้านตัวอย่างใช้รหัสผ่านสุ่มที่ไม่มีใครรู้ จึงล็อกอินเข้าไม่ได้ (ไม่มีรหัสผ่านเริ่มต้นให้คนเดา)
func (a *App) seedAuctions() {
	if a.count(`SELECT COUNT(*) FROM auctions`) > 0 {
		return
	}
	shops := map[string]int64{}
	for _, name := range []string{"napaksoy_shop", "mewcorner", "goldcard_th"} {
		hash, err := bcrypt.GenerateFromPassword([]byte(randToken(24)), bcryptCost)
		if err != nil {
			log.Printf("seed shop: %v", err)
			return
		}
		_, _ = a.db.Exec(`INSERT OR IGNORE INTO users(username,password_hash,role,created_at) VALUES(?,?,'user',?)`,
			name, string(hash), time.Now().Unix())
		var id int64
		if err := a.db.QueryRow(`SELECT id FROM users WHERE username=?`, name).Scan(&id); err != nil {
			log.Printf("seed shop: %v", err)
			return
		}
		shops[name] = id
	}

	type seed struct {
		shop, title, code, rarity, desc, emoji, file string
		start, inc, hours                           int64
	}
	seeds := []seed{
		{"napaksoy_shop", "พิคาชู", "PROMO 144/S-P", "PROMO",
			"HP 70 ธาตุสายฟ้า · ท่า ไวลด์โวลต์ 90 · การ์ดโปรโมชัน ภาพอาร์ตพิเศษรวมโปเกมอนหลายตัวรอบพิคาชู",
			"⚡", "pikachu-promo.jpg", 1500, 100, 30},
		{"mewcorner", "มิว ex", "MA6 166/130", "FUR",
			"HP 160 · ฟูลอาร์ตลายรุ้ง ความสามารถ ขดเกลียวแห่งความทรงจำ · เทเลพอร์ทเบรก 30",
			"💎", "mew-ex-fur.jpg", 2500, 200, 52},
		{"mewcorner", "มิว ex", "MA6 068/130", "RR",
			"HP 160 · ความสามารถ ขดเกลียวแห่งความทรงจำ · เทเลพอร์ทเบรก 30 · ลายมาตรฐาน",
			"🌙", "mew-ex-rr.jpg", 300, 20, 76},
		{"goldcard_th", "เมก้าลูคาริโอ ex", "077/126", "RR",
			"HP 340 · คลื่นพลังพุ่งชน 130 · เมก้าเบรฟ 270",
			"🔥", "mega-lucario-ex.jpg", 400, 50, 100},
		{"napaksoy_shop", "อาร์เซอุส VSTAR", "S9 084/100", "RRR",
			"HP 280 · ทรินิตี้โนวา 200 · พลัง VSTAR สตาร์เบิร์ท ใช้ได้ 1 ครั้งต่อเกม",
			"👑", "arceus-vstar.jpg", 200, 10, 120},
	}

	now := time.Now().Unix()
	for _, s := range seeds {
		img, err := staticFS.ReadFile("static/samples/" + s.file)
		if err != nil {
			log.Printf("seed image %s: %v", s.file, err)
			continue
		}
		res, err := a.db.Exec(`INSERT INTO auctions(seller_id,title,game,set_code,rarity,card_condition,description,emoji,
			start_price,min_increment,current_bid,bid_count,ends_at,created_at,status)
			VALUES(?,?,?,?,?,?,?,?,?,?,0,0,?,?,'active')`,
			shops[s.shop], s.title, games[0], s.code, s.rarity, "NM", s.desc, s.emoji,
			s.start, s.inc, now+s.hours*3600, now)
		if err != nil {
			log.Printf("seed auction: %v", err)
			continue
		}
		id, _ := res.LastInsertId()
		if _, err := a.db.Exec(`INSERT INTO auction_images(auction_id,mime,data,updated_at) VALUES(?,?,?,?)`,
			id, "image/jpeg", img, now); err != nil {
			log.Printf("seed image: %v", err)
		}
	}
	log.Println("เพิ่มร้านค้าและการ์ดตัวอย่างแล้ว")
}
