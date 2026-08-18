// Package results, kontrol sonuçlarının ZAMAN SERİSİNİ yönetir: yazma ve
// okuma.
//
// İki paket, iki sorumluluk (karıştırmamak için):
//
//	internal/checker → kontrolü ÇALIŞTIRIR (HTTP isteği, sınıflandırma)
//	internal/results → sonucu SAKLAR ve okur (check_results tablosu)
//
// Ayrı olmalarının pratik sebebi: 3.2'nin worker'ı ikisini de kullanacak ama
// 2.2'nin manuel ucu bir dönem yalnızca ilkini kullandı; 3.4'ün toplulaştırma
// job'ı ise yalnızca ikincisini kullanacak.
package results

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// MaxListLimit, tek seferde okunabilecek satır sayısının üst sınırı.
//
// Sınırsız bırakmak, 30 günlük ham veriyi (bir monitor için ~43 bin satır)
// tek istekte belleğe almak demek olurdu.
const MaxListLimit = 200

// DefaultListLimit, istemci bir sınır vermediğinde kullanılır.
const DefaultListLimit = 50

// Store, bu servisin veritabanından ihtiyaç duyduğu sorgular.
type Store interface {
	InsertCheckResult(ctx context.Context, arg store.InsertCheckResultParams) (store.CheckResult, error)
	ListRecentChecks(ctx context.Context, arg store.ListRecentChecksParams) ([]store.CheckResult, error)
	ListChecksInRange(ctx context.Context, arg store.ListChecksInRangeParams) ([]store.CheckResult, error)
	SummarizeChecksInRange(
		ctx context.Context, arg store.SummarizeChecksInRangeParams,
	) (store.SummarizeChecksInRangeRow, error)
}

// Service, kontrol sonuçlarının saklanması ve okunması.
type Service struct {
	queries Store
	logger  *slog.Logger
}

// NewService, servisi kurar.
func NewService(queries Store, logger *slog.Logger) *Service {
	return &Service{queries: queries, logger: logger}
}

// Record, bir kontrol sonucunu zaman serisine yazar.
//
// `trigger` sonucun KAYNAĞI: zamanlanmış kontrol (3.2) mü, kullanıcının
// tetiklediği manuel kontrol (2.2) mü. Ayrım satırda duruyor çünkü
// toplulaştırma (3.4) ve durum makinesi (3.3) manuel kontrolleri farklı ele
// alacak — düğmeye üst üste basmak ne uptime yüzdesini bozmalı ne de
// monitor'ü DOWN'a çekmeli.
func (s *Service) Record(
	ctx context.Context,
	monitorID uuid.UUID,
	trigger store.CheckTrigger,
	result checker.Result,
) (store.CheckResult, error) {
	params := store.InsertCheckResultParams{
		MonitorID:   monitorID,
		CheckedAt:   result.CheckedAt,
		Outcome:     result.Outcome,
		TriggeredBy: trigger,
	}

	// Cevap alınamadıysa status ve süre NULL kalıyor: 0 yazmak "sunucu 0 ms'de
	// 0 döndü" gibi okunurdu.
	if result.HTTPStatus > 0 {
		status := result.HTTPStatus
		params.HttpStatus = &status
	}

	// Cevap ALINAN her kontrolde süre yazılıyor — SIFIR olsa bile. "1 ms'den
	// kısa sürdü" gerçek bir ölçüm ve şemadaki tutarlılık kısıtı zaten
	// başarılı/yavaş satırda süre şart koşuyor: koşul `> 0` olsaydı, yerel
	// bir hedefe yapılan çok hızlı bir kontrol (özellikle Windows'un kaba
	// saat çözünürlüğünde) NULL süreyle gelip satırı reddettirirdi.
	// Başarısız kontrolde ise ölçüm ancak gerçekten yapıldıysa yazılıyor.
	if result.Outcome != store.CheckOutcomeFailure || result.ResponseTime > 0 {
		elapsed := int32(result.ResponseTime.Milliseconds()) //nolint:gosec // zaman aşımı üst sınırı 60 sn
		params.ResponseTimeMs = &elapsed
	}

	errorType := result.ErrorType

	// SAVUNMA: başarısız bir sonuç hata tipi olmadan gelirse şemadaki
	// tutarlılık kısıtı satırı reddeder ve kullanıcı ölçümü kaybederdi.
	// Veriyi kaybetmek yerine `unknown` yazılıyor, ama sessizce değil —
	// bu satırın loglanması checker'da bir hata olduğunun işareti.
	if result.Outcome == store.CheckOutcomeFailure && errorType == "" {
		s.logger.Error("hata tipi olmayan başarısız kontrol sonucu",
			slog.String("monitor_id", monitorID.String()))

		errorType = store.CheckErrorTypeUnknown
	}

	if errorType != "" {
		params.ErrorType = store.NullCheckErrorType{CheckErrorType: errorType, Valid: true}
	}

	// Hata mesajı yalnızca başarısız sonuçlarda anlamlı; şemadaki kısıt da
	// bunu şart koşuyor (success/degraded satırında hata alanları boş).
	if result.Outcome == store.CheckOutcomeFailure && result.ErrorMessage != "" {
		message := result.ErrorMessage
		params.ErrorMessage = &message
	}

	row, err := s.queries.InsertCheckResult(ctx, params)
	if err != nil {
		return store.CheckResult{}, fmt.Errorf("kontrol sonucu kaydedilemedi: %w", err)
	}

	return row, nil
}

