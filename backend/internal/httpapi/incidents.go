package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// IncidentService, incident uçlarının ihtiyaç duyduğu davranışlar
// (bkz. internal/incidents).
type IncidentService interface {
	List(ctx context.Context, orgID uuid.UUID, filter incidents.Filter, limit int32) ([]incidents.ListItem, error)
	Get(ctx context.Context, orgID, incidentID uuid.UUID) (incidents.Detail, error)
	AddUpdate(
		ctx context.Context, orgID, incidentID uuid.UUID, in incidents.UpdateInput,
	) (store.IncidentUpdate, error)
	CreateManual(ctx context.Context, orgID uuid.UUID, in incidents.ManualInput) (incidents.Detail, error)
}

type incidentHandler struct {
	incidents IncidentService
	// broadcaster, manuel incident hareketlerini canlı yayına duyurur (6.2).
	// Otomatik incident'ların duyurusu worker'da, durum değişiminin geçtiği
	// tek noktada (bkz. internal/scheduler).
	broadcaster Broadcaster
	logger      *slog.Logger
}

// affectedMonitorIDs, incident'ın kapsadığı servislerin kimlikleri.
//
// PUBLIC KANALIN KAPISI BUNA BAKIYOR: 5.1'in kuralı gereği bir incident,
// kapsadığı servislerden en az biri yayımlanmışsa ziyaretçiye görünüyor.
// Aynı liste kapıya veriliyor ki canlı yayın ile sayfanın gördüğü ayrışmasın.
func affectedMonitorIDs(monitors []incidents.AffectedMonitor) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(monitors))
	for _, monitor := range monitors {
		ids = append(ids, monitor.ID)
	}

	return ids
}

// incidentResponse, incident'ın DIŞA VERİLEN gösterimi.
//
// `autoMonitorId` sözleşmede YOK: o sütun 4.1'in kısmi unique indeksinin
// dayanağı, yani bir İÇ mekanizma. İstemcinin ihtiyacı olan iki şey zaten
// var — kaydın otomatik doğup doğmadığı (`isAutoCreated`) ve hangi servisleri
// etkilediği (`monitors`).
type incidentResponse struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Impact string `json:"impact"`
	// IsAutoCreated true ise incident'ı durum makinesi açtı (4.1).
	IsAutoCreated bool      `json:"isAutoCreated"`
	StartedAt     time.Time `json:"startedAt"`
	// ResolvedAt null = kesinti DEVAM EDİYOR.
	ResolvedAt *time.Time `json:"resolvedAt"`
	// DurationSeconds null = kesinti sürüyor (istemci "şu ana kadar" hesabını
	// kendisi yapar; sunucunun uydurduğu bir "şu an" istemcininkiyle
	// çakışmasın).
	DurationSeconds *int64                    `json:"durationSeconds"`
	Monitors        []affectedMonitorResponse `json:"monitors"`
	CreatedAt       time.Time                 `json:"createdAt"`
	UpdatedAt       time.Time                 `json:"updatedAt"`
}

// affectedMonitorResponse, incident'ın etkilediği servis.
type affectedMonitorResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Status monitor'ün ŞU ANKİ durumu (geçmişteki değil): "hâlâ down mu?"
	// sorusunun cevabı incident detayında bir tık uzakta olmasın.
	Status string `json:"status"`
}

// incidentUpdateResponse, timeline'ın tek bir satırı.
type incidentUpdateResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Body   string `json:"body"`
	// IsSystem true = satırı 4.1'in otomatik açma/kapaması yazdı.
	IsSystem bool `json:"isSystem"`
	// AuthorID null olabilir: sistem satırı ya da kullanıcısı silinmiş satır.
	AuthorID  *string   `json:"authorId"`
	CreatedAt time.Time `json:"createdAt"`
}

func newIncidentResponse(item incidents.ListItem) incidentResponse {
	incident := item.Incident

	response := incidentResponse{
		ID:            incident.ID.String(),
		Title:         incident.Title,
		Status:        string(incident.Status),
		Impact:        string(incident.Impact),
		IsAutoCreated: incident.IsAutoCreated,
		StartedAt:     incident.StartedAt,
		ResolvedAt:    incident.ResolvedAt,
		Monitors:      make([]affectedMonitorResponse, 0, len(item.Monitors)),
		CreatedAt:     incident.CreatedAt,
		UpdatedAt:     incident.UpdatedAt,
	}

	if incident.ResolvedAt != nil {
		// Süre SUNUCUDA hesaplanıyor: iki zaman damgasından çıkarma yapmak
		// istemcinin işi olabilirdi ama o zaman her istemci aynı kararı
		// (saniye mi ms mi, yuvarlama nasıl) yeniden verirdi.
		seconds := int64(incident.ResolvedAt.Sub(incident.StartedAt).Seconds())
		response.DurationSeconds = &seconds
	}

	for _, monitor := range item.Monitors {
		response.Monitors = append(response.Monitors, affectedMonitorResponse{
			ID:     monitor.ID.String(),
			Name:   monitor.Name,
			Status: string(monitor.Status),
		})
	}

	return response
}

