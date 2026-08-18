// Package seed, çalıştırılabilir bir DEMO ORTAMI üretir (7.4).
//
// Amaç somut: depoyu klonlayan biri (ör. bir mülakatçı) `docker compose up`
// dedikten sonra 5.1-5.3'ün ekranlarını BOŞ görmemeli. Boş bir uptime
// monitörü hiçbir şey anlatmıyor — 90 günlük grafik, kesinti geçmişi ve
// yayımlanmış bir status page ancak veriyle görünür hâle geliyor.
//
// HİÇBİR DIŞ HESAP GEREKTİRMİYOR (GÜNCEL DURUM §3): tek ihtiyaç veritabanı.
//
// # Neden ham SQL, neden servis katmanı değil
//
// Bu paket GEÇMİŞE TARİHLİ veri yazıyor: 90 günlük özetler, günler önce
// açılıp kapanmış kesintiler, geçmiş timeline satırları. Uygulamanın kendisi
// bunları bilinçli olarak YAZAMIYOR —
//
//   - 4.4: `started_at` istemciden alınmıyor ("geçmişe tarihlenen incident,
//     kesinti geçmişini elle yazmanın kapısıdır"),
//   - 4.3: başlamış bakım penceresi değiştirilemiyor,
//   - 3.4: kapanmış kovalar yeniden hesaplanmıyor.
//
// Yani seed, servis katmanından geçemez; geçebilseydi o kurallar delinmiş
// olurdu. Monitor gibi NORMAL kayıtlar için yine de `internal/store`un
// üretilmiş sorguları kullanılıyor (`next_check_at` gibi ayrıntılar
// uygulamayla aynı yerden gelsin).
package seed

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

const (
	// DemoEmail / DemoPassword, panoya girmek için.
	//
	// Sır DEĞİL ve öyle görünmeli: bu hesap yalnızca demo verisiyle birlikte
	// var olan bir hesap. Parola 1.1'in alt sınırını (8 karakter) geçiyor.
	DemoEmail    = "demo@uptime.local"
	DemoPassword = "demo-uptime-2026"

	// demoOrgName / demoOrgSlug, demo organizasyonu.
	demoOrgName = "Acme Corp"
	demoOrgSlug = "acme"

	// historyDays, üretilecek günlük özet sayısı — 5.2'nin grafiği tam 90
	// gün çiziyor.
	historyDays = 90

	// checksPerDay, bir günün kaç kontrolle özetlendiği (60 sn aralık).
	checksPerDay = 1440

	// defaultPublicBaseURL, demo e-postalarındaki linklerin kökü (8.7).
	defaultPublicBaseURL = "http://localhost:3100"

	// recentRawHours, ham kontrol satırı yazılacak saat sayısı.
	//
	// 90 günün tamamı ham satır olarak yazılmıyor: 5 monitor × 90 gün ×
	// 1440 = 648.000 satır demekti ve zaten 3.4'ün budaması onları 30 günde
	// silerdi. Grafiğin verisi ÖZETLERDEN geliyor (5.2'nin kararı); ham
	// satırlar yalnızca monitor detay sayfasının (2.4) son kontroller listesi
	// ve yanıt süresi grafiği için gerekiyor.
	recentRawHours = 6
)

// Summary, üretilen verinin özeti (çağıran loglasın diye).
type Summary struct {
	OrgID          uuid.UUID
	StatusPageSlug string
	Monitors       int
	DailyRollups   int
	RawChecks      int
	Incidents      int
	// FreeOrgID, kotasına dayanmış ÜCRETSİZ demo organizasyonu (8.5/8.7).
	FreeOrgID uuid.UUID
	// Invitations / Subscribers / OutboxMessages, Faz 8'in yüzeylerini
	// dolduran demo kayıtları (bkz. phase8.go).
	Invitations    int
	Subscribers    int
	OutboxMessages int
}

