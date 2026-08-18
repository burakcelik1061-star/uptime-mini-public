package statuspage

// 90 GÜNLÜK UPTIME GEÇMİŞİ (Faz 5.2).
//
// Veri kaynağı 3.4'ün GÜNLÜK ÖZETLERİ (`uptime_rollup_daily`), ham
// `check_results` değil — ve bu bir optimizasyon değil tasarımın kendisi
// (0.1 §6): ham veri 30 gün sonra budanıyor, yani 90 günün ilk 60'ı yalnızca
// özetlerde yaşıyor.
//
// Uptime yüzdesi BURADA hesaplanıyor (sorguda değil), çünkü hesabın kendisi
// 0.1'de bağlayıcı ve 4.3'te dört parçaya çıktı:
//
//	uptime % = (success + degraded) / (total - maintenance)
//
// ve en önemli kural bir bölme değil bir AYRIM: `total = 0` olan gün "veri yok"
// (null), %100 DEĞİL. 5.2'nin açık şartı bu ayrımın grafikte görünmesi.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// pgDate, takvim günü parametrelerini sorgunun beklediği tipe çevirir.
//
// `uptime_rollup_daily.bucket_date` bir `date` ve sqlc onu `pgtype.Date` olarak
// üretiyor (sqlc.yaml'da `date` için override yok — zaman damgalarının aksine
// takvim günü gerçekten farklı bir tip ve o farkın görünmesi doğru).
func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t.UTC(), Valid: true}
}

// HistoryDays, grafiğin kaç gün geriye baktığı (0.2/5.2: Instatus tarzı 90 gün).
const HistoryDays = 90

// DayState, tek bir günün bar üzerindeki hâli.
//
// Yüzdeden AYRI bir alan: arayüz "hangi deseni çizeceğim" sorusunu bir eşik
// hesabıyla değil doğrudan cevaplayabilmeli, ve "veri yok" ile "%100" ayrımı
// null kontrolüne bırakılmamalı (0.2'nin şartı: veri yok günü yeşil görünemez).
type DayState string

const (
	// DayStateNoData — o gün hiç ZAMANLANMIŞ kontrol yok. Ne iyi ne kötü:
	// bilmiyoruz. (Monitor o gün henüz yoktu, duraklatılmıştı ya da worker
	// çalışmıyordu.)
	DayStateNoData DayState = "no_data"
	// DayStateMaintenance — günün TAMAMI planlı bakımda. Uptime null: 4.3'ün
	// formülünde payda `total - maintenance` = 0.
	DayStateMaintenance DayState = "maintenance"
	// DayStateUp — %100.
	DayStateUp DayState = "up"
	// DayStatePartial — 0 < uptime < 100.
	DayStatePartial DayState = "partial"
	// DayStateDown — %0: o gün alınan hiçbir ölçüm başarılı değil.
	DayStateDown DayState = "down"
)

// UptimeDay, grafikteki tek bir bar.
type UptimeDay struct {
	// Date, UTC takvim günü (rollup'ın `bucket_date`i ile aynı tanım).
	Date  time.Time
	State DayState
	// UptimePercent nil = o gün için oran YOK (veri yok ya da gün tamamen
	// bakımda). Sıfır ile karıştırılmaması şart.
	UptimePercent *float64
	// ScheduledChecks / MaintenanceChecks, ham sayılar: arayüz "12 kontrolün
	// 3'ü bakımda" gibi bir cümle kurabilsin.
	ScheduledChecks   int32
	MaintenanceChecks int32
	// HasMaintenance, günün BİR KISMI bakımda mıydı. `State` ayrı bir değer
	// çünkü kısmi bakım günü hâlâ bir uptime oranı taşıyor — bakım deseni o
	// oranın ÜZERİNE geliyor, onun yerine değil.
	HasMaintenance bool
	// Incidents, o güne düşen kesintiler (bar üstünde gösteriliyor).
	Incidents []DayIncident
}

// DayIncident, bar üstünde görünen kesinti özeti.
type DayIncident struct {
	ID     uuid.UUID
	Title  string
	Impact store.IncidentImpact
	IsOpen bool
}

