// Package monitors, izlenen uçların (monitor) yapılandırmasını yönetir:
// doğrulama, oluşturma, güncelleme, silme.
//
// Kontrolün KENDİSİ burada değil: HTTP isteğini atıp sonucu sınıflandırmak
// 2.2'nin, zamanlama 3.2'nin, durum makinesi 3.3'ün işi. Bu paket "monitor
// kaydı ne olabilir" sorusunu cevaplıyor.
package monitors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrNotFound, monitor yoksa VEYA başka bir organizasyona aitse döner.
//
// 1.3'ün ErrNoAccess'iyle aynı gerekçe: iki durum tek hataya indirgeniyor ki
// "var ama senin değil" bilgisi hiç üretilmesin. Sorgular zaten org_id ile
// sınırlı, yani ayrımı yapmak için fazladan bir sorgu gerekirdi — ki o sorgu
// da tam olarak sızdırılmaması gereken cevabı verirdi.
var ErrNotFound = errors.New("monitor bulunamadı")

// SERVİS ARTIK SOMUT `*store.Queries` TAŞIYOR (8.5).
//
// Önceden bir `Store` arayüzü vardı ama tek bir uygulaması oldu ve hiçbir
// test onu sahtelemedi (bu paketin testleri 2.1'den beri GERÇEK veritabanına
// karşı koşuyor — kısıtlar ve kilitler sahteyle ölçülemez). 8.5'in kota
// kontrolü transaction istiyor, yani `WithTx` gerekiyor; arayüze `WithTx`
// eklemek, arayüzü sqlc'nin somut tipine birebir bağlamak ve soyutluğunu
// tamamen yitirmesi demekti. orgs/incidents/maintenance servisleri de zaten
// havuz + somut sorgu katmanı taşıyor.

// Service, monitor işlemleri.
type Service struct {
	// db, kota kontrolünün transaction'ı için (8.5).
	db      Beginner
	queries *store.Queries
	logger  *slog.Logger
}

// NewService, servisi kurar.
func NewService(db Beginner, queries *store.Queries, logger *slog.Logger) *Service {
	return &Service{db: db, queries: queries, logger: logger}
}

// Create, organizasyona yeni bir monitor ekler.
//
// current_status'u KULLANICI seçmiyor: aktif bir monitor ilk kontrolü
// yapılana kadar 'pending', pasif başlatılan monitor 'paused'.
func (s *Service) Create(ctx context.Context, orgID uuid.UUID, in Settings) (store.Monitor, error) {
	in = Normalize(in)
	if err := Validate(in); err != nil {
		return store.Monitor{}, err
	}

	// KOTA KONTROLÜ VE YAZMA AYNI TRANSACTION'DA (8.5): organizasyon satırı
	// kilitleniyor, sayım yapılıyor ve monitor yazılıyor. Kilit olmadan iki
	// eşzamanlı istek kotanın üstüne çıkardı (bkz. quota.go).
	var monitor store.Monitor

	err := s.withQuota(ctx, orgID, in.CheckIntervalSeconds, func(q *store.Queries, _ store.OrgPlan) error {
		created, err := q.CreateMonitor(ctx, store.CreateMonitorParams{
			OrgID:                orgID,
			Name:                 in.Name,
			Url:                  in.URL,
			Method:               in.Method,
			CheckIntervalSeconds: in.CheckIntervalSeconds,
			TimeoutMs:            in.TimeoutMs,
			ExpectedStatusMin:    in.ExpectedStatusMin,
			ExpectedStatusMax:    in.ExpectedStatusMax,
			DegradedResponseMs:   in.DegradedResponseMs,
			FailureThreshold:     in.FailureThreshold,
			RecoveryThreshold:    in.RecoveryThreshold,
			IsActive:             in.IsActive,
			CurrentStatus:        initialStatus(in.IsActive),
		})
		if err != nil {
			return fmt.Errorf("monitor oluşturulamadı: %w", err)
		}

		monitor = created

		return nil
	})
	if err != nil {
		return store.Monitor{}, err
	}

	s.logger.Info("monitor oluşturuldu",
		slog.String("monitor_id", monitor.ID.String()),
		slog.String("org_id", orgID.String()),
		slog.String("url", monitor.Url))

	return monitor, nil
}

