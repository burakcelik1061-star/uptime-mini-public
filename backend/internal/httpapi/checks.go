package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/checker"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// CheckRunner, kontrolü çalıştıran taraf (bkz. internal/checker).
type CheckRunner interface {
	Run(ctx context.Context, target checker.Target) (checker.Result, error)
}

// CheckResultService, sonuçların zaman serisi (bkz. internal/results).
type CheckResultService interface {
	Record(ctx context.Context, monitorID uuid.UUID,
		trigger store.CheckTrigger, result checker.Result) (store.CheckResult, error)
	ListRecent(ctx context.Context, orgID, monitorID uuid.UUID, limit int32) ([]store.CheckResult, error)
	ListInRange(ctx context.Context, orgID, monitorID uuid.UUID,
		from, to time.Time, limit int32) ([]store.CheckResult, error)
	Summarize(ctx context.Context, orgID, monitorID uuid.UUID,
		from, to time.Time) (results.Summary, error)
}

// checkResponse, tek bir kontrol sonucunun DIŞA VERİLEN gösterimi.
//
// Hem manuel kontrol hem geçmiş listesi AYNI şekli kullanıyor: istemcinin
// "az önce çalıştırdığım kontrol" ile "listedeki kontrol" için iki ayrı tip
// tutması gerekmesin.
type checkResponse struct {
	// ID metin: sütun `bigint` ve JavaScript'in Number'ı 2^53'ten sonra
	// hassasiyet kaybediyor. Bugün küçük olan bir kimliğe güvenip sözleşmeyi
	// sayıya bağlamak, ileride sessiz bir yuvarlama hatası demek olurdu.
	ID          string `json:"id"`
	Outcome     string `json:"outcome"`
	TriggeredBy string `json:"triggeredBy"`
	// null = cevap hiç alınamadı (bağlantı/isim/TLS hatası).
	HTTPStatus     *int32    `json:"httpStatus"`
	ResponseTimeMs *int32    `json:"responseTimeMs"`
	ErrorType      *string   `json:"errorType"`
	ErrorMessage   *string   `json:"errorMessage"`
	CheckedAt      time.Time `json:"checkedAt"`
}

func newCheckResponse(row store.CheckResult) checkResponse {
	response := checkResponse{
		ID:             strconv.FormatInt(row.ID, 10),
		Outcome:        string(row.Outcome),
		TriggeredBy:    string(row.TriggeredBy),
		HTTPStatus:     row.HttpStatus,
		ResponseTimeMs: row.ResponseTimeMs,
		ErrorMessage:   row.ErrorMessage,
		CheckedAt:      row.CheckedAt,
	}

	if row.ErrorType.Valid {
		errorType := string(row.ErrorType.CheckErrorType)
		response.ErrorType = &errorType
	}

	return response
}

