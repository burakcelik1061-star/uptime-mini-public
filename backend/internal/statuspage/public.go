package statuspage

// Status page'in ZİYARETÇİ yüzü (Faz 5.1) — bu projedeki ilk oturumsuz okuma.
//
// Buradaki her sorgu `status_page_items` üzerinden geçiyor. Kural tek cümle:
// YAYIMLANMAMIŞ BİR SERVİSİN ADI HİÇBİR ALANDAN ÇIKMAZ. En ince yeri 4.1'in
// otomatik incident başlığı ("<ad> yanıt vermiyor") — private bir servisin adı
// oradan sızabilirdi, bu yüzden süzgeç hem bağ tablosunda hem de
// `auto_monitor_id` üzerinde ayrıca yazılı (bkz. ListPublicIncidents).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

const (
	// RecentIncidentWindow, "son incident'lar" bölümünün geriye bakışı.
	// Kapanmış kesintiler bu pencereden eskiyse listede yok; geçmiş,
	// 5.2'nin 90 günlük barlarında yaşayacak.
	RecentIncidentWindow = 30 * 24 * time.Hour

	// MaxPublicIncidents / MaxPublicMaintenance, cevabın üst sınırları.
	// Ziyaretçi sayfalama yapamıyor (0.1 §2: ziyaretçi hiçbir şey yazmıyor,
	// sorgu parametresi de sunmuyoruz) — sınır sunucunun kendini koruması.
	MaxPublicIncidents   = 50
	MaxPublicMaintenance = 20
)

// PublicService, ziyaretçiye gösterilen bir servis.
//
// URL YOK (bkz. ListPublicServices): izlenen adres çoğu zaman iç bir uç.
// ID VAR ve opak: 5.2'nin 90 günlük barları ve 6.2'nin canlı güncellemeleri
// satırları bu kimlikle eşleştirecek; zaten yayımlanmış bir servisin
// tanıtıcısı olduğu için ek bir bilgi taşımıyor.
type PublicService struct {
	ID     uuid.UUID
	Name   string
	Status store.MonitorStatus
	// StatusChangedAt, "ne zamandır bu durumda" (3.3).
	StatusChangedAt time.Time
	// InMaintenance, servisin ŞU AN aktif bir bakım penceresinde olup olmadığı.
	//
	// Sunucuda hesaplanıyor çünkü genel durum özetini besliyor: bakımdaki bir
	// servis `down` görünebilir (4.3: bakım "ölçme"yi değil "alarm üretme"yi
	// durduruyor) ve o kesintiyi ziyaretçiye "kısmi kesinti" diye ilan etmek
	// planlı bakımı arıza gibi göstermek olurdu.
	InMaintenance bool
	// Stale, servisin ÖLÇÜMÜNÜN DURMUŞ olduğu (8.6).
	//
	// `Status` son BİLİNEN durumu taşımaya devam ediyor; bu bayrak "o bilgi
	// artık güncel değil" diyor. Kararı sunucu veriyor (istemci türetmiyor):
	// aynı kuralın panoda ve public sayfada iki kopyası olsaydı, bir gün
	// ayrışıp aynı monitor için iki farklı cevap üretirlerdi (5.2'nin "günün
	// hâli sunucudan gelir, istemci eşikten türetmez" kararının aynısı).
	Stale bool
	// LastCheckedAt, son ölçümün anı. nil = hiç kontrol edilmedi.
	//
	// Bayat servisin yanında "N dakikadır ölçüm yok" yazabilmek için gerekiyor;
	// süreyi METNE çevirmek arayüzün işi (4.4'ün "devam eden kesintinin 'şu ana
	// kadar'ı istemcide" kararıyla aynı bölüşüm).
	LastCheckedAt *time.Time
	// History, son 90 günün günlük uptime özeti (5.2). Her zaman TAM `HistoryDays`
	// eleman: verisi olmayan günler "veri yok" olarak DOLDURULMUŞ hâlde geliyor
	// (bkz. history.go).
	History []UptimeDay
	// Group, servisin grubu (5.3); nil = gruplanmamış.
	Group *string
}

// PublicIncident, ziyaretçiye gösterilen bir kesinti kaydı.
type PublicIncident struct {
	ID         uuid.UUID
	Title      string
	Status     store.IncidentStatus
	Impact     store.IncidentImpact
	StartedAt  time.Time
	ResolvedAt *time.Time
	// Services, YALNIZCA yayımlanan etkilenen servisler.
	Services []PublicAffected
	Updates  []PublicUpdate
}

// PublicAffected, incident'ın/pencerenin etkilediği yayımlanmış servis.
type PublicAffected struct {
	ID     uuid.UUID
	Name   string
	Status store.MonitorStatus
}