// Listing, monitor listesi ve listenin okunduğu AN (8.6).
//
// "Şimdi" listeyle BİRLİKTE dönüyor çünkü bayatlık kararı ona dayanıyor ve o
// an VERİTABANININ saati olmak zorunda (bkz. staleness.go). Çağıranın kendi
// `time.Now()`unu kullanması, `last_checked_at`i yazan worker ile farklı bir
// saate bakmak demekti.
type Listing struct {
	Monitors []store.Monitor
	// Now, veritabanının listeyi döndürdüğü andaki saati.
	Now time.Time
}

// List, organizasyonun monitor'lerini döner.
func (s *Service) List(ctx context.Context, orgID uuid.UUID) (Listing, error) {
	rows, err := s.queries.ListMonitorsForOrg(ctx, orgID)
	if err != nil {
		return Listing{}, fmt.Errorf("monitor'ler okunamadı: %w", err)
	}

	listing := Listing{Monitors: make([]store.Monitor, 0, len(rows))}
	for _, row := range rows {
		listing.Monitors = append(listing.Monitors, row.Monitor)
		listing.Now = row.ServerNow
	}

	return listing, nil
}

// Get, tek bir monitor'ü döner (organizasyonuna ait değilse ErrNotFound).
func (s *Service) Get(ctx context.Context, orgID, monitorID uuid.UUID) (store.Monitor, error) {
	monitor, err := s.queries.GetMonitorForOrg(ctx, store.GetMonitorForOrgParams{
		ID:    monitorID,
		OrgID: orgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Monitor{}, ErrNotFound
		}

		return store.Monitor{}, fmt.Errorf("monitor okunamadı: %w", err)
	}

	return monitor, nil
}

// Update, gönderilen alanları mevcut kaydın üzerine bindirir.
//
// AKIŞ ÖNEMLİ: oku → birleştir → BÜTÜNÜ doğrula → yaz. Kısmi güncellemeyi
// doğrudan SQL'de (COALESCE) yapmak daha az kod olurdu ama alanlar arası
// kurallar (timeout <= aralık, min <= max) o zaman yalnızca şemadaki CHECK
// kısıtına kalırdı: kullanıcı "timeoutMs kontrol aralığından uzun olamaz"
// yerine 500 görürdü.
//
// Kayıp güncelleme (iki eş zamanlı PATCH) bilinçli olarak ele alınmıyor:
// monitor yapılandırması nadiren ve tek kullanıcı tarafından değiştirilen bir
// veri. Satır kilidi gereken yer 3.2'nin claim döngüsü.
func (s *Service) Update(ctx context.Context, orgID, monitorID uuid.UUID, patch Patch) (store.Monitor, error) {
	if patch.IsEmpty() {
		return store.Monitor{}, &ValidationError{Problems: []Problem{{
			Field:   "",
			Message: "en az bir alan gönderilmeli",
		}}}
	}

	current, err := s.Get(ctx, orgID, monitorID)
	if err != nil {
		return store.Monitor{}, err
	}

	merged := Normalize(patch.apply(SettingsOf(current)))
	if err := Validate(merged); err != nil {
		return store.Monitor{}, err
	}

	// PLAN KOTASI GÜNCELLEMEDE DE GEÇERLİ ama YALNIZCA ARALIK (8.5).
	//
	// Sayım YAPILMIYOR: güncelleme yeni kayıt üretmiyor ve kotasını doldurmuş
	// bir organizasyonun mevcut monitor'ünü düzenleyememesi saçma olurdu —
	// sınır YENİ bir şey eklemeyi durduruyor, var olanı yönetmeyi değil.
	//
	// Aralık kontrolü ise ŞART: yalnızca oluşturmada denetlenseydi, FREE bir
	// kullanıcı 300 saniyeyle açıp hemen ardından 30 saniyeye çekerek kotayı
	// atlatırdı (2.1'in "kısmi güncelleme de BÜTÜNÜ doğrular" kararı).
	plan, err := s.queries.GetOrganizationPlan(ctx, orgID)
	if err != nil {
		return store.Monitor{}, fmt.Errorf("organizasyon planı okunamadı: %w", err)
	}
	if err := checkIntervalAgainstPlan(plan, merged.CheckIntervalSeconds); err != nil {
		return store.Monitor{}, err
	}

	monitor, err := s.queries.UpdateMonitor(ctx, store.UpdateMonitorParams{
		ID:                   monitorID,
		OrgID:                orgID,
		Name:                 merged.Name,
		Url:                  merged.URL,
		Method:               merged.Method,
		CheckIntervalSeconds: merged.CheckIntervalSeconds,
		TimeoutMs:            merged.TimeoutMs,
		ExpectedStatusMin:    merged.ExpectedStatusMin,
		ExpectedStatusMax:    merged.ExpectedStatusMax,
		DegradedResponseMs:   merged.DegradedResponseMs,
		FailureThreshold:     merged.FailureThreshold,
		RecoveryThreshold:    merged.RecoveryThreshold,
		IsActive:             merged.IsActive,
		CurrentStatus:        statusAfterUpdate(current, merged.IsActive),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Okuma ile yazma arasında silinmiş olabilir.
			return store.Monitor{}, ErrNotFound
		}

		return store.Monitor{}, fmt.Errorf("monitor güncellenemedi: %w", err)
	}

	s.logger.Info("monitor güncellendi",
		slog.String("monitor_id", monitor.ID.String()),
		slog.String("org_id", orgID.String()))

	return monitor, nil
}

