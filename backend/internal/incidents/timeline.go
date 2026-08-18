package incidents

// Incident'ın ORG-SCOPED yüzü (Faz 4.2): okuma ve timeline'a yazma.
//
// service.go'daki yarı worker'ın (otomatik açma/kapama), burası api'nin
// kullandığı yarı. İkisi aynı pakette çünkü aynı kurallara tabiler — en
// görüneni şu: kapanmış bir incident'a hiçbir yoldan yazılamıyor.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Liste sınırları (2.3'ün `results` paketiyle aynı desen: sınır sunucunun
// kendini korumasıdır, istemcinin uyması gereken bir kural değil — daha
// fazlasını isteyen istemci sessizce kırpılmış bir liste alır).
const (
	MaxListLimit     = 200
	DefaultListLimit = 50
)

// MaxUpdateBodyLength, timeline metninin üst sınırı (şemadaki CHECK ile aynı).
const MaxUpdateBodyLength = 2000

// ErrNotFound, incident yoksa VEYA başka bir organizasyona aitse döner
// (2.1'in `monitors.ErrNotFound` kararıyla aynı: iki durum tek hataya
// indirgeniyor ki "var ama senin değil" bilgisi hiç üretilmesin).
var ErrNotFound = errors.New("incident bulunamadı")

// ErrResolved, kapanmış bir incident'a güncelleme eklenmeye çalışıldığında
// döner.
//
// Yeniden açma BİLİNÇLİ OLARAK YOK: kapanmış bir kesintinin geçmişi geriye
// dönük değişmemeli (status page'de yayımlanmış bir "çözüldü" satırı yalan
// olmamalı). Yeni bir sorun yeni bir incident.
var ErrResolved = errors.New("incident kapanmış")

// Problem / ValidationError, alan bazlı doğrulama sonucu.
//
// `monitors` paketindeki ikizinden bilinçli olarak BAĞIMSIZ: iki paket
// birbirini tanımıyor ve HTTP'yi hiçbiri tanımıyor; çeviriyi handler yapıyor.
type Problem struct {
	Field   string
	Message string
}

// ValidationError, kabul edilemez alanları taşır.
type ValidationError struct {
	Problems []Problem
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.Field+": "+p.Message)
	}

	return "incident güncellemesi doğrulanamadı (" + strings.Join(parts, ", ") + ")"
}

// Filter, liste ucunun açık/kapalı süzgeci.
type Filter string

const (
	// FilterAll, açık ve kapanmış incident'ların hepsi.
	FilterAll Filter = "all"
	// FilterOpen, yalnızca devam eden kesintiler.
	FilterOpen Filter = "open"
	// FilterResolved, yalnızca kapanmışlar.
	FilterResolved Filter = "resolved"
)

// ParseFilter, sorgu parametresini süzgece çevirir.
//
// Tanınmayan değer sessizce "hepsi"ne düşMÜYOR: `?status=opne` yazan istemci,
// istediğini aldığını sanmamalı.
func ParseFilter(raw string) (Filter, bool) {
	switch Filter(strings.TrimSpace(raw)) {
	case "":
		return FilterAll, true
	case FilterAll:
		return FilterAll, true
	case FilterOpen:
		return FilterOpen, true
	case FilterResolved:
		return FilterResolved, true
	default:
		return FilterAll, false
	}
}

// AffectedMonitor, incident'ın etkilediği bir servis.
type AffectedMonitor struct {
	ID     uuid.UUID
	Name   string
	Status store.MonitorStatus
}

// ListItem, liste ucunun tek bir satırı.
type ListItem struct {
	Incident store.Incident
	Monitors []AffectedMonitor
}

// Detail, incident detayı: kayıt + etkilenen servisler + TIMELINE.
type Detail struct {
	Incident store.Incident
	Monitors []AffectedMonitor
	Updates  []store.IncidentUpdate
}

// UpdateInput, timeline'a eklenecek güncelleme.
type UpdateInput struct {
	AuthorID uuid.UUID
	Status   store.IncidentStatus
	Body     string
}

