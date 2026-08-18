package subscribers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingMailer, gönderilen e-postaları kaydeder.
type recordingMailer struct {
	targets []string
	bodies  []string
}

func (m *recordingMailer) Send(_ context.Context, target string, msg notifications.Message) error {
	m.targets = append(m.targets, target)
	m.bodies = append(m.bodies, msg.Body)

	return nil
}

type fixture struct {
	pool    *pgxpool.Pool
	queries *store.Queries
	mailer  *recordingMailer
	svc     *Service
	orgID   uuid.UUID
	pageID  uuid.UUID
	slug    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	pool := testdb.Pool(t, "subscribers")
	queries := store.New(pool)

	slug := "org-" + strings.ReplaceAll(uuid.NewString(), "-", "")

	org, err := queries.CreateOrganization(t.Context(), store.CreateOrganizationParams{
		Name: "Acme A.Ş.",
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("organizasyon oluşturulamadı: %v", err)
	}

	page, err := queries.CreateStatusPage(t.Context(), store.CreateStatusPageParams{OrgID: org.ID, Slug: slug})
	if err != nil {
		t.Fatalf("status page oluşturulamadı: %v", err)
	}

	// Abonelik yalnızca YAYIMLANMIŞ sayfada mümkün.
	if _, err := pool.Exec(t.Context(),
		`UPDATE status_pages SET is_published = true WHERE id = $1`, page.ID); err != nil {
		t.Fatalf("sayfa yayımlanamadı: %v", err)
	}

	mailer := &recordingMailer{}

	return &fixture{
		pool:    pool,
		queries: queries,
		mailer:  mailer,
		svc:     NewService(queries, mailer, "http://localhost:3100", discardLogger()),
		orgID:   org.ID,
		pageID:  page.ID,
		slug:    slug,
	}
}

// tokenFromLink, e-posta gövdesindeki linkten token'ı çıkarır (ziyaretçinin
// yaptığı şeyin aynısı).
func tokenFromLink(t *testing.T, body, marker string) string {
	t.Helper()

	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("%q linki gövdede yok: %s", marker, body)
	}

	rest := body[idx+len(marker):]
	if end := strings.IndexAny(rest, "\n "); end >= 0 {
		rest = rest[:end]
	}

	return rest
}

func (f *fixture) subscriberRow(t *testing.T, email string) (store.SubscriberStatus, bool) {
	t.Helper()

	var status store.SubscriberStatus
	err := f.pool.QueryRow(t.Context(),
		`SELECT status FROM status_page_subscribers WHERE status_page_id = $1 AND email = $2`,
		f.pageID, email).Scan(&status)
	if err != nil {
		return "", false
	}

	return status, true
}

// ÇİFT ONAY: abonelik `pending` doğuyor ve onay linki gönderiliyor.
func TestSubscribeStartsPendingAndSendsConfirmation(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "  Ziyaretci@Example.COM "); err != nil {
		t.Fatalf("abonelik başarısız: %v", err)
	}

	// Adres NORMALLEŞTİRİLMİŞ saklanıyor (1.1'in kuralı).
	status, ok := f.subscriberRow(t, "ziyaretci@example.com")
	if !ok {
		t.Fatal("abone satırı yazılmadı")
	}

	if status != store.SubscriberStatusPending {
		t.Errorf("durum = %q, beklenen pending", status)
	}

	if len(f.mailer.targets) != 1 || f.mailer.targets[0] != "ziyaretci@example.com" {
		t.Fatalf("gönderilen e-postalar = %v", f.mailer.targets)
	}

	// Onay e-postasında HEM onay HEM iptal linki var: onaya tıklamayan biri o
	// e-postayı hiç istememiş olabilir ve elinde vazgeçecek bir link olmalı.
	body := f.mailer.bodies[0]
	if !strings.Contains(body, "/confirm?token=") || !strings.Contains(body, "/unsubscribe?token=") {
		t.Errorf("onay e-postasında linkler eksik: %s", body)
	}
}

// 6.4'ÜN ASIL GÜVENLİK KARARI: uç e-posta ENUMERATION'ına kapalı.
// Yeni adres, bekleyen adres ve ONAYLI adres — üçü de aynı sonucu üretiyor.
func TestSubscribeIsEnumerationSafe(t *testing.T) {
	f := newFixture(t)

	// 1. kez: yeni abone.
	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("1. abonelik: %v", err)
	}

	// 2. kez: zaten kayıtlı (bekliyor) — hata YOK.
	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("2. abonelik: %v", err)
	}

	// Onayla, sonra 3. kez dene: onaylı abone — yine hata YOK.
	//
	// EN SON gönderilen link kullanılıyor: ikinci abonelik isteği YENİ bir
	// onay token'ı yazdı ve eskisi böylece geçersizleşti. Bu da bilinçli —
	// aksi hâlde bir adres için üretilmiş her token süresiz geçerli kalırdı.
	token := tokenFromLink(t, f.mailer.bodies[len(f.mailer.bodies)-1], "/confirm?token=")
	if err := f.svc.Confirm(t.Context(), token); err != nil {
		t.Fatalf("onay: %v", err)
	}

	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("3. abonelik: %v", err)
	}

	// ONAYLI ABONEYE YENİ ONAY E-POSTASI GİTMİYOR: gitseydi bir yabancı,
	// başkasının adresini yazarak ona istediği kadar e-posta yollatabilirdi.
	if len(f.mailer.targets) != 2 {
		t.Fatalf("gönderilen e-posta sayısı = %d, beklenen 2 (onaylıya gitmemeli)", len(f.mailer.targets))
	}

	// Onaylı abonelik `pending`e DÜŞMÜYOR.
	status, _ := f.subscriberRow(t, "ziyaretci@example.com")
	if status != store.SubscriberStatusConfirmed {
		t.Errorf("durum = %q, beklenen confirmed", status)
	}
}

