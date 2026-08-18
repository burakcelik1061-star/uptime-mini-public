package httpapi

// PUBLIC YÜZEY (Faz 5.1) — api'nin oturum İSTEMEYEN tek alt-router'ı.
//
// Neden ayrı bir alt-router:
//
//   - Kimlik kaynağı BAŞKA. Faz 1'den beri her uç kimliği token'dan, kiracıyı
//     üyelikten alıyordu; burada kiracıyı yoldaki SLUG belirliyor ve kimlik
//     hiç yok. Bu ucu `requireAuth`/`requireOrgMembership` ağacının altına
//     koyup istisna yazmak, o ağacın tek satırlık varsayımını ("buraya gelen
//     her istek doğrulanmış") delmek olurdu — 1.4'ün kararı tam da bunun
//     tersiydi.
//
//   - Yolun kendisi sözleşme. `/v1/public/...` öneki, bir ucun oturumsuz
//     olduğunu ROUTER'A BAKARAK görünür kılıyor; yeni bir public uç eklemek
//     bilinçli bir hareket (5.4'ün alt alan adı çözümü ve 6.2'nin public
//     kanalı da buraya gelecek).
//
// İzolasyon burada middleware'de DEĞİL, sorgularda: yayımlanmamış bir servisin
// adı hiçbir alandan çıkamıyor (bkz. db/queries/status_pages.sql).

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
)

type publicHandler struct {
	pages  StatusPageService
	logger *slog.Logger
}

// publicPageResponse, ziyaretçinin gördüğü sayfanın tamamı.
type publicPageResponse struct {
	Slug string `json:"slug"`
	// Title ÇÖZÜLMÜŞ geliyor: org özel bir başlık yazmadıysa organizasyonun adı
	// (5.3). Kural sunucuda kalıyor ki her istemci aynı `??` ifadesini yeniden
	// yazmasın.
	Title       string  `json:"title"`
	Description *string `json:"description"`
	LogoURL     *string `json:"logoUrl"`
	// ThemeColor, org'un seçtiği `#rrggbb`; null = varsayılan vurgu rengi.
	// Arayüzde TEK bir CSS değişkenine bağlanıyor, bileşenlere ham değer olarak
	// serpilmiyor (5.3'ün token disiplini).
	ThemeColor *string `json:"themeColor"`
	// ThemeTone, tema renginin ÜZERİNE yazılacak metnin tonu: light | dark.
	// RENK DEĞİL TON dönüyor — istemciye ham bir metin rengi göndermek, arayüzde
	// ham değer yasağını sunucu üzerinden delmek olurdu (bkz. statuspage.Tone).
	ThemeTone string `json:"themeTone"`
	// Groups, servis gruplarının gösterim sırası (5.3).
	Groups []string `json:"groups"`
	// Overall, tek satırlık genel durum: operational | degraded |
	// partial_outage | major_outage | maintenance | unknown. Metne çevirisi
	// istemcide, KARARI sunucuda (bkz. statuspage.Summarize).
	Overall  string                  `json:"overall"`
	Services []publicServiceResponse `json:"services"`
	// StaleServices, ölçümü DURMUŞ yayımlanmış servis sayısı (8.6).
	//
	// Sayfa üstündeki notu besliyor. Neden `overall`a yeni bir değer
	// eklenmediği statuspage.PublicPage'de yazılı: karışık durumda özet
	// `operational` kalıyor ve satır dürüst oluyor ama BAŞLIK olmuyor —
	// bu sayaç tam o boşluğu kapatıyor.
	StaleServices int                         `json:"staleServices"`
	Incidents     []publicIncidentResponse    `json:"incidents"`
	Maintenance   []publicMaintenanceResponse `json:"maintenance"`
}

// publicServiceResponse, yayımlanan bir servis.
//
// URL YOK: ziyaretçinin izlenen adresi bilmesi gerekmiyor (çoğu zaman iç bir
// uç). Normal işleyişte `lastCheckedAt` de yok — hangi sıklıkta kontrol
// ettiğimiz iç bilgi (bkz. `StaleSince`).
type publicServiceResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// StatusChangedAt, "ne zamandır bu durumda".
	StatusChangedAt time.Time `json:"statusChangedAt"`
	// InMaintenance true ise servis ŞU AN planlı bakımda (4.3).
	InMaintenance bool `json:"inMaintenance"`
	// Stale true ise bu servisin ÖLÇÜMÜ DURMUŞ (8.6): `status` son BİLİNEN
	// durumu taşıyor ama artık güncel değil.
	Stale bool `json:"stale"`
	// StaleSince, ölçümün durduğu an — YALNIZCA `stale` true iken dolu.
	//
	// 5.1 `lastCheckedAt`i public sözleşmeden bilinçli olarak çıkarmıştı:
	// "hangi sıklıkta kontrol ediyoruz" iç bilgi. O gerekçe BURADA GEÇERSİZ
	// ve bu yüzden alan koşullu: bayat bir servis tanımı gereği kendi
	// aralığında kontrol EDİLMİYOR, yani bu zaman damgası kontrol sıklığı
	// hakkında hiçbir şey söylemiyor — yalnızca "ne zamandır ölçmüyoruz"
	// diyor, ki zaten ziyaretçiye söylenmesi gereken şey o.
	//
	// Süreyi metne çevirmek arayüzün işi (4.4'ün "devam eden kesintinin 'şu
	// ana kadar'ı istemcide" bölüşümü).
	StaleSince *time.Time `json:"staleSince"`
	// History, son 90 günün günlük özeti (5.2) — HER ZAMAN tam 90 eleman,
	// verisi olmayan günler dahil (bkz. statuspage.history).
	History []publicUptimeDayResponse `json:"history"`
	// Group null = gruplanmamış (5.3).
	Group *string `json:"group"`
}