// ListRecent, bir monitor'ün son kontrollerini (en yeniden geriye) döner.
//
// orgID sorguya GİRİYOR: monitor'ün organizasyonu tek doğruluk kaynağı olarak
// `monitors` tablosunda duruyor ve okuma oradan doğrulanıyor. Başka bir
// organizasyonun monitor kimliği verilirse sonuç BOŞ döner.
func (s *Service) ListRecent(
	ctx context.Context, orgID, monitorID uuid.UUID, limit int32,
) ([]store.CheckResult, error) {
	rows, err := s.queries.ListRecentChecks(ctx, store.ListRecentChecksParams{
		MonitorID: monitorID,
		OrgID:     orgID,
		RowLimit:  clampLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("kontroller okunamadı: %w", err)
	}

	return rows, nil
}

// ListInRange, bir zaman aralığındaki kontrolleri döner ([from, to)).
//
// Aralık YARI AÇIK: ardışık pencereler (saatlik/günlük toplulaştırma, 3.4)
// aynı satırı iki kez saymasın.
func (s *Service) ListInRange(
	ctx context.Context, orgID, monitorID uuid.UUID, from, to time.Time, limit int32,
) ([]store.CheckResult, error) {
	rows, err := s.queries.ListChecksInRange(ctx, store.ListChecksInRangeParams{
		MonitorID: monitorID,
		OrgID:     orgID,
		FromTime:  from,
		ToTime:    to,
		RowLimit:  clampLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("kontroller okunamadı: %w", err)
	}

	return rows, nil
}

// Summary, bir zaman aralığındaki kontrollerin özeti (2.4).
type Summary struct {
	From time.Time
	To   time.Time
	// ZAMANLANMIŞ kontrollerin sayıları — uptime hesabının tabanı.
	ScheduledChecks int64
	SuccessChecks   int64
	DegradedChecks  int64
	FailureChecks   int64
	// MaintenanceChecks, PLANLI BAKIM penceresine düşen zamanlanmış kontroller
	// (4.3). `ScheduledChecks`in içinde ama success/degraded/failure
	// sayılarının DIŞINDA: uptime'ın paydasından düşülüyor, sayısı ise
	// saklanıyor ("o gün planlı bakım vardı" bilgisi kaybolmasın — 0.1'in
	// bağlayıcı kararı).
	MaintenanceChecks int64
	// ManualChecks yalnızca bilgi: uptime'a GİRMİYOR (bkz. Summarize).
	ManualChecks int64

	responseTimeTotalMs int64
	responseTimeSamples int64
}

// UptimePercent, 0.1'in BAĞLAYICI uptime tanımını uygular:
//
//	(success + degraded) / (total - maintenance)
//
// DEGRADED "up" sayılıyor: servis cevap veriyor, yavaşlık ayrı bir metrik.
// BAKIM penceresine düşen kontroller (4.3) paydadan düşülüyor ve paya da
// girmiyor — planlı bir kesinti uptime'ı düşürmemeli, ama planlı bir bakımda
// alınan başarılı ölçümler de oranı yukarı çekmemeli.
//
// HİÇ KONTROL YOKSA nil — %100 DEĞİL. "Veri yok" ile "kesintisiz" aynı şey
// değil; 5.2'nin "veri olmayan gün yeşil görünmesin" şartı ancak bu ayrım
// veri modelinden arayüze kadar korunursa mümkün. Aralığın TAMAMI bakımdaysa
// da cevap nil: ölçülmüş bir uptime yok.
func (s Summary) UptimePercent() *float64 {
	denominator := s.ScheduledChecks - s.MaintenanceChecks
	if denominator <= 0 {
		return nil
	}

	percent := float64(s.SuccessChecks+s.DegradedChecks) / float64(denominator) * 100

	return &percent
}

// AvgResponseMs, ölçümü olan kontrollerin ortalaması (örnek yoksa nil).
//
// Cevap alınamayan kontrollerin süresi NULL ve ortalamaya girmiyor: onları
// 0 saymak, kesinti sırasında ortalamanın DÜŞMESİ gibi bir sonuç verirdi.
func (s Summary) AvgResponseMs() *float64 {
	if s.responseTimeSamples == 0 {
		return nil
	}

	average := float64(s.responseTimeTotalMs) / float64(s.responseTimeSamples)

	return &average
}

// Summarize, aralıktaki kontrolleri özetler ([from, to)).
func (s *Service) Summarize(
	ctx context.Context, orgID, monitorID uuid.UUID, from, to time.Time,
) (Summary, error) {
	row, err := s.queries.SummarizeChecksInRange(ctx, store.SummarizeChecksInRangeParams{
		MonitorID: monitorID,
		OrgID:     orgID,
		FromTime:  from,
		ToTime:    to,
	})
	if err != nil {
		return Summary{}, fmt.Errorf("kontrol özeti okunamadı: %w", err)
	}

	return Summary{
		From:                from,
		To:                  to,
		ScheduledChecks:     row.ScheduledChecks,
		SuccessChecks:       row.SuccessChecks,
		DegradedChecks:      row.DegradedChecks,
		FailureChecks:       row.FailureChecks,
		MaintenanceChecks:   row.MaintenanceChecks,
		ManualChecks:        row.ManualChecks,
		responseTimeTotalMs: row.ResponseTimeTotalMs,
		responseTimeSamples: row.ResponseTimeSamples,
	}, nil
}

// clampLimit, sınırı geçerli aralığa çeker.
func clampLimit(limit int32) int32 {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	default:
		return limit
	}
}