// List, organizasyonun incident'larını döndürür (yeniden eskiye).
func (s *Service) List(ctx context.Context, orgID uuid.UUID, filter Filter, limit int32) ([]ListItem, error) {
	rows, err := s.queries.ListIncidentsForOrg(ctx, store.ListIncidentsForOrgParams{
		OrgID:        orgID,
		OpenOnly:     filter == FilterOpen,
		ResolvedOnly: filter == FilterResolved,
		RowLimit:     clampLimit(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("incident'lar okunamadı: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	// Etkilenen servisler TEK sorguda (N+1 yok).
	affected, err := s.affectedMonitors(ctx, orgID, ids)
	if err != nil {
		return nil, err
	}

	items := make([]ListItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, ListItem{Incident: row, Monitors: affected[row.ID]})
	}

	return items, nil
}

// Get, tek bir incident'ı timeline'ıyla birlikte döndürür.
func (s *Service) Get(ctx context.Context, orgID, incidentID uuid.UUID) (Detail, error) {
	incident, err := s.queries.GetIncidentForOrg(ctx, store.GetIncidentForOrgParams{
		IncidentID: incidentID,
		OrgID:      orgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Detail{}, ErrNotFound
		}

		return Detail{}, fmt.Errorf("incident okunamadı: %w", err)
	}

	affected, err := s.affectedMonitors(ctx, orgID, []uuid.UUID{incidentID})
	if err != nil {
		return Detail{}, err
	}

	updates, err := s.queries.ListIncidentUpdates(ctx, store.ListIncidentUpdatesParams{
		IncidentID: incidentID,
		OrgID:      orgID,
	})
	if err != nil {
		return Detail{}, fmt.Errorf("incident güncellemeleri okunamadı: %w", err)
	}

	return Detail{Incident: incident, Monitors: affected[incidentID], Updates: updates}, nil
}

// AddUpdate, timeline'a bir güncelleme ekler ve incident'ın durumunu ona taşır.
//
// Akış "önce oku, sonra yaz" ama YARIŞ AÇIK DEĞİL: yazma sorgusu da
// `resolved_at IS NULL` koşulunu taşıyor (ve satırı kilitliyor). Okuma,
// istemciye 404 ile 409 arasındaki farkı söyleyebilmek için — tek sorguda
// "hiç satır dönmedi" iki durumu ayırt edemezdi.
func (s *Service) AddUpdate(
	ctx context.Context, orgID, incidentID uuid.UUID, in UpdateInput,
) (store.IncidentUpdate, error) {
	in = NormalizeUpdate(in)
	if err := ValidateUpdate(in); err != nil {
		return store.IncidentUpdate{}, err
	}

	incident, err := s.queries.GetIncidentForOrg(ctx, store.GetIncidentForOrgParams{
		IncidentID: incidentID,
		OrgID:      orgID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.IncidentUpdate{}, ErrNotFound
		}

		return store.IncidentUpdate{}, fmt.Errorf("incident okunamadı: %w", err)
	}

	if incident.ResolvedAt != nil {
		return store.IncidentUpdate{}, ErrResolved
	}

	row, err := s.queries.AddIncidentUpdate(ctx, store.AddIncidentUpdateParams{
		IncidentID: incidentID,
		OrgID:      orgID,
		AuthorID:   in.AuthorID,
		Status:     in.Status,
		Body:       in.Body,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Okuma ile yazma arasında kapanmış (ör. worker kesintinin bittiğini
			// gördü) ya da silinmiş.
			return store.IncidentUpdate{}, ErrResolved
		}

		return store.IncidentUpdate{}, fmt.Errorf("incident güncellemesi eklenemedi: %w", err)
	}

	s.logger.Info("incident güncellemesi eklendi",
		slog.String("incident_id", incidentID.String()),
		slog.String("org_id", orgID.String()),
		slog.String("author_id", in.AuthorID.String()),
		slog.String("status", string(in.Status)))

	// Sorgu `inserted.*` döndürdüğü için sqlc ayrı bir satır tipi üretti;
	// alanlar birebir aynı olduğundan tip dönüşümü yeterli.
	return store.IncidentUpdate(row), nil
}

// affectedMonitors, incident kimliklerine göre etkilenen servisleri gruplar.
func (s *Service) affectedMonitors(
	ctx context.Context, orgID uuid.UUID, incidentIDs []uuid.UUID,
) (map[uuid.UUID][]AffectedMonitor, error) {
	grouped := make(map[uuid.UUID][]AffectedMonitor, len(incidentIDs))
	if len(incidentIDs) == 0 {
		return grouped, nil
	}

	rows, err := s.queries.ListIncidentMonitors(ctx, store.ListIncidentMonitorsParams{
		OrgID:       orgID,
		IncidentIds: incidentIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("etkilenen servisler okunamadı: %w", err)
	}

	for _, row := range rows {
		grouped[row.IncidentID] = append(grouped[row.IncidentID], AffectedMonitor{
			ID:     row.MonitorID,
			Name:   row.Name,
			Status: row.CurrentStatus,
		})
	}

	return grouped, nil
}

// NormalizeUpdate, girdiyi saklama biçimine getirir (`monitors.Normalize` ile
// aynı desen): doğrulama HER ZAMAN normalleştirilmiş değeri görmeli, yoksa
// yalnızca boşluktan oluşan bir gövde kuralı geçip veritabanına giderdi.
func NormalizeUpdate(in UpdateInput) UpdateInput {
	in.Body = strings.TrimSpace(in.Body)

	return in
}

// ValidateUpdate, timeline girdisini denetler.
//
// Doğrulama SERVİSTE, handler'da değil (2.1'in kuralı): manuel incident
// oluşturma (4.4) da ilk güncellemeyi buradan geçirecek. Dışa açık olması
// `monitors.Validate` ile aynı sebeple: HTTP katmanının testleri kuralların
// KENDİSİNİ değil, kuralların 422'ye çevrimini ölçmeli.
func ValidateUpdate(in UpdateInput) error {
	var problems []Problem

	switch in.Status {
	case store.IncidentStatusInvestigating, store.IncidentStatusIdentified,
		store.IncidentStatusMonitoring, store.IncidentStatusResolved:
	default:
		// Boş dize de dahil: tanınmayan değer sessizce "investigating"e
		// düşmüyor — istemci ne ilan ettiğini bilmeli.
		problems = append(problems, Problem{
			Field:   "status",
			Message: "investigating, identified, monitoring veya resolved olmalı",
		})
	}

	switch {
	case in.Body == "":
		// Boş bir timeline satırı okuyucuya hiçbir şey anlatmaz; durumu
		// değiştirmek isteyen kullanıcı da bir cümle yazabilir.
		problems = append(problems, Problem{Field: "body", Message: "zorunlu"})
	case utf8.RuneCountInString(in.Body) > MaxUpdateBodyLength:
		problems = append(problems, Problem{
			Field:   "body",
			Message: fmt.Sprintf("en fazla %d karakter olabilir", MaxUpdateBodyLength),
		})
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}

	return nil
}

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