// runCheck — POST /v1/orgs/{orgID}/monitors/{monitorID}/check   (admin+)
//
// "Şimdi kontrol et": monitor'ün yapılandırmasıyla tek bir kontrol çalıştırıp
// sonucu ZAMAN SERİSİNE YAZAR ve döndürür.
//
// SAKLAMA KARARI (2.2'den devredilen açık soru, 2.3'te kapandı): manuel
// kontroller de saklanıyor, ama `triggered_by = 'manual'` ile işaretli.
//   - Saklanmasalardı: gerçekten yapılmış bir ölçüm çöpe giderdi ve
//     scheduler (3.2) gelene kadar zaman serisi TAMAMEN boş kalırdı — 2.4'ün
//     detay sayfası gösterecek hiçbir veri bulamazdı.
//   - İşaretsiz saklansalardı: kullanıcının düğmeye üst üste basması uptime
//     yüzdesinin paydasını şişirir (3.4) ve durum makinesini (3.3) tetikleyip
//     olmayan bir kesinti üretebilirdi.
//
// NEDEN admin+ (okuma değil): bu uç SUNUCUYU DIŞARIYA İSTEK ATTIRIYOR —
// salt-okunur bir pano işlemi değil, tetiklenen bir yan etki. SSRF koruması
// ve hız sınırlaması (7.4) doğrudan bu ucu ilgilendiriyor.
func (h *monitorHandler) runCheck(w http.ResponseWriter, r *http.Request) {
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

	result, err := h.checks.Run(r.Context(), checker.TargetOf(monitor))
	if err != nil {
		// Kontrol ÇALIŞMADI (istemci bağlantıyı kesti, sunucu kapanıyor ya da
		// router'ın 30 sn'lik zaman aşımı devreye girdi). Bunu "başarısız
		// kontrol" olarak döndürmek yanlış olurdu: ölçüm yok, sonuç da yok —
		// bu yüzden zaman serisine de bir şey yazılmıyor.
		if errors.Is(err, checker.ErrCheckNotRun) {
			h.logger.Info("manuel kontrol tamamlanamadı",
				slog.String("monitor_id", monitorID.String()),
				slog.String("error", err.Error()))
			writeError(w, r, http.StatusServiceUnavailable, codeCheckNotRun,
				"kontrol tamamlanamadı, tekrar deneyin")

			return
		}

		writeInternalError(w, r, h.logger, "kontrol çalıştırılamadı", err)

		return
	}

	// Yazma başarısız olursa istek de başarısız: ölçümü sessizce düşürüp
	// istemciye "tamam" demek, kullanıcıya kaydedilmemiş bir kontrolü
	// kaydedilmiş gibi gösterirdi.
	row, err := h.results.Record(r.Context(), monitorID, store.CheckTriggerManual, result)
	if err != nil {
		writeInternalError(w, r, h.logger, "kontrol sonucu kaydedilemedi", err)
		return
	}

	// Monitor'ün `current_status`'una DOKUNULMUYOR: denormalize durumun
	// sahibi 3.3'ün durum makinesi ve o, ardışık sonuçlara bakıyor. Tek bir
	// manuel kontrolle monitor'ü DOWN'a çekmek, düğmeye basmayı kesinti
	// üretmek hâline getirirdi.
	h.logger.Info("manuel kontrol çalıştırıldı",
		slog.String("monitor_id", monitorID.String()),
		slog.String("org_id", orgID.String()),
		slog.String("outcome", string(result.Outcome)),
		slog.Int64("response_time_ms", result.ResponseTime.Milliseconds()),
		slog.String("error_type", string(result.ErrorType)))

	// 200 + sonuç: kontrolün BAŞARISIZ olması isteğin başarısız olması
	// demek değil. "Hedef çöktü" bilgisi başarıyla üretilmiş bir cevaptır;
	// 5xx döndürmek istemciye kendi hatamız gibi görünürdü.
	writeJSON(w, http.StatusOK, map[string]checkResponse{"check": newCheckResponse(row)})
}

// listChecks — GET /v1/orgs/{orgID}/monitors/{monitorID}/checks  (üyelik yeterli)
//
// Sorgu parametreleri:
//
//	limit  — kaç kayıt (varsayılan 50, üst sınır 200)
//	from   — RFC 3339 zaman damgası (dahil)
//	to     — RFC 3339 zaman damgası (hariç)
//
// Aralık verildiğinde sorgu YARI AÇIK [from, to): ardışık pencereler aynı
// satırı iki kez saymasın. Aralık verilmezse "son N kontrol" dönüyor.
func (h *monitorHandler) listChecks(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	monitorID, ok := monitorIDFrom(w, r)
	if !ok {
		return
	}

	// Monitor'ün varlığı ÖNCE doğrulanıyor: olmayan bir monitor için boş
	// liste dönmek, "yok" ile "hiç kontrolü yok"u aynı cevaba indirirdi.
	if _, err := h.monitors.Get(r.Context(), orgID, monitorID); err != nil {
		h.writeServiceError(w, r, err, "monitor okunamadı")
		return
	}

	query := r.URL.Query()

	v := &validator{}
	limit := parseLimit(v, query.Get("limit"))
	from, hasFrom := parseTimeParam(v, "from", query.Get("from"))
	to, hasTo := parseTimeParam(v, "to", query.Get("to"))

	if hasFrom != hasTo {
		v.fail("from", "from ve to birlikte gönderilmeli")
	}

	if hasFrom && hasTo && !to.After(from) {
		v.fail("to", "from'dan sonra olmalı")
	}

	if apiErr := v.err(); apiErr != nil {
		// Gövde değil SORGU parametresi hatalı: 400. (422, "gövde geçerli
		// JSON ama alanlar kabul edilemez" için ayrılmış — bkz. request.go.)
		apiErr.status = http.StatusBadRequest
		apiErr.code = codeInvalidQuery
		apiErr.message = "sorgu parametreleri geçersiz"
		apiErr.write(w, r)

		return
	}

	var (
		rows []store.CheckResult
		err  error
	)

	if hasFrom {
		rows, err = h.results.ListInRange(r.Context(), orgID, monitorID, from, to, limit)
	} else {
		rows, err = h.results.ListRecent(r.Context(), orgID, monitorID, limit)
	}

	if err != nil {
		writeInternalError(w, r, h.logger, "kontroller okunamadı", err)
		return
	}

	checks := make([]checkResponse, 0, len(rows))
	for _, row := range rows {
		checks = append(checks, newCheckResponse(row))
	}

	writeJSON(w, http.StatusOK, map[string][]checkResponse{"checks": checks})
}

