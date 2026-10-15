package main

import (
	"bytes"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // ลงทะเบียน decoder เพื่อรองรับ GIF
	"image/jpeg"
	_ "image/png" // ลงทะเบียน decoder เพื่อรองรับ PNG
	"io"
	"net/http"
	"time"
)

const (
	maxUpload     = 2 << 20 // รูปที่อัปโหลดต้องไม่เกิน 2MB
	maxSourceSide = 3000    // กว้าง/ยาวไม่เกิน 3000px (กัน decompression bomb)
	minSourceSide = 50
	cardMaxW      = 800
	cardMaxH      = 1200
	avatarSide    = 256
)

var errBadImage = errors.New("รูปต้องเป็นไฟล์ JPG, PNG หรือ GIF ขนาดไม่เกิน 2MB และกว้าง/ยาวไม่เกิน 3000 พิกเซล")

// processImage รับไฟล์ที่ผู้ใช้อัปโหลด → ตรวจ → decode → ย่อ → encode เป็น JPEG ใหม่
// การ re-encode ทำให้ metadata (EXIF) และข้อมูลแอบแฝงในไฟล์ถูกทิ้งทั้งหมด เหลือแต่พิกเซลภาพ
func processImage(r io.Reader, avatar bool) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUpload+1))
	if err != nil || len(data) == 0 || len(data) > maxUpload {
		return nil, errBadImage
	}
	// อ่านขนาดจากหัวไฟล์ก่อน ยังไม่ decode จริง
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png" && format != "gif") {
		return nil, errBadImage
	}
	if cfg.Width < minSourceSide || cfg.Height < minSourceSide || cfg.Width > maxSourceSide || cfg.Height > maxSourceSide {
		return nil, errBadImage
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errBadImage
	}
	flat := flatten(img)
	var out *image.RGBA
	if avatar {
		sq := cropSquare(flat)
		out = sq
		if sq.Bounds().Dx() > avatarSide {
			out = boxScale(sq, avatarSide, avatarSide)
		}
	} else {
		out = fitCard(flat)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 85}); err != nil {
		return nil, errBadImage
	}
	return buf.Bytes(), nil
}

// วางภาพลงบนพื้นดำ (รองรับ PNG โปร่งใส) และทำให้ origin เป็น (0,0)
func flatten(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Over)
	return dst
}

func cropSquare(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	s := w
	if h < s {
		s = h
	}
	x0 := b.Min.X + (w-s)/2
	y0 := b.Min.Y + (h-s)/2
	return src.SubImage(image.Rect(x0, y0, x0+s, y0+s)).(*image.RGBA)
}

func fitCard(src *image.RGBA) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= cardMaxW && h <= cardMaxH {
		return src
	}
	nw, nh := w, h
	if nw > cardMaxW {
		nh = nh * cardMaxW / nw
		nw = cardMaxW
	}
	if nh > cardMaxH {
		nw = nw * cardMaxH / nh
		nh = cardMaxH
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	return boxScale(src, nw, nh)
}

// ย่อภาพแบบเฉลี่ยสีของพิกเซลต้นทางในแต่ละช่อง (คุณภาพดีพอสำหรับการย่อ)
func boxScale(src *image.RGBA, nw, nh int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					i := src.PixOffset(b.Min.X+sx, b.Min.Y+sy)
					r += uint32(src.Pix[i])
					g += uint32(src.Pix[i+1])
					bl += uint32(src.Pix[i+2])
					n++
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8(r/n), uint8(g/n), uint8(bl/n), 255
		}
	}
	return dst
}

// อ่านไฟล์จากฟอร์ม: present=false แปลว่าไม่ได้เลือกไฟล์ (ไม่ใช่ error)
func readUpload(r *http.Request, field string, avatar bool) (data []byte, present bool, err error) {
	file, _, err := r.FormFile(field)
	if err != nil {
		if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) {
			return nil, false, nil
		}
		return nil, true, errBadImage
	}
	defer file.Close()
	data, err = processImage(file, avatar)
	if err != nil {
		return nil, true, err
	}
	return data, true, nil
}

