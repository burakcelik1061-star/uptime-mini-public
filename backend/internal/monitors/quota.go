package monitors

// PLAN KOTASI (Faz 8.5).
//
// Kota YAZMA ANINDA uygulanıyor, okuma anında değil: bir organizasyon planını
// düşürdüğünde (ya da bir plandaki sayı değiştiğinde) mevcut monitor'leri
// SİLİNMİYOR ve çalışmaya devam ediyor. Geriye dönük uygulamak, kullanıcının
// verisini bir yapılandırma değişikliğiyle yok etmek olurdu — 4.3'ün "bakım
// penceresi geçmişi değiştiremez" ve 3.4'ün "kapanmış kovalar yeniden
// hesaplanmaz" kararlarıyla aynı yön. Sınır, YENİ bir şey eklemeye çalışan
// kişiyi durduruyor.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrMonitorLimitReached, organizasyon plan sınırındaki monitor sayısına
// ulaştığında döner.
type ErrMonitorLimitReached struct {
	Plan  store.OrgPlan
	Limit int
}

func (e *ErrMonitorLimitReached) Error() string {
	return fmt.Sprintf("%s planda en fazla %d monitor tanımlanabilir",
		plans.Label(e.Plan), e.Limit)
}

// ErrIntervalBelowPlan, kontrol aralığı planın alt sınırının altındaysa döner.
type ErrIntervalBelowPlan struct {
	Plan    store.OrgPlan
	Minimum int32
}

func (e *ErrIntervalBelowPlan) Error() string {
	return fmt.Sprintf("%s planda kontrol aralığı en az %d saniye olabilir",
		plans.Label(e.Plan), e.Minimum)
}

// Beginner, transaction başlatabilen bağlantı havuzu.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// checkIntervalAgainstPlan, aralığın plana uygunluğunu denetler.
//
// AYRI BİR FONKSİYON ve saf: hem oluşturmada hem güncellemede aynı kural
// çalışıyor. Yalnızca oluşturmada denetlenseydi, FREE bir kullanıcı 300
// saniyeyle monitor açıp hemen ardından 30 saniyeye çekerek kotayı
// atlatabilirdi (2.1'in "kısmi güncelleme de BÜTÜNÜ doğrular" kararının
// aynı gerekçesi).
func checkIntervalAgainstPlan(plan store.OrgPlan, interval int32) error {
	minimum := plans.For(plan).MinCheckIntervalSeconds
	if interval < minimum {
		return &ErrIntervalBelowPlan{Plan: plan, Minimum: minimum}
	}

	return nil
}

// withQuota, organizasyon satırını kilitleyip kota kontrollerini çalıştırır
// ve ardından `write`i AYNI transaction'da koşar.
//
// KİLİT ŞART: "önce say, sınırın altındaysa yaz" iki eşzamanlı istekte
// ikisini de geçirir ve kotanın üstüne çıkardı. 1.3/4.1/6.3'te bu yarışın
// hakemi UNIQUE kısıtıydı; bir SAYIM kısıtla ifade edilemediği için burada
// hakem satır kilidi (bkz. LockOrganizationForQuota).
func (s *Service) withQuota(
	ctx context.Context,
	orgID uuid.UUID,
	interval int32,
	write func(q *store.Queries, plan store.OrgPlan) error,
) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	org, err := q.LockOrganizationForQuota(ctx, orgID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Organizasyon yok: üyelik middleware'i (1.4) buraya gelen isteği
			// zaten doğrulamış olmalı, yani bu yalnızca org silinirken
			// yarışan bir istekte olabilir.
			return ErrNotFound
		}

		return fmt.Errorf("organizasyon kilitlenemedi: %w", err)
	}

	if err := checkIntervalAgainstPlan(org.Plan, interval); err != nil {
		return err
	}

	count, err := q.CountMonitorsForOrg(ctx, orgID)
	if err != nil {
		return fmt.Errorf("monitor sayısı okunamadı: %w", err)
	}

	limit := plans.For(org.Plan).MaxMonitors
	if int(count) >= limit {
		return &ErrMonitorLimitReached{Plan: org.Plan, Limit: limit}
	}

	if err := write(q, org.Plan); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("monitor kaydedilemedi: %w", err)
	}

	return nil
}