func newIncidentUpdateResponse(row store.IncidentUpdate) incidentUpdateResponse {
	response := incidentUpdateResponse{
		ID:        row.ID.String(),
		Status:    string(row.Status),
		Body:      row.Body,
		IsSystem:  row.IsSystem,
		CreatedAt: row.CreatedAt,
	}

	if row.AuthorID != nil {
		authorID := row.AuthorID.String()
		response.AuthorID = &authorID
	}

	return response
}

// list — GET /v1/orgs/{orgID}/incidents  (üyelik yeterli)
//
// Sorgu parametreleri:
//
//	status — open | resolved | all (varsayılan all)
//	limit  — kaç kayıt (varsayılan 50, üst sınır 200)
//
// Okuma üyeye açık: 0.1 §2 `member` rolünü "salt-okunur pano (incident
// geçmişi dahil)" olarak tanımlıyor.
func (h *incidentHandler) list(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()

	v := &validator{}

	filter, valid := incidents.ParseFilter(query.Get("status"))
	if !valid {
		v.fail("status", "open, resolved veya all olmalı")
	}

	limit := parseLimit(v, query.Get("limit"))

	if apiErr := v.err(); apiErr != nil {
		apiErr.status = http.StatusBadRequest
		apiErr.code = codeInvalidQuery
		apiErr.message = "sorgu parametreleri geçersiz"
		apiErr.write(w, r)

		return
	}

	items, err := h.incidents.List(r.Context(), orgID, filter, limit)
	if err != nil {
		writeInternalError(w, r, h.logger, "incident'lar okunamadı", err)
		return
	}

	// Boş liste `null` değil `[]`.
	response := make([]incidentResponse, 0, len(items))
	for _, item := range items {
		response = append(response, newIncidentResponse(item))
	}

	writeJSON(w, http.StatusOK, map[string][]incidentResponse{"incidents": response})
}

// detail — GET /v1/orgs/{orgID}/incidents/{incidentID}  (üyelik yeterli)
//
// Timeline cevabın İÇİNDE dönüyor, ayrı bir uçta değil: bir incident'ın
// anlatısı olmadan detayının anlamı yok, iki isteklik bir ekran için ikinci
// bir gidiş-geliş gereksiz.
func (h *incidentHandler) detail(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	incidentID, ok := incidentIDFrom(w, r)
	if !ok {
		return
	}

	detail, err := h.incidents.Get(r.Context(), orgID, incidentID)
	if err != nil {
		h.writeServiceError(w, r, err, "incident okunamadı")
		return
	}

	writeJSON(w, http.StatusOK, incidentDetailBody(detail))
}

// addUpdate — POST /v1/orgs/{orgID}/incidents/{incidentID}/updates
//
// YETKİ BURADA İKİ KADEMELİ (0.1 §2'nin rol tanımının birebir karşılığı):
//   - `member` timeline'a yazabilir — nöbetçi mühendis senaryosu: kesintiyi
//     gören kişi ne olduğunu yazabilmeli, rol yükseltmesi beklemeden.
//   - Ama incident'ı KAPATMAK (`status: resolved`) yönetim işi: admin+.
//
// Bu yüzden uç `requireOrgRole` grubunun DIŞINDA duruyor ve rol kontrolü
// handler'da, yalnızca kapatma için yapılıyor.
func (h *incidentHandler) addUpdate(w http.ResponseWriter, r *http.Request) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		h.logger.Error("incident güncellemesi kimlik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	incidentID, ok := incidentIDFrom(w, r)
	if !ok {
		return
	}

	var req createIncidentUpdateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	status := store.IncidentStatus(req.Status)

	if status == store.IncidentStatusResolved && !membership.HasAtLeast(store.OrgRoleAdmin) {
		writeError(w, r, http.StatusForbidden, codeForbidden,
			"incident'ı kapatmak için yetkiniz yetersiz")

		return
	}

	update, err := h.incidents.AddUpdate(r.Context(), membership.OrgID, incidentID, incidents.UpdateInput{
		AuthorID: identity.UserID,
		Status:   status,
		Body:     req.Body,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "incident güncellemesi eklenemedi")
		return
	}

	// Güncelleme incident'ın DURUMUNU da değiştiriyor; istemci ikinci bir
	// istek atmadan yeni hâli görsün diye detay da dönüyor.
	detail, err := h.incidents.Get(r.Context(), membership.OrgID, incidentID)
	if err != nil {
		h.writeServiceError(w, r, err, "incident okunamadı")
		return
	}

	// CANLI YAYIN (6.2): yazma başarılı olduktan SONRA duyuruluyor — olayı alan
	// istemci tazelediğinde yeni hâli görsün.
	h.broadcaster.Changed(r.Context(), membership.OrgID,
		affectedMonitorIDs(detail.Monitors), realtime.ReasonIncidentUpdated)

	body := incidentDetailBody(detail)
	body["update"] = newIncidentUpdateResponse(update)

	writeJSON(w, http.StatusCreated, body)
}