// PublicUpdate, timeline'ın tek satırı — YAZARSIZ.
//
// `author_id` public sözleşmede yok: org üyelerinin kimlikleri ziyaretçiyi
// ilgilendirmiyor. `is_system` duruyor, çünkü "bunu sistem yazdı" ile "bir
// mühendis yazdı" ayrımı okuyucu için anlamlı.
type PublicUpdate struct {
	ID        uuid.UUID
	Status    store.IncidentStatus
	Body      string
	IsSystem  bool
	CreatedAt time.Time
}

// PublicMaintenance, aktif veya yaklaşan bir bakım penceresi.
type PublicMaintenance struct {
	ID          uuid.UUID
	Title       string
	Description *string
	StartsAt    time.Time
	EndsAt      time.Time
	Services    []PublicAffected
}

// PublicPage, `GET /v1/public/status/{slug}` cevabının tamamı.
//
// TEK İSTEKTE TEK SAYFA: servisler, genel durum, kesintiler ve bakımlar aynı
// cevapta. Ziyaretçinin gördüğü ekran bölünmez bir bütün ve sunucuda render
// ediliyor (0.2'nin SSR/SEO kararı) — dört ayrı uç, sunucu tarafında dört
// gidiş-geliş demekti.
type PublicPage struct {
	Slug string
	// Title, org'un yazdığı başlık; yazmadıysa organizasyonun ADI (5.3).
	// Sunucuda çözülüyor: "başlık yoksa org adını kullan" kuralının iki ayrı
	// istemcide iki kopyası olmamalı.
	Title string
	// Description, sayfanın altbaşlığı (5.3). nil = yok.
	Description *string
	// LogoURL, https ile doğrulanmış dış adres (5.3). nil = logo yok.
	LogoURL *string
	// ThemeColor, org'un seçtiği `#rrggbb` (5.3). nil = varsayılan vurgu rengi.
	ThemeColor *string
	// ThemeTone, tema renginin ÜZERİNE yazılacak metnin tonu (bkz. theme.go).
	// ThemeColor nil ise anlamsız; istemci o durumda kendi varsayılanını
	// kullanıyor.
	ThemeTone Tone
	// Groups, servis gruplarının GÖSTERİM SIRASI (5.3). Gruplanmamış servisler
	// bu listede yok; arayüz onları en sona koyuyor.
	Groups   []string
	Overall  Overall
	Services []PublicService
	// StaleServices, ölçümü durmuş yayımlanmış servis sayısı (8.6).
	//
	// NEDEN AYRI BİR SAYAÇ ve neden `Overall`a yeni bir değer eklenmedi:
	// karışık durumda (üç sağlam + bir bayat) `Summarize` `operational`
	// döndürüyor ve sayfa "Tüm sistemler çalışıyor" diyor. Bayat servisin
	// SATIRI dürüst — kendi rozetinde "ölçüm yok" yazıyor — ama BAŞLIK
	// değil. Bu sayaç, arayüzün sayfa üstüne bir not koymasını sağlıyor ve
	// 5.1'in `Overall` sözleşmesine hiç dokunmuyor.
	StaleServices int
	Incidents     []PublicIncident
	Maintenance   []PublicMaintenance
}