// summaryResponse, bir aralığın ham uptime özeti (2.4).
type summaryResponse struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// UptimePercent null olabilir: aralıkta HİÇ zamanlanmış kontrol yoksa
	// oran %100 DEĞİL, BİLİNMİYOR (0.1'in bağlayıcı tanımı).
	UptimePercent   *float64 `json:"uptimePercent"`
	ScheduledChecks int64    `json:"scheduledChecks"`
	SuccessChecks   int64    `json:"successChecks"`
	DegradedChecks  int64    `json:"degradedChecks"`
	FailureChecks   int64    `json:"failureChecks"`
	// MaintenanceChecks, planlı bakım penceresine düşen kontroller (4.3):
	// `scheduledChecks`in içinde, ama uptime'ın payına da paydasına da
	// girmiyor. Arayüz "bu aralıkta N kontrol planlı bakımda" diyebilsin.
	MaintenanceChecks int64 `json:"maintenanceChecks"`
	// ManualChecks uptime'a girmiyor; arayüz "bu aralıkta N manuel kontrol
	// var" diyebilsin diye ayrıca dönüyor (bkz. 2.3).
	ManualChecks      int64    `json:"manualChecks"`
	AvgResponseTimeMs *float64 `json:"avgResponseTimeMs"`
}

// summary — GET /v1/orgs/{orgID}/monitors/{monitorID}/summary  (üyelik yeterli)
//
// `from` ve `to` ZORUNLU: "hangi aralık" sorusunun sessiz bir varsayılanı
// olsaydı (ör. son 24 saat), arayüzdeki aralık seçici ile sunucunun varsayımı
// bir gün ayrışırdı ve kullanıcı yanlış pencereye bakan bir yüzde görürdü.
func (h *monitorHandler) summary(w http.ResponseWriter, r *http.Request) {
	orgID, ok := h.orgID(w, r)
	if !ok {
		return
	}

	monitorID, ok := monitorIDFrom(w, r)
	if !ok {
		return
	}

	if _, err := h.monitors.Get(r.Context(), orgID, monitorID); err != nil {
		h.writeServiceError(w, r, err, "monitor okunamadı")
		return
	}

	query := r.URL.Query()

	v := &validator{}
	from, hasFrom := parseTimeParam(v, "from", query.Get("from"))
	to, hasTo := parseTimeParam(v, "to", query.Get("to"))

	if !hasFrom {
		v.fail("from", "zorunlu")
	}

	if !hasTo {
		v.fail("to", "zorunlu")
	}

	if hasFrom && hasTo && !to.After(from) {
		v.fail("to", "from'dan sonra olmalı")
	}

	if apiErr := v.err(); apiErr != nil {
		apiErr.status = http.StatusBadRequest
		apiErr.code = codeInvalidQuery
		apiErr.message = "sorgu parametreleri geçersiz"
		apiErr.write(w, r)

		return
	}

	summary, err := h.results.Summarize(r.Context(), orgID, monitorID, from, to)
	if err != nil {
		writeInternalError(w, r, h.logger, "kontrol özeti okunamadı", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]summaryResponse{"summary": {
		From:              summary.From,
		To:                summary.To,
		UptimePercent:     summary.UptimePercent(),
		ScheduledChecks:   summary.ScheduledChecks,
		SuccessChecks:     summary.SuccessChecks,
		DegradedChecks:    summary.DegradedChecks,
		FailureChecks:     summary.FailureChecks,
		MaintenanceChecks: summary.MaintenanceChecks,
		ManualChecks:      summary.ManualChecks,
		AvgResponseTimeMs: summary.AvgResponseMs(),
	}})
}

// parseLimit, `limit` parametresini çözer (boşsa servisin varsayılanı).
func parseLimit(v *validator, raw string) int32 {
	if raw == "" {
		return 0 // servis varsayılanı uygular
	}

	limit, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || limit < 1 {
		v.fail("limit", "pozitif bir tam sayı olmalı")
		return 0
	}

	// Üst sınırı AŞAN değer hata değil: servis kırpıyor (results.MaxListLimit).
	// Sınır sunucunun kendini korumasıdır, istemcinin uyması gereken bir
	// kural değil — 1000 isteyen istemci 200 alıyor.
	return int32(limit)
}

// parseTimeParam, RFC 3339 zaman damgasını çözer.
func parseTimeParam(v *validator, field, raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		v.fail(field, "RFC 3339 biçiminde bir zaman olmalı (2026-08-12T10:00:00Z)")
		return time.Time{}, false
	}

	return parsed, true
}
