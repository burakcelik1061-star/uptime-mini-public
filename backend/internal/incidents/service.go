// Package incidents, kesinti KAYITLARINI yönetir (Faz 4).
//
// Sorumluluk sınırı (bu projede paketler bilinçli olarak dar):
//
//	internal/monitors   → NextState: bir kontrol sonucu durumu nasıl değiştirir (3.3)
//	internal/scheduler  → kontrolü çalıştırır, sonucu ve durumu yazar (3.2)
//	internal/incidents  → DURUM DEĞİŞİMİNİN kesinti defterine yansıması (4.1)
//
// Bu paket DURUMU YENİDEN HESAPLAMIYOR: girdisi scheduler'ın elindeki
// `monitors.Transition`. "Şu monitor down mu" sorusunun ikinci bir cevabı
// olsaydı (ör. burada ham kontrol satırlarına bakmak), iki cevap er ya da geç
// ayrışır ve incident'lar monitor'ün durumuyla çelişirdi.
package incidents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// AutoImpact, otomatik açılan incident'ın etkisi.
//
// `major`, `critical` DEĞİL: kritik olup olmadığı servisin iş açısından
// önemine bağlı ve bunu sistem bilmiyor — kullanıcı 4.4'te değiştirebilecek.
// `minor` de olmazdı: hiç cevap vermeyen bir servis küçük bir sorun değil.
const AutoImpact = store.IncidentImpactMajor

// autoTitleSuffix, otomatik incident başlığının eki.
//
// Başlık uzunluğu şemada 200 karakterle sınırlı; monitor adı en fazla 120
// karakter (2.1) olduğu için burada kırpma gerekmiyor.
const autoTitleSuffix = " yanıt vermiyor"

// Action, bir durum değişiminin incident defterindeki karşılığı.
//
// Log ve testler için: "hiçbir şey yapılmadı" ile "zaten açıktı" aynı şey
// değil — ikincisi 4.1'in asıl garantisinin (aynı kesinti için ikinci incident
// açılmaz) çalıştığının kanıtı.
type Action string

const (
	// ActionNone, durum değişimi incident üretmiyor (ör. pending → up).
	ActionNone Action = "none"
	// ActionOpened, yeni bir otomatik incident açıldı.
	ActionOpened Action = "opened"
	// ActionAlreadyOpen, bu monitor için açık bir incident zaten vardı.
	ActionAlreadyOpen Action = "already_open"
	// ActionResolved, açık incident kapatıldı.
	ActionResolved Action = "resolved"
	// ActionNoOpenIncident, kapatılacak açık incident bulunamadı.
	ActionNoOpenIncident Action = "no_open_incident"
	// ActionSuppressed, monitor PLANLI BAKIMDA olduğu için incident açılmadı
	// (4.3). "Hiçbir şey yapılmadı" ile aynı değil: kesinti gerçek, ama
	// beklenen bir kesinti.
	ActionSuppressed Action = "suppressed_by_maintenance"
)

// Otomatik güncellemelerin (4.2) timeline'a yazdığı metinler.
//
// Sistem satırları da kullanıcı satırlarıyla aynı tablodadır: "bu incident'ı
// ne açtı, ne zaman kapandı" sorusunun cevabı geçmişin İÇİNDE durmalı,
// yalnızca worker logunda değil.
const (
	autoOpenBody    = "Otomatik kontroller yanıt alamıyor: kesinti tespit edildi."
	autoResolveBody = "Otomatik kontroller yeniden yanıt alıyor: servis normale döndü."
)

// Store, bu servisin veritabanından ihtiyaç duyduğu sorgular.
type Store interface {
	OpenAutoIncident(ctx context.Context, arg store.OpenAutoIncidentParams) (store.OpenAutoIncidentRow, error)
	ResolveAutoIncident(
		ctx context.Context, arg store.ResolveAutoIncidentParams,
	) (store.ResolveAutoIncidentRow, error)
	ListIncidentsForOrg(ctx context.Context, arg store.ListIncidentsForOrgParams) ([]store.Incident, error)
	GetIncidentForOrg(ctx context.Context, arg store.GetIncidentForOrgParams) (store.Incident, error)
	ListIncidentMonitors(
		ctx context.Context, arg store.ListIncidentMonitorsParams,
	) ([]store.ListIncidentMonitorsRow, error)
	ListIncidentUpdates(ctx context.Context, arg store.ListIncidentUpdatesParams) ([]store.IncidentUpdate, error)
	AddIncidentUpdate(ctx context.Context, arg store.AddIncidentUpdateParams) (store.AddIncidentUpdateRow, error)
	// MonitorInMaintenanceAt, 4.3'ün kapısı (bkz. db/queries/maintenance.sql).
	MonitorInMaintenanceAt(ctx context.Context, arg store.MonitorInMaintenanceAtParams) (bool, error)
}

