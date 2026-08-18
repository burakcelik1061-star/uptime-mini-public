package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// MonitorService, monitor uçlarının ihtiyaç duyduğu davranışlar
// (bkz. internal/monitors).
type MonitorService interface {
	Create(ctx context.Context, orgID uuid.UUID, in monitors.Settings) (store.Monitor, error)
	List(ctx context.Context, orgID uuid.UUID) (monitors.Listing, error)
	Get(ctx context.Context, orgID, monitorID uuid.UUID) (store.Monitor, error)
	Update(ctx context.Context, orgID, monitorID uuid.UUID, patch monitors.Patch) (store.Monitor, error)
	Delete(ctx context.Context, orgID, monitorID uuid.UUID) error
}

type monitorHandler struct {
	monitors MonitorService
	// checks, manuel "şimdi kontrol et" ucunun çalıştırıcısı (bkz. checks.go).
	checks CheckRunner
	// results, kontrol sonuçlarının zaman serisi (bkz. internal/results).
	results CheckResultService
	logger  *slog.Logger
}

// monitorResponse, monitor'ün DIŞA VERİLEN gösterimi.
//
// store.Monitor doğrudan serileştirilmiyor (userResponse ile aynı gerekçe):
// o struct şemayı takip ediyor ve 3.x'te scheduler alanlarıyla büyüyecek —
// sözleşme, şemanın büyümesiyle kendiliğinden değişmemeli.
type monitorResponse struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	URL                  string `json:"url"`
	Method               string `json:"method"`
	CheckIntervalSeconds int32  `json:"checkIntervalSeconds"`
	TimeoutMs            int32  `json:"timeoutMs"`
	ExpectedStatusMin    int32  `json:"expectedStatusMin"`
	ExpectedStatusMax    int32  `json:"expectedStatusMax"`
	DegradedResponseMs   *int32 `json:"degradedResponseMs"`
	FailureThreshold     int32  `json:"failureThreshold"`
	RecoveryThreshold    int32  `json:"recoveryThreshold"`
	IsActive             bool   `json:"isActive"`
	Status               string `json:"status"`
	// Durum makinesinin (3.3) türettiği okunur alanlar. `nextCheckAt`
	// BİLİNÇLİ OLARAK YOK: sıradaki kontrolün zamanı worker'ın iç defteri,
	// istemcinin kararını değiştirmiyor.
	LastCheckedAt *time.Time `json:"lastCheckedAt"`
	// Stale, ölçümün DURMUŞ olduğu (8.6): `status` son BİLİNEN durumu
	// taşımaya devam ediyor, bu bayrak "o bilgi artık güncel değil" diyor.
	//
	// Kararı SUNUCU veriyor: istemci `lastCheckedAt` ile kendi saatinden
	// türetseydi, aynı kuralın panoda ve public sayfada iki kopyası olurdu
	// (5.2'nin "günün hâlini istemci eşikten türetmez" kararı) ve tarayıcının
	// saati yanlışsa sonuç da yanlış olurdu.
	Stale           bool      `json:"stale"`
	StatusChangedAt time.Time `json:"statusChangedAt"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// newMonitorResponse, bayatlık BİLİNMEDEN çağrılan yollar için (detay,
// oluşturma, güncelleme). Bu cevaplarda `stale` her zaman false: tek bir
// monitor'ün cevabı için veritabanından ayrıca "şimdi" okumak, panonun
// listesinde zaten çözülmüş bir soruyu ikinci kez sormak olurdu.
func newMonitorResponse(m store.Monitor) monitorResponse {
	return monitorResponse{
		ID:                   m.ID.String(),
		Name:                 m.Name,
		URL:                  m.Url,
		Method:               string(m.Method),
		CheckIntervalSeconds: m.CheckIntervalSeconds,
		TimeoutMs:            m.TimeoutMs,
		ExpectedStatusMin:    m.ExpectedStatusMin,
		ExpectedStatusMax:    m.ExpectedStatusMax,
		// null = "yavaş" sinyali kapalı. Alan cevapta HER ZAMAN var
		// (omitempty yok): istemci "gönderilmedi" ile "kapalı"yı ayırmak
		// zorunda kalmasın.
		DegradedResponseMs: m.DegradedResponseMs,
		IsActive:           m.IsActive,
		// Şemadaki adı `current_status`; sözleşmede `status`. "current"
		// istemci için bilgi taşımıyor.
		Status:            string(m.CurrentStatus),
		FailureThreshold:  m.FailureThreshold,
		RecoveryThreshold: m.RecoveryThreshold,
		LastCheckedAt:     m.LastCheckedAt,
		StatusChangedAt:   m.StatusChangedAt,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}

// createMonitorRequest — zorunlu olmayan alanlar İŞARETÇİ.
//
// `checkIntervalSeconds: 0` gönderen istemci ile alanı hiç göndermeyen
// istemci aynı şey değil: ilki hata almalı, ikincisi varsayılanı. Sıfır
// değerle "gönderilmedi"yi ayırmanın Go'daki yolu işaretçi.
type createMonitorRequest struct {
	Name                 string  `json:"name"`
	URL                  string  `json:"url"`
	Method               *string `json:"method"`
	CheckIntervalSeconds *int32  `json:"checkIntervalSeconds"`
	TimeoutMs            *int32  `json:"timeoutMs"`
	ExpectedStatusMin    *int32  `json:"expectedStatusMin"`
	ExpectedStatusMax    *int32  `json:"expectedStatusMax"`
	DegradedResponseMs   *int32  `json:"degradedResponseMs"`
	FailureThreshold     *int32  `json:"failureThreshold"`
	RecoveryThreshold    *int32  `json:"recoveryThreshold"`
	IsActive             *bool   `json:"isActive"`
}

type updateMonitorRequest struct {
	Name                 *string `json:"name"`
	URL                  *string `json:"url"`
	Method               *string `json:"method"`
	CheckIntervalSeconds *int32  `json:"checkIntervalSeconds"`
	TimeoutMs            *int32  `json:"timeoutMs"`
	ExpectedStatusMin    *int32  `json:"expectedStatusMin"`
	ExpectedStatusMax    *int32  `json:"expectedStatusMax"`
	FailureThreshold     *int32  `json:"failureThreshold"`
	RecoveryThreshold    *int32  `json:"recoveryThreshold"`
	IsActive             *bool   `json:"isActive"`
	// ÜÇ DURUMLU alan: gönderilmedi / null (eşiği kaldır) / sayı.
	// İşaretçi tek başına ilk ikisini ayıramıyor, çünkü null da geçerli bir
	// DEĞER (bkz. nullable).
	DegradedResponseMs nullable[int32] `json:"degradedResponseMs"`
}

// list — GET /v1/orgs/{orgID}/monitors  (üyelik yeterli)
func (h *monitorHandler) list(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	listing, err := h.monitors.List(r.Context(), orgID)
	if err != nil {
		writeInternalError(w, r, h.logger, "monitor'ler okunamadı", err)
		return
	}

	// Boş liste `null` değil `[]` (bkz. orgs listesi).
	items := make([]monitorResponse, 0, len(listing.Monitors))
	for _, row := range listing.Monitors {
		item := newMonitorResponse(row)
		// BAYATLIK (8.6): "şimdi" listeyle birlikte VERİTABANINDAN geldi
		// (bkz. monitors.Listing) — api'nin kendi saati kullanılsaydı,
		// `last_checked_at`i yazan worker ile farklı bir saate bakılırdı.
		item.Stale = monitors.IsStale(
			row.IsActive, row.LastCheckedAt, row.CheckIntervalSeconds, listing.Now)
		items = append(items, item)
	}

	writeJSON(w, http.StatusOK, map[string][]monitorResponse{"monitors": items})
}

// detail — GET /v1/orgs/{orgID}/monitors/{monitorID}  (üyelik yeterli)
func (h *monitorHandler) detail(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	monitorID, ok := monitorIDFrom(w, r)
	if !ok {
		return
	}

	monitor, err := h.monitors.Get(r.Context(), orgID, monitorID)
	if err != nil {
		h.writeServiceError(w, r, err, "monitor okunamadı")
		return
	}

	writeJSON(w, http.StatusOK, map[string]monitorResponse{"monitor": newMonitorResponse(monitor)})
}

// create — POST /v1/orgs/{orgID}/monitors  (admin+)
func (h *monitorHandler) create(w http.ResponseWriter, r *http.Request) {
	// Üyeliğin TAMAMI gerekiyor, yalnızca org kimliği değil: varsayılan
	// kontrol aralığı organizasyonun PLANINA göre seçiliyor (8.5).
	membership, ok := h.membership(w, r)
	if !ok {
		return
	}

	orgID := membership.OrgID

	var req createMonitorRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	// Gönderilmeyen alanlar VARSAYILANA düşüyor; doğrulama servisin işi.
	in := monitors.Settings{
		Name:   req.Name,
		URL:    req.URL,
		Method: parseMethod(req.Method),
		// VARSAYILAN ARALIK PLANA GÖRE (8.5). Ürünün varsayılanı 60 saniye
		// ama FREE planın alt sınırı 300 — sabit varsayılan, YENİ bir
		// kullanıcının hiçbir alan doldurmadan eklediği İLK monitor'ün
		// reddedilmesi demekti (çıkmaz yol).
		//
		// Bu, kuralın ikinci bir kopyası DEĞİL: sınır yine `plans`ten
		// okunuyor ve asıl denetim serviste, kilitli satırın planına karşı
		// koşuyor. Buradaki tek iş, kullanıcı bir değer YAZMADIĞINDA
		// planına uyan bir başlangıç seçmek. Açıkça yazılmış çok küçük bir
		// aralık yine reddediliyor.
		CheckIntervalSeconds: valueOr(
			req.CheckIntervalSeconds, defaultIntervalFor(membership.OrgPlan)),
		TimeoutMs:         valueOr(req.TimeoutMs, monitors.DefaultTimeoutMs),
		ExpectedStatusMin: valueOr(req.ExpectedStatusMin, monitors.DefaultExpectedStatusMin),
		ExpectedStatusMax: valueOr(req.ExpectedStatusMax, monitors.DefaultExpectedStatusMax),
		// Varsayılanı YOK: gönderilmezse "yavaş" sinyali kapalı. Uydurulmuş
		// bir eşik, kullanıcının hiç istemediği degraded kayıtları üretirdi.
		DegradedResponseMs: req.DegradedResponseMs,
		// Durum makinesinin eşikleri (3.3): asimetrik varsayılan — kesinti
		// ilan etmek pahalı, kesintinin bittiğini söylemek ucuz.
		FailureThreshold:  valueOr(req.FailureThreshold, monitors.DefaultFailureThreshold),
		RecoveryThreshold: valueOr(req.RecoveryThreshold, monitors.DefaultRecoveryThreshold),
		IsActive:          valueOr(req.IsActive, true),
	}

	monitor, err := h.monitors.Create(r.Context(), orgID, in)
	if err != nil {
		h.writeServiceError(w, r, err, "monitor oluşturulamadı")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]monitorResponse{"monitor": newMonitorResponse(monitor)})
}

// update — PATCH /v1/orgs/{orgID}/monitors/{monitorID}  (admin+)
//
// PUT değil PATCH: listedeki "duraklat" düğmesi tek alan gönderiyor, tüm
// yapılandırmayı geri yazmak zorunda değil (aksi hâlde iki istemci arasında
// birbirinin ayarını ezmek kolaylaşırdı).
func (h *monitorHandler) update(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	monitorID, ok := monitorIDFrom(w, r)
	if !ok {
		return
	}

	var req updateMonitorRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		apiErr.write(w, r)
		return
	}

	patch := monitors.Patch{
		Name:                  req.Name,
		URL:                   req.URL,
		CheckIntervalSeconds:  req.CheckIntervalSeconds,
		TimeoutMs:             req.TimeoutMs,
		ExpectedStatusMin:     req.ExpectedStatusMin,
		ExpectedStatusMax:     req.ExpectedStatusMax,
		FailureThreshold:      req.FailureThreshold,
		RecoveryThreshold:     req.RecoveryThreshold,
		IsActive:              req.IsActive,
		DegradedResponseMs:    req.DegradedResponseMs.Value,
		SetDegradedResponseMs: req.DegradedResponseMs.Present,
	}

	if req.Method != nil {
		method := parseMethod(req.Method)
		patch.Method = &method
	}

	monitor, err := h.monitors.Update(r.Context(), orgID, monitorID, patch)
	if err != nil {
		h.writeServiceError(w, r, err, "monitor güncellenemedi")
		return
	}

	writeJSON(w, http.StatusOK, map[string]monitorResponse{"monitor": newMonitorResponse(monitor)})
}

// remove — DELETE /v1/orgs/{orgID}/monitors/{monitorID}  (admin+)
func (h *monitorHandler) remove(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	monitorID, ok := monitorIDFrom(w, r)
	if !ok {
		return
	}

	if err := h.monitors.Delete(r.Context(), orgID, monitorID); err != nil {
		h.writeServiceError(w, r, err, "monitor silinemedi")
		return
	}

	// 204: silinen kaydın gövdesini döndürmenin istemciye faydası yok.
	w.WriteHeader(http.StatusNoContent)
}

// orgID, doğrulanmış üyelikten organizasyon kimliğini alır.
//
// YOL PARÇASINDAN OKUNMUYOR: context'teki değer middleware'in veritabanına
// karşı doğruladığı değer (bkz. orgscope.go). Yolu yeniden okumak,
// doğrulanmamış bir girdiyle sorgu yapmanın kapısını açardı.
func (h *monitorHandler) orgID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	membership, ok := h.membership(w, r)
	if !ok {
		return uuid.UUID{}, false
	}

	return membership.OrgID, true
}

// membership, context'teki doğrulanmış üyeliği döner.
func (h *monitorHandler) membership(w http.ResponseWriter, r *http.Request) (orgs.Membership, bool) {
	membership, ok := orgs.MembershipFrom(r.Context())
	if !ok {
		h.logger.Error("org-scoped handler üyelik olmadan çağrıldı",
			slog.String("path", r.URL.Path))
		writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)

		return orgs.Membership{}, false
	}

	return membership, true
}

// monitorIDFrom, yol parçasını çözer; biçimsizse 404.
//
// "Geçersiz uuid" demek zararsız olurdu ama org kimliğinde olduğu gibi
// (orgscope.go) tek bir cevap üretmek, ileride varlık bilgisi sızdıran bir
// üçüncü cevabın doğmasını imkânsız kılıyor.
func monitorIDFrom(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	monitorID, err := uuid.Parse(chi.URLParam(r, "monitorID"))
	if err != nil {
		writeMonitorNotFound(w, r)
		return uuid.UUID{}, false
	}

	return monitorID, true
}

// writeServiceError, servis katmanının hatalarını HTTP'ye çevirir.
func (h *monitorHandler) writeServiceError(w http.ResponseWriter, r *http.Request, err error, logMsg string) {
	var validation *monitors.ValidationError
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

	if errors.Is(err, monitors.ErrNotFound) {
		writeMonitorNotFound(w, r)
		return
	}

	// PLAN KOTASI (8.5) — İKİ FARKLI DURUM, İKİ FARKLI CEVAP ama AYNI KOD.
	//
	// Kod ortak (`plan_limit_reached`) çünkü istemcinin yapacağı şey aynı:
	// kullanıcıya yükseltme yolunu göstermek. Durum kodu ise ayrı, çünkü
	// sorunun yeri farklı.
	var limitReached *monitors.ErrMonitorLimitReached
	if errors.As(err, &limitReached) {
		// 409: gövdede yanlış bir ALAN yok — organizasyonun DURUMU (zaten N
		// monitor'ü var) isteği reddediyor. 1.1'in `email_taken`i ve 6.3'ün
		// `channel_exists`i ile aynı sınıf.
		writeError(w, r, http.StatusConflict, codePlanLimitReached, limitReached.Error())

		return
	}

	var intervalBelow *monitors.ErrIntervalBelowPlan
	if errors.As(err, &intervalBelow) {
		// 422 + ALAN BAZLI: sorun gerçekten gövdedeki bir alanın değerinde ve
		// düzeltmesi kullanıcıda (7.4'ün `target` alanı kararıyla aynı).
		(&apiError{
			status:  http.StatusUnprocessableEntity,
			code:    codePlanLimitReached,
			message: intervalBelow.Error(),
			fields: []FieldError{{
				Field:   "checkIntervalSeconds",
				Message: intervalBelow.Error(),
			}},
		}).write(w, r)

		return
	}

	writeInternalError(w, r, h.logger, logMsg, err)
}

func writeMonitorNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir monitor yok")
}

// parseMethod, metodu enum değerine çevirir.
//
// Büyük/küçük harf esnetiliyor ("get" → "GET"): HTTP metotları zaten büyük
// harfli, kullanıcının küçük yazması bir hata değil. TANINMAYAN değer
// olduğu gibi bırakılıyor — doğrulama onu reddedip alan hatası üretsin,
// burada sessizce GET'e düşmesin.
func parseMethod(raw *string) store.MonitorMethod {
	if raw == nil {
		return monitors.DefaultMethod
	}

	return store.MonitorMethod(strings.ToUpper(strings.TrimSpace(*raw)))
}

// valueOr, işaretçi nil ise varsayılanı döner.
func valueOr[T any](ptr *T, fallback T) T {
	if ptr == nil {
		return fallback
	}

	return *ptr
}

// defaultIntervalFor, plana uyan başlangıç kontrol aralığı (8.5).
//
// Ürünün varsayılanı (60 sn) planın alt sınırının altında kalabiliyor —
// FREE planda 300 saniye. O durumda planın sınırı kazanıyor, yani kullanıcı
// hiçbir alan doldurmadan geçerli bir monitor ekleyebiliyor.
func defaultIntervalFor(plan store.OrgPlan) int32 {
	minimum := plans.For(plan).MinCheckIntervalSeconds
	if monitors.DefaultCheckIntervalSeconds > minimum {
		return monitors.DefaultCheckIntervalSeconds
	}

	return minimum
}