// Public, slug'a karşılık gelen YAYIMLANMIŞ status sayfasını döndürür.
//
// Yayımlanmamış sayfa ve olmayan slug aynı cevabı veriyor: ErrNotFound.
func (s *Service) Public(ctx context.Context, slug string) (PublicPage, error) {
	slug = NormalizeSlug(slug)
	if slug == "" {
		// Boş/yalnızca boşluktan oluşan slug için veritabanına gitmeye gerek yok.
		return PublicPage{}, ErrNotFound
	}

	page, err := s.queries.GetPublishedStatusPage(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PublicPage{}, ErrNotFound
		}

		return PublicPage{}, fmt.Errorf("status page okunamadı: %w", err)
	}

	// "ŞİMDİ" TEK YERDEN VE VERİTABANINDAN okunuyor (3.2'nin "zamanın kaynağı
	// veritabanı" kararının okuma tarafındaki karşılığı). Uygulama saatinden
	// alınsaydı aynı cevabın iki yarısı iki farklı ana bakardı: bakım
	// pencerelerinin süzgeci SQL'de `now()` kullanıyor, "şu an bakımda mı" ve
	// 90 günlük pencere ise burada hesaplanıyor.
	now := page.ServerNow.UTC()

	// YAYIMLANAN SERVİSLER BİR KEZ OKUNUYOR: hem geçmiş dizisinin hangi
	// servisler için üretileceğini hem de cevabın servis listesini aynı satırlar
	// besliyor. İki ayrı okuma, aynı isteğin içinde ayrışabilen iki liste
	// demekti (ve boşuna bir sorgu).
	rows, err := s.queries.ListPublicServices(ctx, page.ID)
	if err != nil {
		return PublicPage{}, fmt.Errorf("yayımlanan servisler okunamadı: %w", err)
	}

	maintenance, err := s.publicMaintenance(ctx, page.OrgID, page.ID)
	if err != nil {
		return PublicPage{}, err
	}

	histories, err := s.history(ctx, page.OrgID, page.ID, rows, now)
	if err != nil {
		return PublicPage{}, err
	}

	services := publicServices(rows, maintenance, histories, now)

	incidents, err := s.publicIncidents(ctx, page.OrgID, page.ID, now)
	if err != nil {
		return PublicPage{}, err
	}

	groups, err := s.publicGroups(ctx, page.ID)
	if err != nil {
		return PublicPage{}, err
	}

	// Başlık ÇÖZÜLMÜŞ hâlde dönüyor: "özel başlık yoksa organizasyonun adı"
	// kuralı sunucuda kalmalı, yoksa her istemci aynı `??` ifadesini yeniden
	// yazar ve biri unutunca sayfa başlıksız kalır.
	title := page.OrgName
	if page.Title != nil {
		title = *page.Title
	}

	tone := ToneLight
	if page.ThemeColor != nil {
		tone = ToneOn(*page.ThemeColor)
	}

	return PublicPage{
		Slug:          page.Slug,
		Title:         title,
		Description:   page.Description,
		LogoURL:       page.LogoUrl,
		ThemeColor:    page.ThemeColor,
		ThemeTone:     tone,
		Groups:        groups,
		Overall:       Summarize(services),
		Services:      services,
		StaleServices: countStale(services),
		Incidents:     incidents,
		Maintenance:   maintenance,
	}, nil
}

// publicGroups, grup adlarını gösterim sırasında döndürür.
func (s *Service) publicGroups(ctx context.Context, pageID uuid.UUID) ([]string, error) {
	rows, err := s.queries.ListPublicStatusPageGroups(ctx, pageID)
	if err != nil {
		return nil, fmt.Errorf("gruplar okunamadı: %w", err)
	}

	groups := make([]string, 0, len(rows))
	for _, row := range rows {
		groups = append(groups, row.Name)
	}

	return groups, nil
}

// publicServices, okunmuş servis satırlarını bakım işaretleriyle birleştirir.
//
// SAF: veritabanına gitmiyor (satırları çağıran okudu). Böylece "hangi servis
// şu an bakımda" kararı gerçek veri kurmadan da sınanabiliyor ve aynı satırlar
// hem burada hem geçmiş dizisinde kullanılıyor.
func publicServices(
	rows []store.ListPublicServicesRow,
	maintenance []PublicMaintenance,
	histories map[uuid.UUID][]UptimeDay,
	now time.Time,
) []PublicService {
	// AKTİF pencerelerin kapsadığı servisler. Yarı açık aralık [starts, ends):
	// 2.3'ten beri bu projedeki tüm zaman aralıklarının kuralı.
	inMaintenance := make(map[uuid.UUID]struct{})
	for _, window := range maintenance {
		if now.Before(window.StartsAt) || !now.Before(window.EndsAt) {
			continue
		}

		for _, service := range window.Services {
			inMaintenance[service.ID] = struct{}{}
		}
	}

	services := make([]PublicService, 0, len(rows))
	for _, row := range rows {
		_, maintaining := inMaintenance[row.MonitorID]

		services = append(services, PublicService{
			ID:              row.MonitorID,
			Name:            row.Name,
			Status:          row.CurrentStatus,
			StatusChangedAt: row.StatusChangedAt,
			InMaintenance:   maintaining,
			// BAYATLIK KARARI (8.6) — "şimdi" VERİTABANINDAN geliyor
			// (`page.ServerNow`, bkz. Public). api sürecinin `time.Now()`u
			// kullanılsaydı, `last_checked_at`i yazan worker ile karşılaştıran
			// api arasındaki saat kayması bayatlığa dönüşürdü. 7.5 aynı hatayı
			// public cevabın "şimdi"si için düzeltmişti.
			Stale: monitors.IsStale(
				row.IsActive, row.LastCheckedAt, row.CheckIntervalSeconds, now),
			LastCheckedAt: row.LastCheckedAt,
			History:       histories[row.MonitorID],
			Group:         row.GroupName,
		})
	}

	return services
}