// Service, incident defteri.
//
// Havuz, MANUEL incident oluşturmanın (4.4) tek transaction'ı için: incident +
// kapsadığı servisler + ilk timeline satırı birlikte yazılıyor. Otomatik
// açma/kapama tek ifadelik olduğu için havuza ihtiyaç duymuyor — worker'da da
// aynı servis kuruluyor.
type Service struct {
	pool    *pgxpool.Pool
	queries Store
	logger  *slog.Logger
}

// NewService, servisi kurar.
func NewService(pool *pgxpool.Pool, queries Store, logger *slog.Logger) *Service {
	return &Service{pool: pool, queries: queries, logger: logger}
}

// ApplyTransition, bir durum değişimini incident defterine yansıtır.
//
// KURALLAR (yalnızca durum GERÇEKTEN değiştiyse çalışıyor — 3.3'ün
// `Transition.Changed`'i):
//
//	… → down     : otomatik incident aç (zaten açıksa hiçbir şey yapma)
//	down → up|degraded : açık otomatik incident'ı kapat
//	diğer        : hiçbir şey (ör. up → degraded "yavaşladı" demek, kesinti değil)
//
// DEGRADED KESİNTİ DEĞİL (3.3'ün asimetrisinin devamı): servis cevap veriyor,
// yavaşlık ayrı bir metrik. `degraded` için incident açmak, uptime formülünün
// (0.1) degraded'ı "up" saymasıyla da çelişirdi.
//
// `checkedAt` KONTROLÜN zamanı, `now()` değil: kesintinin sınırları ölçümlere
// dayanmalı, yazma anına değil.
func (s *Service) ApplyTransition(
	ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
) (Action, error) {
	if !transition.Changed {
		return ActionNone, nil
	}

	switch {
	case transition.Next.Status == store.MonitorStatusDown:
		return s.open(ctx, monitor, transition, checkedAt)

	case transition.From == store.MonitorStatusDown:
		// Buraya yalnızca `up`/`degraded` ile gelinir: `paused`a geçiş bu
		// yoldan değil PATCH ucundan oluyor (ve orada incident bilinçli olarak
		// AÇIK bırakılıyor — bkz. docs/4.1).
		return s.resolve(ctx, monitor, transition, checkedAt)

	default:
		return ActionNone, nil
	}
}

