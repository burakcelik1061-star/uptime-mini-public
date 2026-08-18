package seed

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/token"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/testdb"
)

// Seed'in testi olması bir "iyi olur" değil: bu paket ŞEMAYA ham SQL ile
// yazıyor (gerekçesi seed.go'da), yani bir migration sütun adını
// değiştirdiğinde derleyici sessiz kalır ve hatayı ancak demoyu çalıştıran
// kişi görürdü. Test, seed'i CI'ın kapsamına sokuyor.

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testdb.Pool(t, "seed")
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("sayım başarısız (%s): %v", query, err)
	}

	return n
}

func TestSeedProducesAWorkingDemo(t *testing.T) {
	pool := newPool(t)

	summary, err := Run(t.Context(), pool, Options{})
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	// 8.7'DEN BERİ İKİ ORGANİZASYON: vitrin (Acme, PRO) ve kotasına dayanmış
	// ücretsiz org (Startup). Toplam monitor sayısı ikisinin toplamı.
	if want := 5 + len(freeMonitors); summary.Monitors != want {
		t.Errorf("monitor sayısı = %d, beklenen %d", summary.Monitors, want)
	}
	if summary.FreeOrgID == uuid.Nil {
		t.Error("ücretsiz demo organizasyonu üretilmemiş")
	}
	if summary.Incidents != 4 {
		t.Errorf("kesinti sayısı = %d, beklenen 4", summary.Incidents)
	}
	if summary.DailyRollups == 0 || summary.RawChecks == 0 {
		t.Errorf("geçmiş üretilmemiş: %d özet, %d ham satır",
			summary.DailyRollups, summary.RawChecks)
	}

	// EKRANLARIN BOŞ GÖRÜNMEMESİ ŞARTI (7.4'ün gerekçesi) satır sayılarıyla
	// ölçülüyor.
	if n := count(t, pool, `SELECT count(*) FROM status_pages WHERE is_published`); n != 1 {
		t.Errorf("yayımlanmış status page sayısı = %d, beklenen 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM status_page_items`); n != 4 {
		t.Errorf("yayımlanan servis sayısı = %d, beklenen 4 (biri bilinçli olarak private)", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM status_page_groups`); n != 2 {
		t.Errorf("grup sayısı = %d, beklenen 2", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM incident_updates`); n < 8 {
		t.Errorf("timeline satırı = %d, daha fazlası bekleniyordu", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM maintenance_windows`); n != 1 {
		t.Errorf("bakım penceresi = %d, beklenen 1", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM notification_channels`); n != 1 {
		t.Errorf("bildirim kanalı = %d, beklenen 1", n)
	}

	// AÇIK bir kesinti olmalı: demo "her şey yolunda" hâlinden ibaret
	// kalmasın.
	if n := count(t, pool, `SELECT count(*) FROM incidents WHERE resolved_at IS NULL`); n != 1 {
		t.Errorf("açık kesinti sayısı = %d, beklenen 1", n)
	}

	// 5.2'nin grafiği 90 gün çiziyor; "veri yok" günleri de bilinçli.
	days := count(t, pool, `
		SELECT count(DISTINCT bucket_date) FROM uptime_rollup_daily`)
	if days < 80 || days > historyDays {
		t.Errorf("özet günü sayısı = %d, 80..%d arası beklenirdi", days, historyDays)
	}
}

// SIZINTI KURALI DEMO VERİDE DE SINANIYOR (5.1): yayımlanmamış servisin
// kesintisi var ve BAŞLIĞINDA adı geçiyor — public sorgular onu dışarıda
// bırakmalı. Burada ölçülen şey verinin bu sınavı KURMUŞ olması; sorgunun
// kendisi internal/statuspage'de test ediliyor.
func TestSeedIncludesAnUnpublishedServiceWithItsOwnIncident(t *testing.T) {
	pool := newPool(t)

	if _, err := Run(t.Context(), pool, Options{}); err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	// SAYIM VİTRİN ORGANİZASYONUNA KAPSANDI (8.7): ücretsiz demo org'unda da
	// yayımlanmamış monitor'ler var ve onlar bu testin konusu değil. Kapsamı
	// daraltmak testi ZAYIFLATMIYOR — sızıntı kuralı zaten org bazında
	// çalışıyor ve asıl iddia ("bu org'un private servisi public sayfasında
	// görünmüyor") aynen ölçülüyor.
	private := count(t, pool, `
		SELECT count(*) FROM monitors m
		JOIN organizations o ON o.id = m.org_id
		WHERE o.slug = 'acme'
		  AND NOT EXISTS (SELECT 1 FROM status_page_items i WHERE i.monitor_id = m.id)`)
	if private != 1 {
		t.Fatalf("yayımlanmamış monitor sayısı = %d, beklenen 1", private)
	}

	hidden := count(t, pool, `
		SELECT count(*) FROM incidents i
		JOIN monitors m ON m.id = i.auto_monitor_id
		WHERE NOT EXISTS (SELECT 1 FROM status_page_items s WHERE s.monitor_id = m.id)
		  AND i.title LIKE '%' || m.name || '%'`)
	if hidden != 1 {
		t.Errorf("yayımlanmamış servisin ADINI TAŞIYAN kesinti sayısı = %d, beklenen 1", hidden)
	}
}

// İkinci koşu SESSİZCE İKİNCİ BİR KOPYA ÜRETMİYOR.
func TestSeedRefusesToRunTwice(t *testing.T) {
	pool := newPool(t)

	if _, err := Run(t.Context(), pool, Options{}); err != nil {
		t.Fatalf("ilk seed başarısız: %v", err)
	}

	if _, err := Run(t.Context(), pool, Options{}); err == nil {
		t.Fatal("ikinci seed hata vermedi — sessizce ikinci bir demo üretilmiş olurdu")
	}

	// İki organizasyon: vitrin + ücretsiz demo (8.7). Üçüncüsü yok, yani
	// ikinci koşu gerçekten hiçbir şey yazmamış.
	if n := count(t, pool, `SELECT count(*) FROM organizations`); n != 2 {
		t.Errorf("organizasyon sayısı = %d, beklenen 2", n)
	}
}

// -reset var olanı SİLİP yeniden üretiyor ve ortada ARTIK kalmıyor.
func TestSeedResetRebuilds(t *testing.T) {
	pool := newPool(t)

	first, err := Run(t.Context(), pool, Options{})
	if err != nil {
		t.Fatalf("ilk seed başarısız: %v", err)
	}

	second, err := Run(t.Context(), pool, Options{Reset: true})
	if err != nil {
		t.Fatalf("reset başarısız: %v", err)
	}

	if first.OrgID == second.OrgID {
		t.Error("reset sonrası organizasyon kimliği aynı — silme çalışmamış")
	}

	// KATLANMA YOK: iki org, iki kullanıcı ve iki org'un monitor'leri —
	// reset ikisini de silip yeniden ürettiği için sayılar sabit kalıyor.
	if n := count(t, pool, `SELECT count(*) FROM organizations`); n != 2 {
		t.Errorf("organizasyon sayısı = %d, beklenen 2", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM users`); n != 2 {
		t.Errorf("kullanıcı sayısı = %d, beklenen 2", n)
	}
	// Cascade'ler geçmişi de götürmüş olmalı: monitor sayısı katlanmamalı.
	if want := 5 + len(freeMonitors); count(t, pool, `SELECT count(*) FROM monitors`) != want {
		t.Errorf("monitor sayısı = %d, beklenen %d",
			count(t, pool, `SELECT count(*) FROM monitors`), want)
	}

	// POSTA KUTUSU DA KATLANMIYOR (8.7): mesajlar organizasyona bağlı değil,
	// yani cascade onları götürmüyor — reset açıkça siliyor. Katlanan bir
	// posta kutusu, ESKİ demonun ölü linklerini yenisinin yanında bırakırdı.
	if n := count(t, pool, `SELECT count(*) FROM email_outbox`); n != second.OutboxMessages {
		t.Errorf("posta kutusu mesajı = %d, beklenen %d", n, second.OutboxMessages)
	}
}

// Seed BAŞKA verilere dokunmuyor: yalnızca demo organizasyonu siliniyor.
func TestSeedResetOnlyRemovesTheDemoOrg(t *testing.T) {
	pool := newPool(t)

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO organizations (name, slug) VALUES ('Başka Şirket', 'baska-sirket')`); err != nil {
		t.Fatalf("yabancı organizasyon eklenemedi: %v", err)
	}

	if _, err := Run(t.Context(), pool, Options{Reset: true}); err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	if n := count(t, pool,
		`SELECT count(*) FROM organizations WHERE slug = 'baska-sirket'`); n != 1 {
		t.Error("seed, demo dışındaki bir organizasyonu sildi")
	}
}

// Zaman testin elinde: geçmiş, seed'in çalıştığı ana göre üretiliyor.
func TestSeedHistoryIsRelativeToNow(t *testing.T) {
	pool := newPool(t)

	fixed := time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

	if _, err := Run(t.Context(), pool, Options{Now: func() time.Time { return fixed }}); err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	var newest, oldest time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT max(bucket_date), min(bucket_date) FROM uptime_rollup_daily`).
		Scan(&newest, &oldest); err != nil {
		t.Fatalf("özet aralığı okunamadı: %v", err)
	}

	if !newest.Equal(fixed.Truncate(24 * time.Hour)) {
		t.Errorf("en yeni özet = %s, beklenen %s", newest, fixed.Truncate(24*time.Hour))
	}
	if gap := newest.Sub(oldest).Hours() / 24; gap < 70 {
		t.Errorf("geçmiş yalnızca %.0f gün — grafiğin doldurulması bekleniyordu", gap)
	}
}

// --- Faz 8'in yüzeyleri (8.7) ------------------------------------------------

// DEMO E-POSTALARININ LİNKLERİ GERÇEKTEN ÇALIŞIYOR.
//
// Bu dosyanın en önemli 8.7 testi: posta kutusuna "gönderilmiş gibi" duran
// ama tıklanınca 404 veren mesajlar yazmak, 8.2'nin bütün amacını (akışın
// ÇALIŞTIĞINI göstermek) baltalardı. Test, e-postadaki HAM token'ın
// veritabanındaki kaydı gerçekten açtığını doğruluyor.
func TestSeedOutboxLinksResolveToRealRecords(t *testing.T) {
	pool := newPool(t)

	summary, err := Run(t.Context(), pool, Options{})
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	if summary.OutboxMessages == 0 {
		t.Fatal("posta kutusuna hiç mesaj yazılmamış")
	}

	bodies := map[string]string{}
	rows, err := pool.Query(t.Context(), `SELECT recipient, body FROM email_outbox`)
	if err != nil {
		t.Fatalf("posta kutusu okunamadı: %v", err)
	}
	for rows.Next() {
		var recipient, body string
		if err := rows.Scan(&recipient, &body); err != nil {
			t.Fatalf("satır okunamadı: %v", err)
		}
		bodies[recipient] = body
	}
	rows.Close()

	// 1) DAVET LİNKİ → bekleyen davet satırı (token veritabanında HASH'li).
	inviteToken := tokenFromBody(t, bodies[demoInviteEmail], "/invitations/accept?token=")
	if n := count(t, pool,
		`SELECT count(*) FROM organization_invitations WHERE token_hash = $1 AND accepted_at IS NULL`,
		token.Hash(inviteToken)); n != 1 {
		t.Errorf("davet linki hiçbir bekleyen daveti açmıyor (eşleşen satır: %d)", n)
	}

	// 2) ABONELİK ONAY LİNKİ → bekleyen abone (onay token'ı HASH'li).
	confirmToken := tokenFromBody(t, bodies[demoPendingSubscriberEmail], "/confirm?token=")
	if n := count(t, pool,
		`SELECT count(*) FROM status_page_subscribers WHERE confirm_token_hash = $1 AND status = 'pending'`,
		token.Hash(confirmToken)); n != 1 {
		t.Errorf("onay linki hiçbir bekleyen aboneyi açmıyor (eşleşen satır: %d)", n)
	}

	// 3) İPTAL LİNKİ → onaylı abone (iptal token'ı HAM saklanıyor — 6.4).
	unsubscribeToken := tokenFromBody(t, bodies[demoSubscriberEmail], "/unsubscribe?token=")
	if n := count(t, pool,
		`SELECT count(*) FROM status_page_subscribers WHERE unsubscribe_token = $1 AND status = 'confirmed'`,
		unsubscribeToken); n != 1 {
		t.Errorf("iptal linki hiçbir aboneyi açmıyor (eşleşen satır: %d)", n)
	}
}

// tokenFromBody, e-posta gövdesindeki linkten ham token'ı çıkarır.
func tokenFromBody(t *testing.T, body, marker string) string {
	t.Helper()

	if body == "" {
		t.Fatalf("gövde boş (aranan: %q)", marker)
	}

	_, after, found := strings.Cut(body, marker)
	if !found {
		t.Fatalf("gövdede %q yok: %q", marker, body)
	}

	return strings.Fields(after)[0]
}

// ÜCRETSİZ DEMO ORGANİZASYONU KOTASINA DAYANMIŞ ve verisi KENDİ PLANIYLA
// TUTARLI (8.5/8.7).
//
// Seed ham SQL yazdığı için kotayı teknik olarak delebilirdi; planına
// sığmayan bir demo, ekranda "5/5 monitor" yazarken kotayı aşan bir
// organizasyon göstermek olurdu.
func TestSeedFreeOrgSitsExactlyAtItsQuota(t *testing.T) {
	pool := newPool(t)

	summary, err := Run(t.Context(), pool, Options{})
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	var plan store.OrgPlan
	if err := pool.QueryRow(t.Context(),
		`SELECT plan FROM organizations WHERE id = $1`, summary.FreeOrgID).Scan(&plan); err != nil {
		t.Fatalf("plan okunamadı: %v", err)
	}
	if plan != store.OrgPlanFree {
		t.Errorf("ücretsiz demo org planı = %q, beklenen free", plan)
	}

	limits := plans.For(store.OrgPlanFree)

	// KOTA TAM DOLU: bir eksik olsaydı "sınıra dayanmış" demo olmazdı,
	// bir fazla olsaydı demo kendi kuralını çiğnerdi.
	n := count(t, pool, `SELECT count(*) FROM monitors WHERE org_id = $1`, summary.FreeOrgID)
	if n != limits.MaxMonitors {
		t.Errorf("ücretsiz org monitor sayısı = %d, beklenen %d (kota)", n, limits.MaxMonitors)
	}

	// HİÇBİR MONITOR PLANIN ALT SINIRININ ALTINDA DEĞİL.
	below := count(t, pool,
		`SELECT count(*) FROM monitors WHERE org_id = $1 AND check_interval_seconds < $2`,
		summary.FreeOrgID, limits.MinCheckIntervalSeconds)
	if below != 0 {
		t.Errorf("%d monitor planın alt sınırının altında — demo kendi planıyla çelişiyor", below)
	}
}

// VİTRİN ORGANİZASYONU **PRO**: 60 saniyelik monitor'leri FREE planda
// yazılamazdı, yani plan verisiyle tutarlı olmak zorunda.
func TestSeedShowcaseOrgIsProAndConsistent(t *testing.T) {
	pool := newPool(t)

	summary, err := Run(t.Context(), pool, Options{})
	if err != nil {
		t.Fatalf("seed başarısız: %v", err)
	}

	var plan store.OrgPlan
	if err := pool.QueryRow(t.Context(),
		`SELECT plan FROM organizations WHERE id = $1`, summary.OrgID).Scan(&plan); err != nil {
		t.Fatalf("plan okunamadı: %v", err)
	}
	if plan != store.OrgPlanPro {
		t.Fatalf("vitrin org planı = %q, beklenen pro", plan)
	}

	limits := plans.For(store.OrgPlanPro)

	below := count(t, pool,
		`SELECT count(*) FROM monitors WHERE org_id = $1 AND check_interval_seconds < $2`,
		summary.OrgID, limits.MinCheckIntervalSeconds)
	if below != 0 {
		t.Errorf("%d monitor PRO planın alt sınırının altında", below)
	}
}
