package httpapi

// Status page'in ADMİN uçları (Faz 5.1) — org-scoped ağacın altında.
//
// Ziyaretçi tarafı public.go'da ve o alt-router bilinçli olarak bu ağacın
// DIŞINDA: oturumsuz bir yüzeyi `requireOrgMembership`in koruduğu ağaca
// bağlamak, "kimliği doğrulanmış istek" varsayımını taşımayan tek uç için
// tüm ağacın varsayımını zayıflatmak olurdu.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
)

// StatusPageService, status page uçlarının ihtiyaç duyduğu davranışlar
// (bkz. internal/statuspage).
type StatusPageService interface {
	Get(ctx context.Context, orgID uuid.UUID) (statuspage.Settings, error)
	UpdateSettings(
		ctx context.Context, orgID uuid.UUID, patch statuspage.SettingsPatch,
	) (statuspage.Settings, error)
	ReplaceServices(
		ctx context.Context, orgID uuid.UUID, in statuspage.ServicesInput,
	) (statuspage.Settings, error)
	Public(ctx context.Context, slug string) (statuspage.PublicPage, error)
	// PublishedSlug, public WebSocket kanalının kapısı (6.2): slug
	// yayımlanmış bir sayfaya ait mi. Okuma ucunun kullandığı sorgunun
	// aynısına dayanıyor — "yayımlanmamış ≡ olmayan" kararı tek yerde.
	PublishedSlug(ctx context.Context, slug string) error
}

type statusPageHandler struct {
	pages  StatusPageService
	logger *slog.Logger
}

// statusPageResponse, sayfanın ADMİN gözüyle gösterimi.
//
// `id` sözleşmede YOK: org başına tek sayfa olduğu için (0.1 §5) istemcinin
// onu adresleyeceği bir uç de yok — organizasyon zaten yolun içinde.
type statusPageResponse struct {
	Slug        string `json:"slug"`
	IsPublished bool   `json:"isPublished"`
	// Görünüm alanları (5.3); null = varsayılan davranış (başlıkta
	// organizasyonun adı, renkte varsayılan vurgu).
	Title       *string `json:"title"`
	Description *string `json:"description"`
	LogoURL     *string `json:"logoUrl"`
	ThemeColor  *string `json:"themeColor"`
	// PublicPath, sayfanın path tabanlı adresi. Sunucudan dönüyor ki istemci
	// "/status/" önekini kendi kurmasın — 5.4'te alt alan adı geldiğinde bu
	// bilginin tek bir yerde değişmesi yeterli olsun.
	PublicPath string    `json:"publicPath"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// statusPageItemResponse, yayımlanan tek bir servis.
type statusPageItemResponse struct {
	MonitorID string `json:"monitorId"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Status    string `json:"status"`
	// Group null = gruplanmamış (5.3).
	Group *string `json:"group"`
}

// replaceStatusPageServicesRequest, yayımlanan servis seçiminin ve
// gruplamasının TAMAMI.
//
// İkisi tek gövdede: gruplar ayrı bir uçtan yazılsaydı "servisleri olmayan
// grup" ve "silinmiş gruba işaret eden servis" diye iki ara durum doğardı.
type replaceStatusPageServicesRequest struct {
	// Groups, grup adları — SIRA ÖNEMLİ.
	Groups []string `json:"groups"`
	Items  []struct {
		MonitorID string `json:"monitorId"`
		// Group, `groups` içinde geçen bir ad; null/boş = gruplanmamış.
		Group *string `json:"group"`
	} `json:"items"`
}

// updateStatusPageRequest, sayfa ayarları.
//
// Görünüm alanları `nullable[T]`: üç durum ayrı — gönderilmedi (dokunma),
// `null` (temizle), değer (yaz). Tek bir işaretçi bunları ayıramazdı ve
// "başlığı sil" isteği sessizce yok sayılırdı (2.2'nin `degradedResponseMs`
// kararının aynısı).
type updateStatusPageRequest struct {
	IsPublished *bool            `json:"isPublished"`
	Title       nullable[string] `json:"title"`
	Description nullable[string] `json:"description"`
	LogoURL     nullable[string] `json:"logoUrl"`
	ThemeColor  nullable[string] `json:"themeColor"`
}

// get — GET /v1/orgs/{orgID}/status-page  (üyelik yeterli)
//
// Okuma üyeye açık: 0.1 §2 `member`ı "salt-okunur pano" olarak tanımlıyor ve
// hangi servislerin yayımlandığı panonun bir parçası. YAZMA (aşağıdaki iki uç)
// admin+: 0.1 §2'de "status page özelleştirme" açıkça admin yetkisi.
func (h *statusPageHandler) get(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	settings, err := h.pages.Get(r.Context(), orgID)
	if err != nil {
		h.writeServiceError(w, r, err, "status page okunamadı")
		return
	}

	writeJSON(w, http.StatusOK, statusPageBody(settings))
}