// ---------- Serving ----------

// เสิร์ฟรูปจากฐานข้อมูล ด้วย id เท่านั้น (ไม่มีชื่อไฟล์/พาธจากผู้ใช้ → ไม่มี path traversal)
func serveImage(w http.ResponseWriter, r *http.Request, data []byte, mime string, ts int64) {
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, "", time.Unix(ts, 0), bytes.NewReader(data))
}

func (a *App) cardImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var data []byte
	var mime string
	var ts int64
	err := a.db.QueryRow(`SELECT data,mime,updated_at FROM auction_images WHERE auction_id=?`, id).Scan(&data, &mime, &ts)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	serveImage(w, r, data, mime, ts)
}

func (a *App) avatarImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	var data []byte
	var mime string
	var ts int64
	err := a.db.QueryRow(`SELECT data,mime,updated_at FROM avatars WHERE user_id=?`, id).Scan(&data, &mime, &ts)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	serveImage(w, r, data, mime, ts)
}

// ---------- Profile (เปลี่ยนรูปโปรไฟล์) ----------

type ProfileData struct {
	NListings int
	NBids     int
}

func (a *App) profilePage(w http.ResponseWriter, r *http.Request) {
	a.renderProfile(w, r, http.StatusOK, nil)
}

func (a *App) renderProfile(w http.ResponseWriter, r *http.Request, status int, errs []string) {
	u := sess(r).User
	p := a.page(r, "โปรไฟล์ของฉัน")
	p.Errors = errs
	p.Data = ProfileData{
		NListings: a.count(`SELECT COUNT(*) FROM auctions WHERE seller_id=?`, u.ID),
		NBids:     a.count(`SELECT COUNT(*) FROM bids WHERE user_id=?`, u.ID),
	}
	a.render(w, status, "profile", p)
}

func (a *App) avatarUpload(w http.ResponseWriter, r *http.Request) {
	u := sess(r).User
	if !a.limiter.allow("avatar|"+idStr(u.ID), 10, time.Hour) {
		a.errorPage(w, r, http.StatusTooManyRequests, tooMany)
		return
	}
	data, present, err := readUpload(r, "avatar", true)
	if err != nil {
		a.renderProfile(w, r, http.StatusUnprocessableEntity, []string{err.Error()})
		return
	}
	if !present {
		a.renderProfile(w, r, http.StatusUnprocessableEntity, []string{"กรุณาเลือกรูปก่อนกดบันทึก"})
		return
	}
	// user_id มาจาก session เสมอ → เปลี่ยนได้แค่รูปของตัวเอง
	_, err = a.db.Exec(`INSERT INTO avatars(user_id,mime,data,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET mime=excluded.mime, data=excluded.data, updated_at=excluded.updated_at`,
		u.ID, "image/jpeg", data, time.Now().Unix())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/profile?ok=avatar", http.StatusSeeOther)
}

func (a *App) avatarDelete(w http.ResponseWriter, r *http.Request) {
	if _, err := a.db.Exec(`DELETE FROM avatars WHERE user_id=?`, sess(r).User.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/profile?ok=avatardel", http.StatusSeeOther)
}

// ---------- Shop (ร้านค้า = หน้าสินค้าของผู้ขายแต่ละคน) ----------

type ShopData struct {
	Shop     User
	Auctions []Auction
}

func (a *App) shopPage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	u, err := a.userByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	list, err := a.queryAuctions(`a.seller_id=? AND a.status='active' AND a.ends_at>?`,
		[]any{id, time.Now().Unix()}, `a.ends_at ASC`, 60)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	p := a.page(r, "ร้าน "+u.Username)
	p.Data = ShopData{Shop: *u, Auctions: list}
	a.render(w, http.StatusOK, "shop", p)
}