// Delete, monitor'ü siler.
func (s *Service) Delete(ctx context.Context, orgID, monitorID uuid.UUID) error {
	affected, err := s.queries.DeleteMonitor(ctx, store.DeleteMonitorParams{
		ID:    monitorID,
		OrgID: orgID,
	})
	if err != nil {
		return fmt.Errorf("monitor silinemedi: %w", err)
	}

	// Sorgu org_id ile sınırlı: başka organizasyonun monitor'ü de 0 satır
	// etkiler, yani "yok" ile ayırt edilemez. İstenen davranış bu.
	if affected == 0 {
		return ErrNotFound
	}

	s.logger.Info("monitor silindi",
		slog.String("monitor_id", monitorID.String()),
		slog.String("org_id", orgID.String()))

	return nil
}

// SettingsOf, kayıttan kullanıcı tarafından belirlenen alanları çıkarır.
func SettingsOf(m store.Monitor) Settings {
	return Settings{
		Name:                 m.Name,
		URL:                  m.Url,
		Method:               m.Method,
		CheckIntervalSeconds: m.CheckIntervalSeconds,
		TimeoutMs:            m.TimeoutMs,
		ExpectedStatusMin:    m.ExpectedStatusMin,
		ExpectedStatusMax:    m.ExpectedStatusMax,
		DegradedResponseMs:   m.DegradedResponseMs,
		FailureThreshold:     m.FailureThreshold,
		RecoveryThreshold:    m.RecoveryThreshold,
		IsActive:             m.IsActive,
	}
}

func initialStatus(isActive bool) store.MonitorStatus {
	if isActive {
		return store.MonitorStatusPending
	}

	return store.MonitorStatusPaused
}

// statusAfterUpdate, aktiflik DEĞİŞTİYSE denormalize durumu tazeler.
//
// Duraklatılan monitor 'paused' oluyor; yeniden başlatılan monitor eski
// durumuna değil 'pending'e dönüyor — duraklatma süresince dünya değişmiş
// olabilir, "up" demek olmayan bir bilgiyi uydurmak olurdu. Aktiflik
// değişmediyse durum bu adımda ELLENMİYOR: sahibi 3.3'ün durum makinesi.
func statusAfterUpdate(current store.Monitor, isActive bool) store.MonitorStatus {
	if isActive == current.IsActive {
		return current.CurrentStatus
	}

	return initialStatus(isActive)
}