// history, yayımlanan her servis için son `HistoryDays` günü hazırlar.
//
// GÜNLER SUNUCUDA DOLDURULUYOR: sorgu yalnızca VERİSİ OLAN günleri döndürüyor,
// yani eksik günler cevapta hiç yok. Boşlukları istemciye doldurtmak, "veri
// yok" gününü üretme sorumluluğunu her istemciye ayrı ayrı vermek olurdu — ve
// o günü yanlışlıkla atlayan bir istemci, 90 günü olduğundan kısa ve daha
// iyi gösterirdi.
// `services` ÇAĞIRANDAN geliyor, burada yeniden okunmuyor: aynı istek içinde
// iki kez okumak hem boşuna bir sorgu hem de ayrışabilen iki liste demekti
// (cevabın servisleri ile grafiğin servisleri).
func (s *Service) history(
	ctx context.Context, orgID, pageID uuid.UUID,
	services []store.ListPublicServicesRow, now time.Time,
) (map[uuid.UUID][]UptimeDay, error) {
	today := now.UTC().Truncate(24 * time.Hour)
	since := today.AddDate(0, 0, -(HistoryDays - 1))

	rows, err := s.queries.ListPublicDailyUptime(ctx, store.ListPublicDailyUptimeParams{
		StatusPageID: pageID,
		SinceDate:    pgDate(since),
	})
	if err != nil {
		return nil, fmt.Errorf("günlük uptime özetleri okunamadı: %w", err)
	}

	incidentRows, err := s.queries.ListPublicIncidentDays(ctx, store.ListPublicIncidentDaysParams{
		StatusPageID: pageID,
		SinceDate:    pgDate(since),
		OrgID:        orgID,
	})
	if err != nil {
		return nil, fmt.Errorf("kesinti günleri okunamadı: %w", err)
	}

	// (monitor, gün) → özet ve kesintiler.
	summaries := make(map[dayKey]store.ListPublicDailyUptimeRow, len(rows))
	for _, row := range rows {
		summaries[dayKey{row.MonitorID, row.BucketDate.Time.UTC()}] = row
	}

	incidents := make(map[dayKey][]DayIncident, len(incidentRows))
	for _, row := range incidentRows {
		key := dayKey{row.MonitorID, row.Day.Time.UTC()}
		incidents[key] = append(incidents[key], DayIncident{
			ID:     row.IncidentID,
			Title:  row.Title,
			Impact: row.Impact,
			IsOpen: row.IsOpen,
		})
	}

	// Hangi servisler için dizi üreteceğimizi ÖZETLERDEN değil, sayfanın
	// yayımlanan servislerinden biliyoruz: hiç ölçümü olmayan bir servis de
	// 90 boş bar göstermeli.
	histories := make(map[uuid.UUID][]UptimeDay, len(services))
	for _, service := range services {
		days := make([]UptimeDay, 0, HistoryDays)

		for offset := range HistoryDays {
			date := since.AddDate(0, 0, offset)
			key := dayKey{service.MonitorID, date}

			day := newUptimeDay(date, summaries[key])
			day.Incidents = incidents[key]

			days = append(days, day)
		}

		histories[service.MonitorID] = days
	}

	return histories, nil
}

// dayKey, (monitor, gün) çifti.
type dayKey struct {
	monitorID uuid.UUID
	date      time.Time
}

// newUptimeDay, tek bir günün özetini bara çevirir.
//
// Sıfır değerli `row` = o gün için hiç özet satırı yok (map'ten eksik anahtar)
// ve sonucu `total = 0` ile aynı: veri yok. İkisini ayırmanın bir anlamı
// olmazdı — 3.4 kontrol üretmemiş bir kova için satır da yazmıyor.
func newUptimeDay(date time.Time, row store.ListPublicDailyUptimeRow) UptimeDay {
	day := UptimeDay{
		Date:              date,
		State:             DayStateNoData,
		ScheduledChecks:   row.TotalChecks,
		MaintenanceChecks: row.MaintenanceChecks,
		HasMaintenance:    row.MaintenanceChecks > 0,
	}

	if row.TotalChecks == 0 {
		return day
	}

	// 4.3'ün paydası. Günün TAMAMI bakımdaysa payda 0: oran yok, ama "veri yok"
	// da değil — ne olduğunu biliyoruz, planlı bakımdı.
	denominator := row.TotalChecks - row.MaintenanceChecks
	if denominator <= 0 {
		day.State = DayStateMaintenance

		return day
	}

	percent := float64(row.SuccessChecks+row.DegradedChecks) / float64(denominator) * 100

	// DEGRADED "up" sayılıyor (0.1'in bağlayıcı kararı): servis yanıt veriyor.
	day.UptimePercent = &percent

	switch {
	case percent >= 100:
		day.State = DayStateUp
	case percent <= 0:
		day.State = DayStateDown
	default:
		// UYDURULMUŞ EŞİK YOK (ör. "%99'un altı kötü"): eşik, o günü hangi
		// tarafa yazacağına dair sessiz bir yargı olurdu. Kural sade: o gün
		// başarısız ölçüm ALINDIYSA bar bunu gösteriyor, ne kadar olduğunu ise
		// yüzdenin kendisi söylüyor.
		day.State = DayStatePartial
	}

	return day
}
