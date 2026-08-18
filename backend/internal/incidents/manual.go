package incidents

// Manuel incident oluşturma (Faz 4.4).
//
// Otomatik incident'tan iki farkı var ve ikisi de 0.1'de yazılıydı:
// BİRDEN FAZLA servisi kapsayabiliyor ("veritabanı bakımı API'yi ve web'i
// düşürdü") ve etkisini kullanıcı seçiyor (sistem bir servisin iş açısından
// önemini bilmiyor).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// MaxTitleLength, incident başlığının üst sınırı (şemadaki CHECK ile aynı).
const MaxTitleLength = 200

// MaxMonitorsPerIncident, tek bir incident'ın kapsayabileceği servis sayısı.
const MaxMonitorsPerIncident = 100

// ManualInput, elle açılan bir incident'ın alanları.
type ManualInput struct {
	Title      string
	Impact     store.IncidentImpact
	MonitorIDs []uuid.UUID
	// AuthorID ve Body, timeline'ın İLK satırı. Boş bir incident okuyucuya
	// hiçbir şey anlatmaz: "ne oluyor" cümlesi kaydın kendisiyle birlikte
	// doğmalı.
	AuthorID uuid.UUID
	Body     string
}

// CreateManual, elle bir incident açar.
//
// ÜÇ YAZMA TEK TRANSACTION'DA: incident + kapsadığı servisler + ilk timeline
// satırı. Ayrı ayrı yapılsaydı aradaki bir hata "hiçbir servise bağlı
// olmayan" ya da "hiç anlatısı olmayan" bir kayıt bırakırdı (1.3'ün "sahipsiz
// organizasyon olmasın" kararının aynısı).
//
// Servisler yalnızca AYNI ORGANİZASYONDAN bağlanıyor ve yazılan satır sayısı
// istenenle karşılaştırılıyor; eksikse işlem tümden geri alınıyor — sessizce
// eksik kapsayan bir incident, status page'de yanlış servisi "çalışıyor"
// göstermek demekti.
func (s *Service) CreateManual(ctx context.Context, orgID uuid.UUID, in ManualInput) (Detail, error) {
	in = NormalizeManual(in)

	if err := ValidateManual(NormalizeManual(in)); err != nil {
		return Detail{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Detail{}, fmt.Errorf("transaction başlatılamadı: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := store.New(tx)

	incident, err := queries.CreateIncident(ctx, store.CreateIncidentParams{
		OrgID:  orgID,
		Title:  in.Title,
		Impact: in.Impact,
	})
	if err != nil {
		return Detail{}, fmt.Errorf("incident oluşturulamadı: %w", err)
	}

	linked, err := queries.AddIncidentMonitors(ctx, store.AddIncidentMonitorsParams{
		IncidentID: incident.ID,
		MonitorIds: in.MonitorIDs,
		OrgID:      orgID,
	})
	if err != nil {
		return Detail{}, fmt.Errorf("incident'a servisler bağlanamadı: %w", err)
	}

	if int(linked) != len(in.MonitorIDs) {
		return Detail{}, &ValidationError{Problems: []Problem{{
			Field:   "monitorIds",
			Message: "bu organizasyona ait olmayan veya bulunmayan servis var",
		}}}
	}

	// İlk timeline satırı, güncelleme eklemenin AYNI sorgusundan geçiyor:
	// "durum ve timeline tek yazmada" kuralının (4.2) ikinci bir kopyası
	// doğmasın.
	if _, err := queries.AddIncidentUpdate(ctx, store.AddIncidentUpdateParams{
		IncidentID: incident.ID,
		OrgID:      orgID,
		AuthorID:   in.AuthorID,
		Status:     store.IncidentStatusInvestigating,
		Body:       in.Body,
	}); err != nil {
		return Detail{}, fmt.Errorf("ilk incident güncellemesi yazılamadı: %w", err)
	}

	monitors, err := queries.ListIncidentMonitors(ctx, store.ListIncidentMonitorsParams{
		OrgID:       orgID,
		IncidentIds: []uuid.UUID{incident.ID},
	})
	if err != nil {
		return Detail{}, fmt.Errorf("etkilenen servisler okunamadı: %w", err)
	}

	updates, err := queries.ListIncidentUpdates(ctx, store.ListIncidentUpdatesParams{
		IncidentID: incident.ID,
		OrgID:      orgID,
	})
	if err != nil {
		return Detail{}, fmt.Errorf("incident güncellemeleri okunamadı: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Detail{}, fmt.Errorf("incident kaydedilemedi: %w", err)
	}

	s.logger.Info("manuel incident açıldı",
		slog.String("incident_id", incident.ID.String()),
		slog.String("org_id", orgID.String()),
		slog.String("author_id", in.AuthorID.String()),
		slog.String("impact", string(in.Impact)),
		slog.Int("monitors", len(monitors)))

	affected := make([]AffectedMonitor, 0, len(monitors))
	for _, row := range monitors {
		affected = append(affected, AffectedMonitor{ID: row.MonitorID, Name: row.Name, Status: row.CurrentStatus})
	}

	return Detail{Incident: incident, Monitors: affected, Updates: updates}, nil
}

// NormalizeManual, girdiyi saklama biçimine getirir.
func NormalizeManual(in ManualInput) ManualInput {
	in.Title = strings.TrimSpace(in.Title)
	in.Body = NormalizeUpdate(UpdateInput{Body: in.Body}).Body

	return in
}

// ValidateManual, manuel incident alanlarını denetler.
//
// Dışa açık (monitors.Validate ile aynı gerekçe): HTTP katmanının testleri
// kuralların KENDİSİNİ değil, kuralların 422'ye çevrimini ölçmeli.
func ValidateManual(in ManualInput) error {
	var problems []Problem

	switch {
	case in.Title == "":
		problems = append(problems, Problem{Field: "title", Message: "zorunlu"})
	case utf8.RuneCountInString(in.Title) > MaxTitleLength:
		problems = append(problems, Problem{
			Field:   "title",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxTitleLength),
		})
	}

	switch in.Impact {
	case store.IncidentImpactNone, store.IncidentImpactMinor,
		store.IncidentImpactMajor, store.IncidentImpactCritical:
	default:
		// Tanınmayan değer sessizce bir varsayılana düşmüyor: etkiyi kullanıcı
		// ilan ediyor, sistem uydurmuyor.
		problems = append(problems, Problem{
			Field:   "impact",
			Message: "none, minor, major veya critical olmalı",
		})
	}

	problems = append(problems, validateManualMonitors(in.MonitorIDs)...)

	// Gövde kuralları güncelleme ile AYNI (tek kural seti): ilk timeline satırı
	// da bir timeline satırı.
	var validation *ValidationError
	if err := ValidateUpdate(UpdateInput{
		Status: store.IncidentStatusInvestigating,
		Body:   in.Body,
	}); errors.As(err, &validation) {
		problems = append(problems, validation.Problems...)
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

func validateManualMonitors(ids []uuid.UUID) []Problem {
	switch {
	case len(ids) == 0:
		// Hiçbir servisi kapsamayan incident status page'de hangi servisin
		// etkilendiğini söyleyemez.
		return []Problem{{Field: "monitorIds", Message: "en az bir servis seçilmeli"}}
	case len(ids) > MaxMonitorsPerIncident:
		return []Problem{{
			Field:   "monitorIds",
			Message: fmt.Sprintf("en fazla %d servis seçilebilir", MaxMonitorsPerIncident),
		}}
	}

	seen := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return []Problem{{Field: "monitorIds", Message: "aynı servis birden fazla kez gönderilemez"}}
		}
		seen[id] = struct{}{}
	}

	return nil
}