// publicIncidents, açık ve yakın geçmişteki kesintileri timeline'larıyla okur.
func (s *Service) publicIncidents(
	ctx context.Context, orgID, pageID uuid.UUID, now time.Time,
) ([]PublicIncident, error) {
	rows, err := s.queries.ListPublicIncidents(ctx, store.ListPublicIncidentsParams{
		OrgID:        orgID,
		StatusPageID: pageID,
		Since:        now.Add(-RecentIncidentWindow),
		RowLimit:     MaxPublicIncidents,
	})
	if err != nil {
		return nil, fmt.Errorf("kesintiler okunamadı: %w", err)
	}

	if len(rows) == 0 {
		return []PublicIncident{}, nil
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	// Etkilenen servisler ve timeline TEK sorguda (4.2'nin N+1'siz deseni).
	affectedRows, err := s.queries.ListPublicIncidentMonitors(ctx, store.ListPublicIncidentMonitorsParams{
		StatusPageID: pageID,
		IncidentIds:  ids,
	})
	if err != nil {
		return nil, fmt.Errorf("etkilenen servisler okunamadı: %w", err)
	}

	affected := make(map[uuid.UUID][]PublicAffected, len(ids))
	for _, row := range affectedRows {
		affected[row.IncidentID] = append(affected[row.IncidentID], PublicAffected{
			ID:     row.MonitorID,
			Name:   row.Name,
			Status: row.CurrentStatus,
		})
	}

	updateRows, err := s.queries.ListPublicIncidentUpdates(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("kesinti güncellemeleri okunamadı: %w", err)
	}

	updates := make(map[uuid.UUID][]PublicUpdate, len(ids))
	for _, row := range updateRows {
		updates[row.IncidentID] = append(updates[row.IncidentID], PublicUpdate{
			ID:        row.ID,
			Status:    row.Status,
			Body:      row.Body,
			IsSystem:  row.IsSystem,
			CreatedAt: row.CreatedAt,
		})
	}

	incidents := make([]PublicIncident, 0, len(rows))
	for _, row := range rows {
		incidents = append(incidents, PublicIncident{
			ID:         row.ID,
			Title:      row.Title,
			Status:     row.Status,
			Impact:     row.Impact,
			StartedAt:  row.StartedAt,
			ResolvedAt: row.ResolvedAt,
			Services:   affected[row.ID],
			Updates:    updates[row.ID],
		})
	}

	return incidents, nil
}

// publicMaintenance, aktif ve yaklaşan bakım pencerelerini okur.
func (s *Service) publicMaintenance(ctx context.Context, orgID, pageID uuid.UUID) ([]PublicMaintenance, error) {
	rows, err := s.queries.ListPublicMaintenanceWindows(ctx, store.ListPublicMaintenanceWindowsParams{
		OrgID:        orgID,
		StatusPageID: pageID,
		RowLimit:     MaxPublicMaintenance,
	})
	if err != nil {
		return nil, fmt.Errorf("bakım pencereleri okunamadı: %w", err)
	}

	if len(rows) == 0 {
		return []PublicMaintenance{}, nil
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	monitorRows, err := s.queries.ListPublicMaintenanceMonitors(ctx, store.ListPublicMaintenanceMonitorsParams{
		StatusPageID: pageID,
		WindowIds:    ids,
	})
	if err != nil {
		return nil, fmt.Errorf("bakım servisleri okunamadı: %w", err)
	}

	affected := make(map[uuid.UUID][]PublicAffected, len(ids))
	for _, row := range monitorRows {
		affected[row.WindowID] = append(affected[row.WindowID], PublicAffected{
			ID:     row.MonitorID,
			Name:   row.Name,
			Status: row.CurrentStatus,
		})
	}

	windows := make([]PublicMaintenance, 0, len(rows))
	for _, row := range rows {
		windows = append(windows, PublicMaintenance{
			ID:          row.ID,
			Title:       row.Title,
			Description: row.Description,
			StartsAt:    row.StartsAt,
			EndsAt:      row.EndsAt,
			Services:    affected[row.ID],
		})
	}

	return windows, nil
}

// NormalizeSlug, yol/alt alan adından gelen slug'ı arama biçimine getirir.
//
// Küçültme burada, sorguda `lower(slug)` DEĞİL: sütunda zaten yalnızca küçük
// harf var (şemadaki CHECK) ve sorguya fonksiyon koymak UNIQUE indeksin
// kullanılmasını engellerdi. 5.4'te alt alan adı da buradan geçecek — alan
// adları büyük/küçük harf duyarsız.
func NormalizeSlug(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// countStale, ölçümü durmuş servis sayısı (8.6).
func countStale(services []PublicService) int {
	count := 0
	for _, service := range services {
		if service.Stale {
			count++
		}
	}

	return count
}