// Bilinmeyen/yayımlanmamış sayfa da ayırt edilmiyor: servis ErrNotFound
// dönüyor ve handler onu BAŞARI gibi cevaplıyor (bkz. httpapi/subscribers.go).
func TestSubscribeUnknownPageIsNotFound(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), "hic-olmayan-sayfa", "a@example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}

	if len(f.mailer.targets) != 0 {
		t.Errorf("olmayan sayfa için e-posta gönderildi: %v", f.mailer.targets)
	}
}

func TestSubscribeRejectsInvalidEmail(t *testing.T) {
	f := newFixture(t)

	for _, email := range []string{"", "adres-degil", "ali@localhost", strings.Repeat("a", 250) + "@x.com"} {
		if err := f.svc.Subscribe(t.Context(), f.slug, email); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q için hata = %v, beklenen ErrInvalidEmail", email, err)
		}
	}
}

// ONAY TOKEN'I TEK KULLANIMLIK: aynı link ikinci kez çalışmıyor.
func TestConfirmTokenIsSingleUse(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("abonelik: %v", err)
	}

	token := tokenFromLink(t, f.mailer.bodies[0], "/confirm?token=")

	if err := f.svc.Confirm(t.Context(), token); err != nil {
		t.Fatalf("1. onay: %v", err)
	}

	if err := f.svc.Confirm(t.Context(), token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("2. onay hatası = %v, beklenen ErrNotFound", err)
	}
}

func TestConfirmRejectsUnknownToken(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Confirm(t.Context(), "bu-token-yok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hata = %v, beklenen ErrNotFound", err)
	}
}

// ABONELİKTEN ÇIKMA satırı SİLİYOR: "ayrıldı" bayrağı, ayrılmış bir adresi
// süresiz saklamak demekti.
func TestUnsubscribeDeletesSubscriber(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("abonelik: %v", err)
	}

	token := tokenFromLink(t, f.mailer.bodies[0], "/unsubscribe?token=")

	slug, err := f.svc.Unsubscribe(t.Context(), token)
	if err != nil {
		t.Fatalf("iptal: %v", err)
	}

	if slug != f.slug {
		t.Errorf("slug = %q, beklenen %q", slug, f.slug)
	}

	if _, ok := f.subscriberRow(t, "ziyaretci@example.com"); ok {
		t.Error("abone satırı silinmedi")
	}

	// İkinci iptal aynı cevabı alıyor (satır yok).
	if _, err := f.svc.Unsubscribe(t.Context(), token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("2. iptal hatası = %v, beklenen ErrNotFound", err)
	}
}

// Onay token'ı veritabanında HAM saklanmıyor (1.2'nin kararının devamı).
func TestConfirmTokenIsStoredHashed(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "ziyaretci@example.com"); err != nil {
		t.Fatalf("abonelik: %v", err)
	}

	token := tokenFromLink(t, f.mailer.bodies[0], "/confirm?token=")

	var stored *string
	if err := f.pool.QueryRow(t.Context(),
		`SELECT confirm_token_hash FROM status_page_subscribers WHERE email = $1`,
		"ziyaretci@example.com").Scan(&stored); err != nil {
		t.Fatalf("satır okunamadı: %v", err)
	}

	if stored == nil || *stored == token {
		t.Fatal("onay token'ı veritabanında ham saklanıyor")
	}

	if *stored != hashToken(token) {
		t.Error("saklanan değer token'ın hash'i değil")
	}
}

func TestCountConfirmed(t *testing.T) {
	f := newFixture(t)

	if err := f.svc.Subscribe(t.Context(), f.slug, "a@example.com"); err != nil {
		t.Fatalf("abonelik: %v", err)
	}

	total, err := f.svc.CountConfirmed(t.Context(), f.pageID)
	if err != nil {
		t.Fatalf("sayım: %v", err)
	}

	// Onaylanmamış abone SAYILMIYOR.
	if total != 0 {
		t.Fatalf("onaysız abone sayıldı: %d", total)
	}

	if err := f.svc.Confirm(t.Context(), tokenFromLink(t, f.mailer.bodies[0], "/confirm?token=")); err != nil {
		t.Fatalf("onay: %v", err)
	}

	total, err = f.svc.CountConfirmed(t.Context(), f.pageID)
	if err != nil {
		t.Fatalf("sayım: %v", err)
	}

	if total != 1 {
		t.Fatalf("onaylı abone sayısı = %d, beklenen 1", total)
	}
}