// publicUptimeDayResponse, 90 günlük grafikteki tek bir bar (5.2).
type publicUptimeDayResponse struct {
	// Date, UTC takvim günü (YYYY-AA-GG). Zaman damgası DEĞİL: bar bir gün, bir
	// an değil — ve saat taşımak "bu gün hangi saatte başlıyor" sorusunu
	// gereksizce açardı (3.4'ün `bucket_date` kararının aynısı).
	Date string `json:"date"`
	// State: no_data | maintenance | up | partial | down. Yüzdeden AYRI, çünkü
	// arayüz "veri yok" gününü bir null kontrolüyle değil doğrudan ayırt
	// edebilmeli (0.2: veri yok günü yeşil görünemez).
	State string `json:"state"`
	// UptimePercent null = oran YOK (veri yok ya da gün tamamen bakımda).
	UptimePercent     *float64                    `json:"uptimePercent"`
	ScheduledChecks   int32                       `json:"scheduledChecks"`
	MaintenanceChecks int32                       `json:"maintenanceChecks"`
	Incidents         []publicDayIncidentResponse `json:"incidents"`
}

// publicDayIncidentResponse, bar üstünde görünen kesinti özeti.
type publicDayIncidentResponse struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Impact string `json:"impact"`
	IsOpen bool   `json:"isOpen"`
}

// publicIncidentResponse, ziyaretçiye gösterilen kesinti.
//
// `isAutoCreated` YOK: kesintiyi bir alarmın mı yoksa bir mühendisin mi ilan
// ettiği iç işleyiş. Timeline satırlarındaki `isSystem` ise duruyor — orada
// ayrım okuyucunun anladığı bir şey ("otomatik tespit" ↔ "biz yazdık").
type publicIncidentResponse struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Impact     string     `json:"impact"`
	StartedAt  time.Time  `json:"startedAt"`
	ResolvedAt *time.Time `json:"resolvedAt"`
	// DurationSeconds null = kesinti sürüyor (4.2'nin kararı: bitişi olmayan
	// bir sürenin hesabı istemcinin saatiyle yapılır).
	DurationSeconds *int64 `json:"durationSeconds"`
	// Services, YALNIZCA yayımlanan etkilenen servisler.
	Services []publicAffectedResponse `json:"services"`
	Updates  []publicUpdateResponse   `json:"updates"`
}

type publicAffectedResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// publicUpdateResponse, timeline satırı — YAZARSIZ (bkz. statuspage.PublicUpdate).
type publicUpdateResponse struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Body      string    `json:"body"`
	IsSystem  bool      `json:"isSystem"`
	CreatedAt time.Time `json:"createdAt"`
}

// publicMaintenanceResponse, aktif veya yaklaşan bakım penceresi.
//
// "Aktif mi yaklaşan mı" ETİKETİ YOK: 4.3'ün kararı gereği o ayrım
// `startsAt`/`endsAt`ten istemcide türetiliyor (aynı pencere iki yerde farklı
// etiket almasın). Servis satırındaki `inMaintenance` ise sunucuda, çünkü genel
// durum özetini besliyor.
type publicMaintenanceResponse struct {
	ID          string                   `json:"id"`
	Title       string                   `json:"title"`
	Description *string                  `json:"description"`
	StartsAt    time.Time                `json:"startsAt"`
	EndsAt      time.Time                `json:"endsAt"`
	Services    []publicAffectedResponse `json:"services"`
}

// status — GET /v1/public/status/{slug}  (KİMLİK DOĞRULAMASI YOK)
//
// Yayımlanmamış sayfa ile olmayan slug AYNI cevabı alıyor (404): fark üretmek,
// denenen slug'lardan hangi organizasyonların kayıtlı olduğunu dışarıya
// söylemek olurdu.
func (h *publicHandler) status(w http.ResponseWriter, r *http.Request) {
	page, err := h.pages.Public(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		if errors.Is(err, statuspage.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir status page yok")
			return
		}

		writeInternalError(w, r, h.logger, "status page okunamadı", err)

		return
	}

	writeJSON(w, http.StatusOK, newPublicPageResponse(page))
}