// open, otomatik incident açar.
//
// PLANLI BAKIM KAPISI (4.3) BURADA, kapanışta DEĞİL: pencere "beklenen bir
// kesinti" ilan ediyor, yani alarm üretilmiyor. Kapanış tarafına konsaydı,
// bakım başlamadan önce açılmış gerçek bir kesinti bakım bittiğinde de açık
// kalırdı.
//
// Kontrol AYRI BİR SORGU (açma ifadesinin içine gömülmedi): "hiç satır
// dönmedi" cevabı o zaman "zaten açık" ile "bakımda" arasında ayrım
// yapamazdı — ikisi de sağlıklı sonuçlar ama farklı olaylar. Araya sıkışan
// bir pencere değişikliği en fazla bir turluk fark yaratır; DOWN geçişi
// zaten nadir.
func (s *Service) open(
	ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
) (Action, error) {
	inMaintenance, err := s.queries.MonitorInMaintenanceAt(ctx, store.MonitorInMaintenanceAtParams{
		MonitorID: monitor.ID,
		At:        checkedAt,
	})
	if err != nil {
		return ActionNone, fmt.Errorf("bakım penceresi okunamadı: %w", err)
	}

	if inMaintenance {
		// Info: kesinti gerçek, yalnızca beklenen. Sessiz kalmak, "neden
		// incident açılmadı" sorusunu cevapsız bırakırdı.
		s.logger.Info("planlı bakım: otomatik incident açılmadı",
			slog.String("monitor_id", monitor.ID.String()),
			slog.String("name", monitor.Name),
			slog.Time("checked_at", checkedAt))

		return ActionSuppressed, nil
	}

	incident, err := s.queries.OpenAutoIncident(ctx, store.OpenAutoIncidentParams{
		MonitorID: monitor.ID,
		CheckedAt: checkedAt,
		// Kesintinin başlangıcı için geriye kaç kontrol bakılacağı: ardışık
		// hata sayacının kendisi (bkz. db/queries/incidents.sql).
		StreakLength: streakLength(transition.Next.ConsecutiveFailures),
		Title:        AutoTitle(monitor.Name),
		Impact:       AutoImpact,
		UpdateBody:   autoOpenBody,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Kısmi unique indeks devrede: bu monitor'ün açık bir incident'ı
			// zaten var. Beklenen ve sağlıklı bir sonuç, hata değil.
			s.logger.Debug("otomatik incident zaten açık",
				slog.String("monitor_id", monitor.ID.String()))

			return ActionAlreadyOpen, nil
		}

		return ActionNone, fmt.Errorf("otomatik incident açılamadı: %w", err)
	}

	s.logger.Info("otomatik incident açıldı",
		slog.String("incident_id", incident.ID.String()),
		slog.String("monitor_id", monitor.ID.String()),
		slog.String("title", incident.Title),
		slog.Time("started_at", incident.StartedAt),
		// Kesintinin ne kadarı eşik beklenirken geçti: 3.3'ün eşiklerinin
		// gerçek maliyeti burada görünüyor.
		slog.Duration("detection_delay", checkedAt.Sub(incident.StartedAt)))

	return ActionOpened, nil
}

// resolve, açık otomatik incident'ı kapatır.
func (s *Service) resolve(
	ctx context.Context, monitor store.Monitor, transition monitors.Transition, checkedAt time.Time,
) (Action, error) {
	incident, err := s.queries.ResolveAutoIncident(ctx, store.ResolveAutoIncidentParams{
		MonitorID:    monitor.ID,
		CheckedAt:    checkedAt,
		StreakLength: streakLength(transition.Next.ConsecutiveSuccesses),
		UpdateBody:   autoResolveBody,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Açık incident yok: elle kapatılmış (4.2) ya da bu sürümden önce
			// başlamış bir kesinti olabilir. Monitor'ün durumu doğru, defterde
			// kapatılacak bir şey yok.
			s.logger.Debug("kapatılacak açık otomatik incident yok",
				slog.String("monitor_id", monitor.ID.String()))

			return ActionNoOpenIncident, nil
		}

		return ActionNone, fmt.Errorf("otomatik incident kapatılamadı: %w", err)
	}

	var duration time.Duration
	if incident.ResolvedAt != nil {
		duration = incident.ResolvedAt.Sub(incident.StartedAt)
	}

	s.logger.Info("otomatik incident kapatıldı",
		slog.String("incident_id", incident.ID.String()),
		slog.String("monitor_id", monitor.ID.String()),
		slog.Time("started_at", incident.StartedAt),
		slog.Any("resolved_at", incident.ResolvedAt),
		// Kesintinin GERÇEK süresi (3.4'ün yaklaşık `downtime_seconds`'ının
		// aksine): başlangıç ve bitiş iki gerçek ölçümden geliyor.
		slog.Duration("downtime", duration))

	return ActionResolved, nil
}

// AutoTitle, otomatik incident'ın başlığı.
func AutoTitle(monitorName string) string {
	return monitorName + autoTitleSuffix
}

// streakLength, seri uzunluğunu güvenli aralığa çeker.
//
// Sayaç şemada ve durum makinesinde zaten pozitif; 0 veya negatif bir değer
// sorguyu `LIMIT 0` ile boş döndürür ve kesinti başlangıcı sessizce tespit
// anına kayardı. En az bir kontrol her zaman var: az önce yazılan.
func streakLength(counter int32) int32 {
	if counter < 1 {
		return 1
	}

	return counter
}
