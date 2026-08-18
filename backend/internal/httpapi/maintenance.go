package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/maintenance"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
)

// MaintenanceService, bakım penceresi uçlarının ihtiyaç duyduğu davranışlar
// (bkz. internal/maintenance).
type MaintenanceService interface {
	Create(ctx context.Context, orgID uuid.UUID, in maintenance.Input) (maintenance.Window, error)
	List(ctx context.Context, orgID uuid.UUID, limit int32) ([]maintenance.Window, error)
	Delete(ctx context.Context, orgID, windowID uuid.UUID) error
}

type maintenanceHandler struct {
	windows MaintenanceService
	logger  *slog.Logger
}

// maintenanceWindowResponse, pencerenin DIŞA VERİLEN gösterimi.
//
// "Aktif / yaklaşan / geçmiş" ETİKETİ SUNUCUDAN GELMİYOR: üçü de
// `startsAt`/`endsAt` ile şimdiden türetilebiliyor. Sunucunun "şu an"ı ile
// istemcininki arasındaki fark, aynı pencerenin listede bir yerde "yaklaşan"
// başka bir yerde "aktif" görünmesi demekti.
type maintenanceWindowResponse struct {
	ID          string                    `json:"id"`
	Title       string                    `json:"title"`
	Description *string                   `json:"description"`
	StartsAt    time.Time                 `json:"startsAt"`
	EndsAt      time.Time                 `json:"endsAt"`
	Monitors    []affectedMonitorResponse `json:"monitors"`
	CreatedAt   time.Time                 `json:"createdAt"`
}

func newMaintenanceWindowResponse(window maintenance.Window) maintenanceWindowResponse {
	response := maintenanceWindowResponse{
		ID:          window.Window.ID.String(),
		Title:       window.Window.Title,
		Description: window.Window.Description,
		StartsAt:    window.Window.StartsAt,
		EndsAt:      window.Window.EndsAt,
		Monitors:    make([]affectedMonitorResponse, 0, len(window.Monitors)),
		CreatedAt:   window.Window.CreatedAt,
	}

	for _, monitor := range window.Monitors {
		response.Monitors = append(response.Monitors, affectedMonitorResponse{
			ID:     monitor.ID.String(),
			Name:   monitor.Name,
			Status: string(monitor.Status),
		})
	}

	return response
}

// createMaintenanceWindowRequest, yeni pencerenin gövdesi.
type createMaintenanceWindowRequest struct {
	Title       string   `json:"title"`
	Description *string  `json:"description"`
	StartsAt    string   `json:"startsAt"`
	EndsAt      string   `json:"endsAt"`
	MonitorIDs  []string `json:"monitorIds"`
}

// list — GET /v1/orgs/{orgID}/maintenance-windows  (üyelik yeterli)
//
// Okuma üyeye açık: "ne zaman bakım var" bilgisi salt-okunur panonun parçası.
func (h *maintenanceHandler) list(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	v := &validator{}
	limit := parseLimit(v, r.URL.Query().Get("limit"))

	if apiErr := v.err(); apiErr != nil {
		apiErr.status = http.StatusBadRequest
		apiErr.code = codeInvalidQuery
		apiErr.message = "sorgu parametreleri geçersiz"
		apiErr.write(w, r)

		return
	}

	windows, err := h.windows.List(r.Context(), orgID, limit)
	if err != nil {
		writeInternalError(w, r, h.logger, "bakım pencereleri okunamadı", err)
		return
	}

	items := make([]maintenanceWindowResponse, 0, len(windows))
	for _, window := range windows {
		items = append(items, newMaintenanceWindowResponse(window))
	}

	writeJSON(w, http.StatusOK, map[string][]maintenanceWindowResponse{"maintenanceWindows": items})
}

// create — POST /v1/orgs/{orgID}/maintenance-windows  (admin+)
//
// Bakım penceresi tanımlamak 0.1 §2'de açıkça admin yetkisi ("bakım penceresi
// tanımlama"): pencere otomatik incident'ları bastırıyor, yani yanlış
// kullanıldığında gerçek bir kesintiyi görünmez yapabilir.
func (h *maintenanceHandler) create(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	var req createMaintenanceWindowRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	v := &validator{}
	startsAt, hasStart := parseTimeParam(v, "startsAt", req.StartsAt)
	endsAt, hasEnd := parseTimeParam(v, "endsAt", req.EndsAt)

	if !hasStart && req.StartsAt == "" {
		v.fail("startsAt", "zorunlu")
	}

	if !hasEnd && req.EndsAt == "" {
		v.fail("endsAt", "zorunlu")
	}

	monitorIDs := make([]uuid.UUID, 0, len(req.MonitorIDs))
	for _, raw := range req.MonitorIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			// Biçimsiz kimlik, "olmayan servis"ten farklı bir hata: istemcinin
			// gövdeyi yanlış kurduğunu söylüyor.
			v.fail("monitorIds", "geçerli bir servis kimliği olmalı")
			break
		}

		monitorIDs = append(monitorIDs, id)
	}

	if apiErr := v.err(); apiErr != nil {
		// Zaman ve kimlik biçimi GÖVDE doğrulaması: 422 (bkz. request.go).
		apiErr.write(w, r)
		return
	}

	window, err := h.windows.Create(r.Context(), orgID, maintenance.Input{
		Title:       req.Title,
		Description: req.Description,
		StartsAt:    startsAt,
		EndsAt:      endsAt,
		MonitorIDs:  monitorIDs,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "bakım penceresi oluşturulamadı")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]maintenanceWindowResponse{
		"maintenanceWindow": newMaintenanceWindowResponse(window),
	})
}

// remove — DELETE /v1/orgs/{orgID}/maintenance-windows/{windowID}  (admin+)
//
// Yalnızca HENÜZ BAŞLAMAMIŞ pencere silinebiliyor; başlamış olan 409 alıyor.
// Gerekçe: başlamış bir pencereyi kaldırmak uptime geçmişini yeniden yazmak
// demek (bkz. maintenance.ErrStarted).
func (h *maintenanceHandler) remove(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	windowID, ok := windowIDFrom(w, r)
	if !ok {
		return
	}

	if err := h.windows.Delete(r.Context(), orgID, windowID); err != nil {
		h.writeServiceError(w, r, err, "bakım penceresi silinemedi")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *maintenanceHandler) orgID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return uuid.UUID{}, false
	}

	return membership.OrgID, true
}

func windowIDFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	windowID, err := uuid.Parse(chi.URLParam(r, "windowID"))
	if err != nil {
		writeMaintenanceNotFound(w, r)
		return uuid.UUID{}, false
	}

	return windowID, true
}

func (h *maintenanceHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var validation *maintenance.ValidationError
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

	if errors.Is(err, maintenance.ErrNotFound) {
		writeMaintenanceNotFound(w, r)
		return
	}

	if errors.Is(err, maintenance.ErrStarted) {
		writeError(w, r, http.StatusConflict, codeMaintenanceStarted,
			"başlamış bir bakım penceresi silinemez")

		return
	}

	writeInternalError(w, r, h.logger, logMsg, err)
}

func writeMaintenanceNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir bakım penceresi yok")
}