// update — PATCH /v1/orgs/{orgID}/status-page  (admin+)
//
// Yayım durumu ve görünüm alanları AYNI UÇTA: hepsi aynı kaydın alanı ve iki
// ayrı yazma yolu, "yayımla" ile "başlığı değiştir"in birbirini ezmesine açık
// olurdu. Gönderilmeyen alan değişmiyor (2.1'in PATCH kararı).
func (h *statusPageHandler) update(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	var req updateStatusPageRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	settings, err := h.pages.UpdateSettings(r.Context(), orgID, statuspage.SettingsPatch{
		IsPublished:    req.IsPublished,
		Title:          req.Title.Value,
		SetTitle:       req.Title.Present,
		Description:    req.Description.Value,
		SetDescription: req.Description.Present,
		LogoURL:        req.LogoURL.Value,
		SetLogoURL:     req.LogoURL.Present,
		ThemeColor:     req.ThemeColor.Value,
		SetThemeColor:  req.ThemeColor.Present,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "status page güncellenemedi")
		return
	}

	writeJSON(w, http.StatusOK, statusPageBody(settings))
}

// replaceServices — PUT /v1/orgs/{orgID}/status-page/services  (admin+)
//
// PUT, POST/DELETE değil: gönderilen liste yeni seçimin TAMAMI. "Ekle"/"çıkar"
// uçları, arayüzdeki seçim ile sunucudaki seçimin sessizce ayrışmasına açıktı
// ve o ayrışmanın adı "yayımlanmaması gereken bir servis yayımda kaldı".
//
// Gruplar da aynı gövdede (5.3, bkz. replaceStatusPageServicesRequest).
func (h *statusPageHandler) replaceServices(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	var req replaceStatusPageServicesRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	v := &validator{}

	items := make([]statuspage.ItemInput, 0, len(req.Items))
	for _, raw := range req.Items {
		id, err := uuid.Parse(raw.MonitorID)
		if err != nil {
			v.fail("items", "geçerli bir servis kimliği olmalı")
			break
		}

		group := ""
		if raw.Group != nil {
			group = *raw.Group
		}

		items = append(items, statuspage.ItemInput{MonitorID: id, Group: group})
	}

	if apiErr := v.err(); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	settings, err := h.pages.ReplaceServices(r.Context(), orgID, statuspage.ServicesInput{
		Groups: req.Groups,
		Items:  items,
	})
	if err != nil {
		h.writeServiceError(w, r, err, "yayımlanan servisler güncellenemedi")
		return
	}

	writeJSON(w, http.StatusOK, statusPageBody(settings))
}

// statusPageBody, admin cevabının gövdesi.
func statusPageBody(settings statuspage.Settings) map[string]any {
	items := make([]statusPageItemResponse, 0, len(settings.Items))
	for _, item := range settings.Items {
		items = append(items, statusPageItemResponse{
			MonitorID: item.MonitorID.String(),
			Name:      item.Name,
			URL:       item.URL,
			Status:    string(item.Status),
			Group:     item.Group,
		})
	}

	groups := settings.Groups
	if groups == nil {
		// Boş liste `null` değil `[]`.
		groups = []string{}
	}

	return map[string]any{
		"statusPage": statusPageResponse{
			Slug:        settings.Page.Slug,
			IsPublished: settings.Page.IsPublished,
			Title:       settings.Page.Title,
			Description: settings.Page.Description,
			LogoURL:     settings.Page.LogoUrl,
			ThemeColor:  settings.Page.ThemeColor,
			PublicPath:  "/status/" + settings.Page.Slug,
			CreatedAt:   settings.Page.CreatedAt,
			UpdatedAt:   settings.Page.UpdatedAt,
		},
		"groups": groups,
		"items":  items,
	}
}

// orgID, doğrulanmış üyelikten organizasyon kimliğini alır (yol parçası
// DEĞİL, context — 1.4'ün kuralı).
func (h *statusPageHandler) orgID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return uuid.UUID{}, false
	}

	return membership.OrgID, true
}

// writeServiceError, servis katmanının hatalarını HTTP'ye çevirir.
func (h *statusPageHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var validation *statuspage.ValidationError
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

	if errors.Is(err, statuspage.ErrNotFound) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "bu organizasyonun status page'i yok")
		return
	}

	writeInternalError(w, r, h.logger, logMsg, err)
}