// Options, seed davranışı.
type Options struct {
	// Reset açıkken var olan demo organizasyonu SİLİNİP yeniden üretiliyor.
	// Kapalıyken var olan bir demo hata veriyor: sessizce ikinci bir kopya
	// üretmek, "neden iki Acme Corp var" sorusuna yol açardı.
	Reset bool
	// Now, testlerin saati sabitleyebilmesi için (üretimde time.Now).
	Now func() time.Time
	// PublicBaseURL, posta kutusundaki demo e-postalarının link kökü (8.7).
	//
	// Boşsa `http://localhost:3100` kullanılıyor: seed'in çalışması hiçbir
	// yapılandırmaya bağlı olmamalı (GÜNCEL DURUM §3) ve 0.2'de seçilen port
	// zaten bu.
	PublicBaseURL string
}

// Run, demo veriyi TEK BİR TRANSACTION'da üretir.
//
// Yarıda kalan bir seed, yarım bir demo demekti: monitor'ü olan ama geçmişi
// olmayan, ya da yayımlanmış ama servissiz bir status page. Ya hepsi ya
// hiçbiri.
func Run(ctx context.Context, pool *pgxpool.Pool, opts Options) (Summary, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("transaction açılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := existingDemoOrg(ctx, tx)
	if err != nil {
		return Summary{}, err
	}

	if existing != uuid.Nil {
		if !opts.Reset {
			return Summary{}, fmt.Errorf(
				"demo organizasyonu (%s) zaten var — yeniden üretmek için -reset verin", demoOrgSlug)
		}

		// Cascade'ler monitor'leri, kesintileri, özetleri ve status page'i
		// birlikte götürüyor (0.1'in silme kararları).
		if _, err := tx.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, existing); err != nil {
			return Summary{}, fmt.Errorf("eski demo silinemedi: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE email = $1`, DemoEmail); err != nil {
			return Summary{}, fmt.Errorf("eski demo kullanıcısı silinemedi: %w", err)
		}

		// 8.7'nin ÜCRETSİZ demo organizasyonu ve kullanıcısı da düşüyor:
		// ikisi tek bir demonun parçası ve yarısını bırakmak, `-reset`in
		// vaadini ("temiz bir demo") bozardı.
		if _, err := tx.Exec(ctx, `DELETE FROM organizations WHERE slug = $1`, freeOrgSlug); err != nil {
			return Summary{}, fmt.Errorf("eski ücretsiz demo silinemedi: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE email = $1`, FreeDemoEmail); err != nil {
			return Summary{}, fmt.Errorf("eski ücretsiz demo kullanıcısı silinemedi: %w", err)
		}

		// POSTA KUTUSU DA TEMİZLENİYOR (8.2): mesajlar organizasyona bağlı
		// değil (tabloda org_id yok), yani cascade onları götürmüyor. Eski
		// demonun ÖLÜ linkleri yeni demonun yanında durursa, tıklayan kişi
		// çalışmayan bir bağlantıyla karşılaşırdı.
		if _, err := tx.Exec(ctx, `DELETE FROM email_outbox`); err != nil {
			return Summary{}, fmt.Errorf("eski posta kutusu temizlenemedi: %w", err)
		}
	}

	publicBaseURL := opts.PublicBaseURL
	if publicBaseURL == "" {
		publicBaseURL = defaultPublicBaseURL
	}

	builder := &builder{
		tx:            tx,
		queries:       store.New(tx),
		now:           now,
		rng:           rand.New(rand.NewPCG(7, 42)),
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}

	summary, err := builder.run(ctx)
	if err != nil {
		return Summary{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Summary{}, fmt.Errorf("transaction kapatılamadı: %w", err)
	}

	return summary, nil
}

func existingDemoOrg(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	var id uuid.UUID

	err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = $1`, demoOrgSlug).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return uuid.Nil, nil
		}

		return uuid.Nil, fmt.Errorf("demo organizasyonu aranamadı: %w", err)
	}

	return id, nil
}

// builder, üretimin durumunu taşır.
type builder struct {
	tx      pgx.Tx
	queries *store.Queries
	now     func() time.Time
	// rng SABİT TOHUMLU: aynı seed komutu her makinede aynı grafiği
	// üretsin — ekran görüntüsü alan biri "bendeki neden farklı" demesin.
	rng *rand.Rand
	// publicBaseURL, demo e-postalarındaki linklerin kökü (8.7).
	publicBaseURL string
}

// demoMonitor, üretilecek monitor'ün tarifi.
type demoMonitor struct {
	name     string
	url      string
	group    string
	status   store.MonitorStatus
	public   bool
	interval int32
	// badDays, bu monitor'ün geçmişinde kesintili olan günler (bugünden
	// geriye).
	badDays map[int]float64
}

func (b *builder) run(ctx context.Context) (Summary, error) {
	userID, err := b.createUser(ctx)
	if err != nil {
		return Summary{}, err
	}

	orgID, err := b.createOrg(ctx, userID)
	if err != nil {
		return Summary{}, err
	}

	monitors := []demoMonitor{
		{
			// HEDEFLER GERÇEK VE KALICI ADRESLER (IANA'nın örnek alan adları):
			// worker demo verisinin ÜSTÜNE canlı kontroller yazıyor, yani
			// hedefin cevabı ile seed'in yazdığı durum çelişmemeli. 200
			// dönen adresler "çalışıyor" kalıyor; aşağıdaki tek "kesinti"
			// monitor'ü bilerek 404 dönen bir yola bakıyor, böylece açık
			// kesinti demo boyunca AÇIK kalıyor.
			name: "Web Sitesi", url: "https://example.com", group: "Uygulama",
			status: store.MonitorStatusUp, public: true, interval: 60,
			badDays: map[int]float64{12: 0.97, 41: 0.92},
		},
		{
			name: "API", url: "https://example.org", group: "Uygulama",
			status: store.MonitorStatusUp, public: true, interval: 60,
			badDays: map[int]float64{12: 0.86, 13: 0.99, 55: 0.94},
		},
		{
			// AÇIK KESİNTİ BU MONITOR'DE: demo ekranları "her şey yolunda"
			// hâlinden ibaret olmasın.
			name: "Ödeme Servisi", url: "https://example.com/payments", group: "Uygulama",
			status: store.MonitorStatusDown, public: true, interval: 60,
			badDays: map[int]float64{0: 0.62, 12: 0.71, 30: 0.98},
		},
		{
			name: "CDN", url: "https://example.net", group: "Altyapı",
			status: store.MonitorStatusUp, public: true, interval: 300,
			badDays: map[int]float64{7: 0.99},
		},
		{
			// YAYIMLANMAMIŞ: 5.1'in sızıntı kuralı demo veride de görünür
			// olsun — bu servisin adı public sayfada HİÇBİR yerde geçmiyor.
			name: "Iç Yedekleme Işi", url: "https://www.iana.org/help/example-domains", group: "",
			status: store.MonitorStatusUp, public: false, interval: 300,
			badDays: map[int]float64{3: 0.5},
		},
	}

	summary := Summary{OrgID: orgID, StatusPageSlug: demoOrgSlug}

	created := make([]store.Monitor, 0, len(monitors))
	for _, spec := range monitors {
		monitor, err := b.createMonitor(ctx, orgID, spec)
		if err != nil {
			return Summary{}, err
		}
		created = append(created, monitor)

		days, err := b.writeHistory(ctx, monitor.ID, spec)
		if err != nil {
			return Summary{}, err
		}
		summary.DailyRollups += days

		raw, err := b.writeRecentChecks(ctx, monitor.ID, spec)
		if err != nil {
			return Summary{}, err
		}
		summary.RawChecks += raw
	}
	summary.Monitors = len(created)

	incidents, err := b.writeIncidents(ctx, orgID, userID, created, monitors)
	if err != nil {
		return Summary{}, err
	}
	summary.Incidents = incidents

	if err := b.writeMaintenance(ctx, orgID, created[3].ID); err != nil {
		return Summary{}, err
	}

	if err := b.publishStatusPage(ctx, orgID, created, monitors); err != nil {
		return Summary{}, err
	}

	if err := b.writeNotificationChannel(ctx, orgID); err != nil {
		return Summary{}, err
	}

	// --- FAZ 8'İN YÜZEYLERİ (8.7, bkz. phase8.go) -------------------------

	inviteToken, err := b.writeInvitation(ctx, orgID, userID)
	if err != nil {
		return Summary{}, err
	}
	summary.Invitations = 1

	statusPageID, err := b.statusPageID(ctx, orgID)
	if err != nil {
		return Summary{}, err
	}

	subs, err := b.writeSubscribers(ctx, statusPageID)
	if err != nil {
		return Summary{}, err
	}
	summary.Subscribers = 2

	sent, err := b.writeOutbox(ctx, b.publicBaseURL, inviteToken, subs)
	if err != nil {
		return Summary{}, err
	}
	summary.OutboxMessages = sent

	freeOrgID, err := b.createFreeOrg(ctx)
	if err != nil {
		return Summary{}, err
	}
	summary.FreeOrgID = freeOrgID
	summary.Monitors += len(freeMonitors)

	return summary, nil
}

// statusPageID, organizasyonun status page kimliğini döner.
func (b *builder) statusPageID(ctx context.Context, orgID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	if err := b.tx.QueryRow(ctx,
		`SELECT id FROM status_pages WHERE org_id = $1`, orgID).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("status page kimliği okunamadı: %w", err)
	}

	return id, nil
}

func (b *builder) createUser(ctx context.Context) (uuid.UUID, error) {
	// Parola ÜRETİM parametreleriyle hash'leniyor: demo hesabı da gerçek
	// giriş akışından geçiyor (1.1).
	hash, err := auth.NewHasher().Hash(DemoPassword)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parola hash'lenemedi: %w", err)
	}

	user, err := b.queries.CreateUser(ctx, store.CreateUserParams{
		Email:        DemoEmail,
		PasswordHash: hash,
		Name:         "Demo Kullanıcı",
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("demo kullanıcısı oluşturulamadı: %w", err)
	}

	return user.ID, nil
}

func (b *builder) createOrg(ctx context.Context, userID uuid.UUID) (uuid.UUID, error) {
	org, err := b.queries.CreateOrganization(ctx, store.CreateOrganizationParams{
		Name: demoOrgName,
		Slug: demoOrgSlug,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("demo organizasyonu oluşturulamadı: %w", err)
	}

	// ACME **PRO** PLANDA (8.5). Vitrin organizasyonu bu: 90 günlük geçmiş,
	// kesintiler, temalı status page ve 60-300 saniyelik aralıklar. FREE
	// planın alt sınırı 300 saniye, yani 60 saniyelik monitor'ler o planda
	// YAZILAMAZDI — demo verisi kendi planıyla çelişmemeli. Kotanın nasıl
	// ısırdığını gösteren org ayrı ve ücretsiz (bkz. phase8.go).
	if _, err := b.queries.UpdateOrganizationPlan(ctx, store.UpdateOrganizationPlanParams{
		ID:   org.ID,
		Plan: store.OrgPlanPro,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("demo organizasyonunun planı ayarlanamadı: %w", err)
	}

	if _, err := b.queries.AddOrganizationMember(ctx, store.AddOrganizationMemberParams{
		OrgID:  org.ID,
		UserID: userID,
		Role:   store.OrgRoleOwner,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("üyelik eklenemedi: %w", err)
	}

	// Status page organizasyonla AYNI transaction'da doğuyor (5.1'in kuralı);
	// burada da öyle. Yayına açılması aşağıda, servisler seçildikten sonra.
	if _, err := b.tx.Exec(ctx,
		`INSERT INTO status_pages (org_id, slug) VALUES ($1, $2)`, org.ID, demoOrgSlug); err != nil {
		return uuid.Nil, fmt.Errorf("status page oluşturulamadı: %w", err)
	}

	return org.ID, nil
}

func (b *builder) createMonitor(
	ctx context.Context, orgID uuid.UUID, spec demoMonitor,
) (store.Monitor, error) {
	degraded := int32(800)

	monitor, err := b.queries.CreateMonitor(ctx, store.CreateMonitorParams{
		OrgID:                orgID,
		Name:                 spec.name,
		Url:                  spec.url,
		Method:               store.MonitorMethodGET,
		CheckIntervalSeconds: spec.interval,
		TimeoutMs:            10000,
		ExpectedStatusMin:    200,
		ExpectedStatusMax:    299,
		DegradedResponseMs:   &degraded,
		FailureThreshold:     3,
		RecoveryThreshold:    2,
		IsActive:             true,
		CurrentStatus:        spec.status,
	})
	if err != nil {
		return store.Monitor{}, fmt.Errorf("%s monitor'ü oluşturulamadı: %w", spec.name, err)
	}

	// Durum sütunları doğrudan yazılıyor: 3.3'ün durum makinesi yalnızca
	// GERÇEK bir ölçümle çalışıyor ve demo verinin ölçümü geçmişte.
	changedAt := b.now().Add(-3 * time.Hour)
	failures := int32(0)
	if spec.status == store.MonitorStatusDown {
		failures = 3
	}

	if _, err := b.tx.Exec(ctx, `
		UPDATE monitors
		SET last_checked_at = $2, status_changed_at = $3, consecutive_failures = $4
		WHERE id = $1`,
		monitor.ID, b.now().Add(-time.Minute), changedAt, failures); err != nil {
		return store.Monitor{}, fmt.Errorf("%s durumu yazılamadı: %w", spec.name, err)
	}

	return monitor, nil
}

// writeHistory, 90 günlük GÜNLÜK ÖZETLERİ yazar (5.2'nin grafiği).
func (b *builder) writeHistory(ctx context.Context, monitorID uuid.UUID, spec demoMonitor) (int, error) {
	today := b.now().UTC().Truncate(24 * time.Hour)

	rows := make([][]any, 0, historyDays)
	for day := range historyDays {
		date := today.AddDate(0, 0, -day)

		// VERİSİ OLMAYAN GÜN BİLİNÇLİ: 5.2'nin `no_data` durumu (tarama
		// deseni) demo veride de görünsün. Monitor'ün "kurulmadan önceki"
		// günleri.
		if day > 82 {
			continue
		}

		uptime := 1.0
		if ratio, bad := spec.badDays[day]; bad {
			uptime = ratio
		}

		total := checksPerDay
		if spec.interval > 60 {
			total = checksPerDay / int(spec.interval/60)
		}

		failure := int(float64(total) * (1 - uptime))
		degraded := 0
		if uptime < 1 && failure > 4 {
			// Kesintili günlerde bir miktar "yavaş" ölçüm de olsun: degraded
			// UP sayılıyor (0.1) ve grafikte bunun görünmesi doğru.
			degraded = failure / 4
		}
		success := total - failure - degraded

		avg := 120 + b.rng.IntN(60)
		p95 := avg + 80 + b.rng.IntN(200)

		rows = append(rows, []any{
			monitorID, date, total, success, degraded, failure, 0,
			avg, p95, failure * int(spec.interval),
		})
	}

	if _, err := b.tx.CopyFrom(ctx,
		pgx.Identifier{"uptime_rollup_daily"},
		[]string{
			"monitor_id", "bucket_date", "total_checks", "success_checks",
			"degraded_checks", "failure_checks", "maintenance_checks",
			"avg_response_ms", "p95_response_ms", "downtime_seconds",
		},
		pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("%s geçmişi yazılamadı: %w", spec.name, err)
	}

	return len(rows), nil
}

// writeRecentChecks, son birkaç saatin HAM kontrol satırlarını yazar
// (2.4'ün son kontroller listesi ve yanıt süresi grafiği).
func (b *builder) writeRecentChecks(
	ctx context.Context, monitorID uuid.UUID, spec demoMonitor,
) (int, error) {
	now := b.now().UTC()
	interval := time.Duration(spec.interval) * time.Second
	count := int(time.Duration(recentRawHours) * time.Hour / interval)

	if err := b.ensurePartitions(ctx, now.Add(-time.Duration(recentRawHours)*time.Hour), now); err != nil {
		return 0, err
	}

	rows := make([][]any, 0, count)
	for i := range count {
		at := now.Add(-time.Duration(i) * interval)

		outcome := store.CheckOutcomeSuccess
		status := int32(200)
		responseMs := int32(90 + b.rng.IntN(180))
		var errorType any
		var errorMessage any

		switch {
		// Düşmüş monitor son yarım saattir hata veriyor.
		case spec.status == store.MonitorStatusDown && i < int(30*time.Minute/interval):
			outcome = store.CheckOutcomeFailure
			status = 503
			errorType = string(store.CheckErrorTypeUnexpectedStatus)
			errorMessage = "beklenmeyen durum kodu: 503"
		// Arada bir yavaşlık: degraded'ın ekranda görünmesi için.
		case i%37 == 0:
			outcome = store.CheckOutcomeDegraded
			responseMs = 900 + int32(b.rng.IntN(400))
		}

		rows = append(rows, []any{
			monitorID, at, outcome, status, responseMs,
			errorType, errorMessage, store.CheckTriggerScheduled,
		})
	}

	if _, err := b.tx.CopyFrom(ctx,
		pgx.Identifier{"check_results"},
		[]string{
			"monitor_id", "checked_at", "outcome", "http_status",
			"response_time_ms", "error_type", "error_message", "triggered_by",
		},
		pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("%s ham kontrolleri yazılamadı: %w", spec.name, err)
	}

	return len(rows), nil
}

// ensurePartitions, ham satırların düşeceği günlerin partition'larını açar.
//
// Migration'daki `ensure_check_result_partitions` `now()`a göre çalışıyor
// (dün + 30 gün), yani normal bir kurulumda bu fonksiyonun yapacak işi yok.
// Yine de duruyor: seed'in saati testlerde sabitlenebiliyor ve partition'ı
// olmayan bir güne yazmak "no partition of relation" ile patlardı — 2.3'ün
// DEFAULT partition'ı bilinçli olarak yok (eksik partition sessizce
// birikmesin).
func (b *builder) ensurePartitions(ctx context.Context, from, to time.Time) error {
	day := from.UTC().Truncate(24 * time.Hour)
	last := to.UTC().Truncate(24 * time.Hour)

	for ; !day.After(last); day = day.AddDate(0, 0, 1) {
		name := "check_results_" + day.Format("20060102")

		// DDL parametre almıyor; sınırlar tarihten üretiliyor (dışarıdan
		// gelen bir değer yok).
		if _, err := b.tx.Exec(ctx, fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF check_results FOR VALUES FROM (%s) TO (%s)",
			name,
			quoteTimestamp(day),
			quoteTimestamp(day.AddDate(0, 0, 1)))); err != nil {
			return fmt.Errorf("partition oluşturulamadı (%s): %w", name, err)
		}
	}

	return nil
}

func quoteTimestamp(at time.Time) string {
	return "'" + at.Format("2006-01-02") + " 00:00:00+00'"
}