// create — POST /v1/orgs/{orgID}/incidents  (admin+)
//
// MANUEL incident: otomatik olanın aksine BİRDEN FAZLA servisi kapsayabiliyor
// ve etkisini kullanıcı seçiyor (sistem bir servisin iş açısından önemini
// bilmiyor).
//
// `startedAt` İSTEMCİDEN ALINMIYOR (sunucuda `now()`): geçmişe tarihlenen bir
// incident, kesinti geçmişini elle yazmanın kapısı olurdu — 4.3'ün "geçmişe
// dönük bakım penceresi yok" kararıyla aynı gerekçe.
func (h *incidentHandler) create(w http.ResponseWriter, r *http.Request) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	identity, ok := auth.IdentityFrom(r.Context())
	if !ok {
		h.logger.Error("incident oluşturma kimlik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return
	}

	var req createIncidentRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	v := &validator{}

	monitorIDs := make([]uuid.UUID, 0, len(req.MonitorIDs))
	for _, raw := range req.MonitorIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.fail("monitorIds", "geçerli bir servis kimliği olmalı")
			break
		}

		monitorIDs = append(monitorIDs, id)
	}

	if apiErr := v.err(); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	detail, err := h.incidents.CreateManual(r.Context(), membership.OrgID, incidents.ManualInput{
		Title:      req.Title,
		Impact:     store.IncidentImpact(req.Impact),
		MonitorIDs: monitorIDs,
		AuthorID:   identity.UserID,
		Body:       req.Body,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "incident oluşturulamadı")
		return
	}

	h.broadcaster.Changed(r.Context(), membership.OrgID,
		affectedMonitorIDs(detail.Monitors), realtime.ReasonIncidentCreated)

	writeJSON(w, http.StatusCreated, incidentDetailBody(detail))
}

// createIncidentRequest, manuel incident'ın gövdesi.
type createIncidentRequest struct {
	Title      string   `json:"title"`
	Impact     string   `json:"impact"`
	MonitorIDs []string `json:"monitorIds"`
	// Body, timeline'ın İLK satırı: "ne oluyor" cümlesi kaydın kendisiyle
	// birlikte doğmalı.
	Body string `json:"body"`
}

// createIncidentUpdateRequest, timeline'a eklenen güncellemenin gövdesi.
type createIncidentUpdateRequest struct {
	Status string `json:"status"`
	Body   string `json:"body"`
}

// incidentDetailBody, detay cevabının gövdesi.
func incidentDetailBody(detail incidents.Detail) map[string]any {
	updates := make([]incidentUpdateResponse, 0, len(detail.Updates))
	for _, row := range detail.Updates {
		updates = append(updates, newIncidentUpdateResponse(row))
	}

	return map[string]any{
		"incident": newIncidentResponse(incidents.ListItem{
			Incident: detail.Incident,
			Monitors: detail.Monitors,
		}),
		"updates": updates,
	}
}

// orgID, doğrulanmış üyelikten organizasyon kimliğini alır (bkz. monitors.go:
// yol parçası DEĞİL, context — middleware'in veritabanına karşı doğruladığı
// değer).
func (h *incidentHandler) orgID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return uuid.UUID{}, false
	}

	return membership.OrgID, true
}

// incidentIDFrom, yol parçasını çözer; biçimsizse 404 (monitorIDFrom ile aynı
// gerekçe: tek cevap, sızdıracak varlık bilgisi yok).
func incidentIDFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	incidentID, err := uuid.Parse(chi.URLParam(r, "incidentID"))
	if err != nil {
		writeIncidentNotFound(w, r)
		return uuid.UUID{}, false
	}

	return incidentID, true
}

// writeServiceError, servis katmanının hatalarını HTTP'ye çevirir.
func (h *incidentHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var validation *incidents.ValidationError
	if errors.As(err, &validation) {
		fields := make([]FieldError, 0, len(validation.Problems))
		for _, p := range validation.Problems {
			fields = append(fields, FieldError{Field: p.Field, Message: p.Message})
		}

		(&apiError{
			status:  http.StatusUnprocessableEntity,
			code:    codeValidationFailed,
			message: "gövde doğrulanamadı",
			fields:  fields,
		}).write(w, r)

		return
	}

	if errors.Is(err, incidents.ErrNotFound) {
		writeIncidentNotFound(w, r)
		return
	}

	if errors.Is(err, incidents.ErrResolved) {
		// 409: istek biçimsel olarak geçerli, kaydın DURUMU elvermiyor.
		// Kapanmış bir incident'a yazmak, yayımlanmış bir geçmişi değiştirmek
		// demek olurdu (bkz. incidents.ErrResolved).
		writeError(w, r, http.StatusConflict, codeIncidentResolved,
			"kapanmış bir incident'a güncelleme eklenemez")

		return
	}

	writeInternalError(w, r, h.logger, logMsg, err)
}

func writeIncidentNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir incident yok")
}
