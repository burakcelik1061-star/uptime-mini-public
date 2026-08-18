package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// Hata gövdesi formatı BURADA standartlaşıyor (Faz 1.1). Kural: api'nin
// döndürdüğü HER hata aynı zarfı kullanır —
//
//	{"error": {"code": "...", "message": "...", "fields": [...], "requestId": "..."}}
//
// Neden `code` var: istemci karar verirken İNSAN metnine bakmak zorunda
// kalmasın. Mesaj metni değiştiğinde (veya çevrildiğinde) frontend
// kırılmamalı; makine tarafı `code` okur.
//
// Neden `requestId` var: kullanıcının gördüğü hata ile sunucu logundaki satır
// birbirine bağlanabilsin (RequestID middleware'i zaten her isteğe kimlik
// veriyor).
const (
	codeInvalidJSON          = "invalid_json"
	codeValidationFailed     = "validation_failed"
	codeUnsupportedMedia     = "unsupported_media_type"
	codePayloadTooLarge      = "payload_too_large"
	codeInvalidCredentials   = "invalid_credentials"
	codeInvalidRefreshToken  = "invalid_refresh_token"
	codeEmailTaken           = "email_taken"
	codeUnauthorized         = "unauthorized"
	codeForbidden            = "forbidden"
	codeTokenExpired         = "token_expired"
	codeNotFound             = "not_found"
	codeCheckNotRun          = "check_not_run"
	codeIncidentResolved     = "incident_resolved"
	codeMaintenanceStarted   = "maintenance_started"
	codeChannelExists        = "channel_exists"
	codeInvitationExpired    = "invitation_expired"
	codeResetTokenExpired    = "reset_token_expired"
	codePlanLimitReached     = "plan_limit_reached"
	codePlanUnchanged        = "plan_unchanged"
	codeInvalidQuery         = "invalid_query"
	codeMethodNotAllowed     = "method_not_allowed"
	codeRateLimited          = "rate_limited"
	codeInternalServerError  = "internal_error"
	messageInternalServerErr = "beklenmeyen bir hata oluştu"
)

// ErrorResponse, hata cevaplarının gövdesi.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail, tek bir hatanın makine ve insan tarafı.
type ErrorDetail struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Fields    []FieldError `json:"fields,omitempty"`
	RequestID string       `json:"requestId,omitempty"`
}

// FieldError, gövde doğrulamasında tek bir alanın sorunu.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// apiError, handler'lar arasında taşınabilen HTTP'ye çevrilebilir hata.
type apiError struct {
	status  int
	code    string
	message string
	fields  []FieldError
}

func (e *apiError) Error() string { return e.code + ": " + e.message }

// write, hatayı standart zarfla cevaba yazar.
func (e *apiError) write(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, e.status, ErrorResponse{Error: ErrorDetail{
		Code:      e.code,
		Message:   e.message,
		Fields:    e.fields,
		RequestID: middleware.GetReqID(r.Context()),
	}})
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	(&apiError{status: status, code: code, message: message}).write(w, r)
}

// writeFieldError, TEK bir alana bağlı doğrulama hatası yazar (422).
//
// Servis katmanından gelen hataların (2.1'den beri doğrulama orada) hangi
// alana ait olduğunu handler biliyor; `validator` ise gövdeyi kendisi
// ayrıştıran yollar için. İkisi de aynı zarfa (`fields`) çıkıyor, çünkü
// arayüz mesajı doğrudan ilgili girdinin altına yazıyor.
func writeFieldError(w http.ResponseWriter, r *http.Request, field, message string) {
	(&apiError{
		status:  http.StatusUnprocessableEntity,
		code:    codeValidationFailed,
		message: "gövde doğrulanamadı",
		fields:  []FieldError{{Field: field, Message: message}},
	}).write(w, r)
}

// writeInternalError, beklenmeyen hatalarda kullanılır: ayrıntı LOGA gider,
// istemciye yalnızca genel bir mesaj döner.
//
// Hata metnini istemciye yansıtmak, SQL hatalarından şema/sütun adlarına
// kadar iç bilgiyi dışarı sızdırırdı.
func writeInternalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, msg string, err error) {
	logger.Error(msg,
		slog.String("error", err.Error()),
		slog.String("path", r.URL.Path),
		slog.String("request_id", middleware.GetReqID(r.Context())))

	writeError(w, r, http.StatusInternalServerError, codeInternalServerError, messageInternalServerErr)
}

// notFoundHandler / methodNotAllowedHandler: chi'nin varsayılanları düz metin
// döner ("404 page not found"). Sözleşmenin "her hata aynı zarfta" kuralı
// yönlendirici seviyesinde de geçerli olmalı.
func notFoundHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, codeNotFound, "böyle bir uç yok")
	}
}

func methodNotAllowedHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusMethodNotAllowed, codeMethodNotAllowed, "bu uç için metot desteklenmiyor")
	}
}