func newPublicPageResponse(page statuspage.PublicPage) publicPageResponse {
	groups := page.Groups
	if groups == nil {
		groups = []string{}
	}

	response := publicPageResponse{
		Slug:          page.Slug,
		Title:         page.Title,
		Description:   page.Description,
		LogoURL:       page.LogoURL,
		ThemeColor:    page.ThemeColor,
		ThemeTone:     string(page.ThemeTone),
		Groups:        groups,
		Overall:       string(page.Overall),
		StaleServices: page.StaleServices,
		Services:      make([]publicServiceResponse, 0, len(page.Services)),
		Incidents:     make([]publicIncidentResponse, 0, len(page.Incidents)),
		Maintenance:   make([]publicMaintenanceResponse, 0, len(page.Maintenance)),
	}

	for _, service := range page.Services {
		// `staleSince` YALNIZCA bayat servislerde dolu (bkz. tip yorumu):
		// sağlıklı bir servis için son kontrol anını yayımlamak, 5.1'in
		// bilinçli olarak gizlediği kontrol sıklığını ele verirdi.
		var staleSince *time.Time
		if service.Stale {
			staleSince = service.LastCheckedAt
		}

		response.Services = append(response.Services, publicServiceResponse{
			ID:              service.ID.String(),
			Name:            service.Name,
			Status:          string(service.Status),
			StatusChangedAt: service.StatusChangedAt,
			InMaintenance:   service.InMaintenance,
			Stale:           service.Stale,
			StaleSince:      staleSince,
			History:         newPublicHistoryResponse(service.History),
			Group:           service.Group,
		})
	}

	for _, incident := range page.Incidents {
		response.Incidents = append(response.Incidents, newPublicIncidentResponse(incident))
	}

	for _, window := range page.Maintenance {
		response.Maintenance = append(response.Maintenance, publicMaintenanceResponse{
			ID:          window.ID.String(),
			Title:       window.Title,
			Description: window.Description,
			StartsAt:    window.StartsAt,
			EndsAt:      window.EndsAt,
			Services:    newPublicAffectedResponses(window.Services),
		})
	}

	return response
}

func newPublicIncidentResponse(incident statuspage.PublicIncident) publicIncidentResponse {
	response := publicIncidentResponse{
		ID:         incident.ID.String(),
		Title:      incident.Title,
		Status:     string(incident.Status),
		Impact:     string(incident.Impact),
		StartedAt:  incident.StartedAt,
		ResolvedAt: incident.ResolvedAt,
		Services:   newPublicAffectedResponses(incident.Services),
		Updates:    make([]publicUpdateResponse, 0, len(incident.Updates)),
	}

	if incident.ResolvedAt != nil {
		seconds := int64(incident.ResolvedAt.Sub(incident.StartedAt).Seconds())
		response.DurationSeconds = &seconds
	}

	for _, update := range incident.Updates {
		response.Updates = append(response.Updates, publicUpdateResponse{
			ID:        update.ID.String(),
			Status:    string(update.Status),
			Body:      update.Body,
			IsSystem:  update.IsSystem,
			CreatedAt: update.CreatedAt,
		})
	}

	return response
}

// newPublicHistoryResponse, 90 günlük diziyi sözleşmeye çevirir.
//
// Tarih `time.DateOnly` ile yazılıyor, RFC 3339 ile değil: istemcinin
// "2026-08-13T00:00:00Z" değerini kendi saat diliminde bir GÜN ÖNCESİ olarak
// göstermesi, grafiği bir gün kaydırırdı.
func newPublicHistoryResponse(days []statuspage.UptimeDay) []publicUptimeDayResponse {
	response := make([]publicUptimeDayResponse, 0, len(days))

	for _, day := range days {
		entry := publicUptimeDayResponse{
			Date:              day.Date.Format(time.DateOnly),
			State:             string(day.State),
			UptimePercent:     day.UptimePercent,
			ScheduledChecks:   day.ScheduledChecks,
			MaintenanceChecks: day.MaintenanceChecks,
			Incidents:         make([]publicDayIncidentResponse, 0, len(day.Incidents)),
		}

		for _, incident := range day.Incidents {
			entry.Incidents = append(entry.Incidents, publicDayIncidentResponse{
				ID:     incident.ID.String(),
				Title:  incident.Title,
				Impact: string(incident.Impact),
				IsOpen: incident.IsOpen,
			})
		}

		response = append(response, entry)
	}

	return response
}

func newPublicAffectedResponses(services []statuspage.PublicAffected) []publicAffectedResponse {
	response := make([]publicAffectedResponse, 0, len(services))
	for _, service := range services {
		response = append(response, publicAffectedResponse{
			ID:     service.ID.String(),
			Name:   service.Name,
			Status: string(service.Status),
		})
	}

	return response
}
