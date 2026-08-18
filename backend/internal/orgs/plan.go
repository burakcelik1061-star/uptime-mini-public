package orgs

// PLAN YÖNETİMİ (Faz 8.5).
//
// NEDEN BU PAKETTE: plan organizasyonun bir sütunu (migration 00021) ve bu
// paket organizasyon satırının sahibi. Ayrı bir "billing servisi" paketi,
// organizasyonu iki yerden yazmak demekti.
//
// `internal/billing` ise yalnızca ÖDEME ADIMINI soyutluyor (mock) — planın
// veritabanına yazılması burada.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/billing"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// ErrSamePlan, organizasyon zaten istenen plandaysa döner.
var ErrSamePlan = errors.New("organizasyon zaten bu planda")

// PlanUsage, planın sınırları ve organizasyonun bugünkü kullanımı.
//
// KULLANIM DA DÖNÜYOR: "5 monitor sınırı var" tek başına eksik bilgi,
// kullanıcının bilmek istediği "kaçını kullandım". Arayüzün bunu iki ayrı
// istekten birleştirmesi (plan ucu + monitor listesi) gereksiz.
type PlanUsage struct {
	Plan   store.OrgPlan
	Limits plans.Limits
	// Monitors, organizasyonun şu anki monitor sayısı.
	Monitors int64
}

// PlanFor, organizasyonun planını, sınırlarını ve kullanımını döner.
func (s *Service) PlanFor(ctx context.Context, membership Membership) (PlanUsage, error) {
	count, err := s.queries.CountMonitorsForOrg(ctx, membership.OrgID)
	if err != nil {
		return PlanUsage{}, fmt.Errorf("monitor sayısı okunamadı: %w", err)
	}

	return PlanUsage{
		Plan:     membership.OrgPlan,
		Limits:   plans.For(membership.OrgPlan),
		Monitors: count,
	}, nil
}

// ChangePlan, ödeme adımını çalıştırıp organizasyonun planını değiştirir.
//
// ÖDEME ÖNCE, YAZMA SONRA: sağlayıcı reddederse plan değişmiyor. Ters sırada
// (önce planı yaz, sonra tahsil et) başarısız bir ödemede kullanıcı ücretsiz
// olarak yükselmiş olurdu ve geri almak ikinci bir yazma yolu isterdi.
//
// TEK TRANSACTION DEĞİL ve olamaz: ödeme DIŞ bir sistemde, veritabanı
// transaction'ının içine alınamaz. Aradaki pencere yazılı: tahsilat başarılı
// olup plan yazması patlarsa kullanıcı ödediği planı ALAMAZ — bu, 6.3'ün
// "gönderdikten sonra deftere yazma patlarsa" penceresinin aynısı ve orada
// olduğu gibi burada da kayıt loga düşüyor (dekont referansıyla), çünkü
// alternatifi — önce planı vermek — daha kötü.
func (s *Service) ChangePlan(
	ctx context.Context, membership Membership, target store.OrgPlan,
) (store.Organization, error) {
	if membership.OrgPlan == target {
		return store.Organization{}, ErrSamePlan
	}

	receipt, err := s.billing.Charge(ctx, billing.Charge{
		OrgID:   membership.OrgID,
		OrgName: membership.OrgName,
		Plan:    target,
	})
	if err != nil {
		return store.Organization{}, fmt.Errorf("ödeme alınamadı: %w", err)
	}

	org, err := s.queries.UpdateOrganizationPlan(ctx, store.UpdateOrganizationPlanParams{
		ID:   membership.OrgID,
		Plan: target,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Organization{}, ErrNoAccess
		}

		// Tahsilat yapıldı ama plan yazılamadı: dekont referansı loga
		// düşüyor ki elle telafi edilebilsin.
		s.logger.Error("ödeme alındı ama plan yazılamadı",
			slog.String("org_id", membership.OrgID.String()),
			slog.String("reference", receipt.Reference),
			slog.String("error", err.Error()))

		return store.Organization{}, fmt.Errorf("plan güncellenemedi: %w", err)
	}

	s.logger.Info("plan değişti",
		slog.String("org_id", org.ID.String()),
		slog.String("plan", string(org.Plan)),
		slog.String("reference", receipt.Reference))

	return org, nil
}
