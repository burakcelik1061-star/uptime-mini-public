package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// Bildirim kanalları (Faz 6.3) — org-scoped admin yüzeyi.
//
// Gönderimin KENDİSİ burada değil: bildirimleri worker'daki job gönderiyor
// (bkz. internal/notifications → Dispatcher). api yalnızca yapılandırmayı
// yönetiyor ve TEK bir istisna olarak test bildirimini senkron gönderiyor —
// orada düğmeye basıp sonucu bekleyen bir kullanıcı var.

// NotificationService, bildirim uçlarının ihtiyaç duyduğu davranışlar.
type NotificationService interface {
	List(ctx context.Context, orgID uuid.UUID) ([]notifications.ChannelWithStats, error)
	Create(
		ctx context.Context, orgID uuid.UUID, in notifications.ChannelInput,
	) (store.NotificationChannel, error)
	Delete(ctx context.Context, orgID, channelID uuid.UUID) error
	SendTest(ctx context.Context, orgID, channelID uuid.UUID, orgName string) error
}

type notificationHandler struct {
	channels NotificationService
	logger   *slog.Logger
}

// notificationChannelResponse, kanalın dışa verilen gösterimi.
type notificationChannelResponse struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Target string `json:"target"`
	// Deliveries, gönderim özeti: ekran "kanal çalışıyor mu" sorusunu
	// cevaplayabilsin.
	Deliveries deliveryStatsResponse `json:"deliveries"`
	CreatedAt  time.Time             `json:"createdAt"`
}

type deliveryStatsResponse struct {
	Sent    int64 `json:"sent"`
	Pending int64 `json:"pending"`
	Failed  int64 `json:"failed"`
}

// createNotificationChannelRequest, yeni kanalın gövdesi.
type createNotificationChannelRequest struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Target string `json:"target"`
}

// list — GET /v1/orgs/{orgID}/notification-channels
//
// OKUMA: üyelik yeterli — "kesinti olunca kime haber gidiyor" salt-okunur
// panonun parçası (0.1 §2).
func (h *notificationHandler) list(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	channels, err := h.channels.List(r.Context(), membership.OrgID)
	if err != nil {
		writeInternalError(w, r, h.logger, "bildirim kanalları okunamadı", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string][]notificationChannelResponse{
		"channels": newNotificationChannelResponses(channels),
	})
}

// create — POST /v1/orgs/{orgID}/notification-channels  (admin+)
func (h *notificationHandler) create(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	var req createNotificationChannelRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	// DOĞRULAMA SERVİSTE (Faz 2'den beri): burada yalnızca gövde sözleşmeden
	// alan sözlüğüne çevriliyor.
	channel, err := h.channels.Create(r.Context(), membership.OrgID, notifications.ChannelInput{
		Kind:   store.NotificationChannelKind(req.Kind),
		Name:   req.Name,
		Target: req.Target,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "bildirim kanalı oluşturulamadı")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]notificationChannelResponse{
		"channel": newNotificationChannelResponse(notifications.ChannelWithStats{Channel: channel}),
	})
}

// remove — DELETE /v1/orgs/{orgID}/notification-channels/{channelID}  (admin+)
func (h *notificationHandler) remove(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir bildirim kanalı yok")
		return
	}

	if err := h.channels.Delete(r.Context(), membership.OrgID, channelID); err != nil {
		h.writeServiceError(w, r, err, "bildirim kanalı silinemedi")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// test — POST /v1/orgs/{orgID}/notification-channels/{channelID}/test  (admin+)
//
// SENKRON: kullanıcı düğmeye basıp sonucu görmek istiyor. Gerçek bildirimler
// asenkron (worker job'ı) çünkü orada bekleyen kimse yok ve gönderim kesinti
// yazmayı bekletmemeli.
//
// Deftere YAZILMIYOR: defterin sorusu "şu KESİNTİ şu kanala bildirildi mi" ve
// test bildirimi bir kesintiye ait değil.
func (h *notificationHandler) test(w http.ResponseWriter, r *http.Request) {
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	channelID, err := uuid.Parse(chi.URLParam(r, "channelID"))
	if err != nil {
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir bildirim kanalı yok")
		return
	}

	if err := h.channels.SendTest(r.Context(), membership.OrgID, channelID, membership.OrgName); err != nil {
		h.writeServiceError(w, r, err, "test bildirimi gönderilemedi")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// membership, org-scoped context'ten üyeliği okur.
func (h *notificationHandler) membership(w http.ResponseWriter, r *http.Request) (orgs.Membership, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return orgs.Membership{}, false
	}

	return membership, true
}

// writeServiceError, servis hatalarını sözleşmedeki karşılıklarına çevirir.
func (h *notificationHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var validation *notifications.ValidationError

	switch {
	case errors.As(err, &validation):
		fields := make([]FieldError, 0, len(validation.Problems))
		for _, problem := range validation.Problems {
			fields = append(fields, FieldError{Field: problem.Field, Message: problem.Message})
		}

		(&apiError{
			status:  http.StatusUnprocessableEntity,
			code:    codeValidationFailed,
			message: "gövde doğrulanamadı",
			fields:  fields,
		}).write(w, r)

	case errors.Is(err, notifications.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir bildirim kanalı yok")

	case errors.Is(err, notifications.ErrDuplicate):
		// 409: gövde geçerli, çakışan bir kayıt var. Aynı hedefi iki kez
		// eklemek her kesintide iki bildirim demek olurdu.
		writeError(w, r, http.StatusConflict, codeChannelExists, "bu hedef bu organizasyonda zaten ekli")

	case errors.Is(err, notifications.ErrTargetBlocked):
		// 422 ve ALAN BAZLI: sorun gövdenin `target` alanında ve
		// düzeltmesi kullanıcıda. 500 dönmek, kullanıcının yazdığı iç ağ
		// adresini sunucunun arızası gibi gösterirdi (7.4).
		(&apiError{
			status:  http.StatusUnprocessableEntity,
			code:    codeValidationFailed,
			message: "hedef adres gönderilemedi",
			fields: []FieldError{{
				Field:   "target",
				Message: "iç ağ, loopback ve link-local adreslere bildirim gönderilemiyor",
			}},
		}).write(w, r)

	case errors.Is(err, notifications.ErrUnsupportedKind):
		writeError(w, r, http.StatusUnprocessableEntity, codeValidationFailed,
			"bu kanal türü için gönderici tanımlı değil")

	default:
		writeInternalError(w, r, h.logger, logMsg, err)
	}
}

func newNotificationChannelResponses(channels []notifications.ChannelWithStats) []notificationChannelResponse {
	result := make([]notificationChannelResponse, 0, len(channels))
	for _, channel := range channels {
		result = append(result, newNotificationChannelResponse(channel))
	}

	return result
}

func newNotificationChannelResponse(channel notifications.ChannelWithStats) notificationChannelResponse {
	return notificationChannelResponse{
		ID:     channel.Channel.ID.String(),
		Kind:   string(channel.Channel.Kind),
		Name:   channel.Channel.Name,
		Target: channel.Channel.Target,
		Deliveries: deliveryStatsResponse{
			Sent:    channel.Stats.Sent,
			Pending: channel.Stats.Pending,
			Failed:  channel.Stats.Failed,
		},
		CreatedAt: channel.Channel.CreatedAt,
	}
}
