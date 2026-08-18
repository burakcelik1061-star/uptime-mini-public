package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// maxRequestBody, JSON gövdeleri için üst sınır.
//
// Sınırsız gövde, tek bir istekle belleği tüketmenin en ucuz yolu. Auth
// uçlarının gövdesi birkaç yüz byte; 64 KiB fazlasıyla cömert.
const maxRequestBody = 64 << 10

// decodeJSON, istek gövdesini çözümler ve gövde doğrulamasının ORTAK
// kurallarını uygular (Faz 1.1'de standartlaştı):
//
//   - Content-Type application/json olmalı.
//   - Gövde maxRequestBody'yi aşamaz.
//   - TANIMSIZ ALANLAR reddedilir: "passwordd" yazan bir istemci sessizce
//     boş parolayla devam etmesin; yazım hatası istekte görünsün.
//   - Gövdede tek bir JSON değeri olmalı (arkasına iliştirilmiş ikinci bir
//     nesne yok sayılmasın).
//
// Hata döndüğünde çağıran taraf onu doğrudan cevaba yazabilir.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *apiError {
	// BAŞLIK ZORUNLU, "varsa doğru olsun" DEĞİL (7.5'in düzeltmesi).
	//
	// Eski hâlde başlık hiç gönderilmediğinde gövde sessizce ayrıştırılıyordu;
	// oysa 1.1'in sözleşmesi (docs/1.1-auth.md → "Gövde kuralları") başlığı
	// ZORUNLU sayıyor. Yazılı davranışla gerçek davranışın ayrışması tek
	// başına bir hata — ayrıca başlığı şart koşmak ucuz bir savunma katmanı:
	// tarayıcı formları yalnızca üç medya tipi gönderebiliyor ve üçü de burada
	// reddediliyor, yani JSON uçları basit çapraz-origin isteklere kapalı
	// kalıyor.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &apiError{
			status:  http.StatusUnsupportedMediaType,
			code:    codeUnsupportedMedia,
			message: "Content-Type application/json olmalı",
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return decodeError(err)
	}

	// Decode yalnızca İLK değeri okur; gövdenin geri kalanı sessizce
	// yok sayılmamalı.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: "gövde tek bir JSON nesnesi olmalı",
		}
	}

	return nil
}

func decodeError(err error) *apiError {
	var (
		syntaxErr   *json.SyntaxError
		typeErr     *json.UnmarshalTypeError
		maxBytesErr *http.MaxBytesError
	)

	switch {
	case errors.As(err, &maxBytesErr):
		return &apiError{
			status:  http.StatusRequestEntityTooLarge,
			code:    codePayloadTooLarge,
			message: fmt.Sprintf("gövde en fazla %d byte olabilir", maxRequestBody),
		}

	case errors.As(err, &syntaxErr):
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: fmt.Sprintf("gövde geçerli JSON değil (%d. byte)", syntaxErr.Offset),
		}

	case errors.As(err, &typeErr):
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: "alan tipi beklenenden farklı",
			fields: []FieldError{{
				Field:   typeErr.Field,
				Message: fmt.Sprintf("%s bekleniyordu", typeErr.Type.String()),
			}},
		}

	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: "gövde boş veya yarım",
		}

	// encoding/json bunun için tipli bir hata vermiyor; metin eşleştirmek
	// tek yol.
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)

		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: "gövdede tanımsız alan var",
			fields:  []FieldError{{Field: field, Message: "bu alan bilinmiyor"}},
		}

	default:
		return &apiError{
			status:  http.StatusBadRequest,
			code:    codeInvalidJSON,
			message: "gövde çözümlenemedi",
		}
	}
}

// nullable, JSON'da ÜÇ DURUMU ayırt eder: alan gönderilmedi · `null`
// gönderildi · değer gönderildi.
//
// Neden gerekli (2.2): kısmi güncellemede (PATCH) diğer alanlarda nil
// işaretçi "dokunma" demek. NULL OLABİLEN bir alanda (monitor'ün "yavaş"
// eşiği) nil aynı zamanda geçerli bir DEĞER — "eşiği kaldır". Tek bir
// işaretçi ikisini ayıramaz; bu tip UnmarshalJSON çağrılıp çağrılmadığına
// bakarak ayırıyor.
type nullable[T any] struct {
	// Present, alanın gövdede olduğunu söyler.
	Present bool
	// Value nil ise açıkça `null` gönderilmiş demektir.
	Value *T
}

func (n *nullable[T]) UnmarshalJSON(data []byte) error {
	// Buraya girilmesi alanın gövdede OLDUĞU anlamına geliyor;
	// encoding/json eksik alanlar için UnmarshalJSON çağırmıyor.
	n.Present = true

	if string(data) == "null" {
		n.Value = nil
		return nil
	}

	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	n.Value = &value

	return nil
}

// validator, alan doğrulama hatalarını toplar.
//
// Neden hazır bir doğrulama kütüphanesi yok: struct etiketleriyle yazılan
// kurallar derleyici tarafından denetlenmiyor ve hata mesajlarını dilimize
// çevirmek ayrı bir katman gerektiriyor. Buradaki kurallar birkaç satır ve
// okunduğu gibi çalışıyor.
type validator struct {
	fields []FieldError
}

// fail, alanı hatalı olarak işaretler.
func (v *validator) fail(field, message string) {
	v.fields = append(v.fields, FieldError{Field: field, Message: message})
}

// err, toplanan hatalardan bir cevap üretir (hata yoksa nil).
//
// 422 (Unprocessable Content) kullanılıyor, 400 değil: gövde geçerli JSON
// ama ALANLAR kabul edilebilir değil. İstemci ikisini `code` ile de ayırt
// edebilir; ayrım yine de HTTP seviyesinde doğru olsun.
func (v *validator) err() *apiError {
	if len(v.fields) == 0 {
		return nil
	}

	return &apiError{
		status:  http.StatusUnprocessableEntity,
		code:    codeValidationFailed,
		message: "gövde doğrulanamadı",
		fields:  v.fields,
	}
}

const (
	maxEmailLength = 254 // RFC 5321'in yol (path) sınırı
	maxNameLength  = 120
)

// normalizeEmail, email'i saklama/karşılaştırma biçimine getirir.
//
// Normalleştirme TEK YERDE: kayıt da giriş de aynı fonksiyondan geçiyor,
// aksi hâlde "Ali@X.com" ile kayıt olan biri "ali@x.com" ile giriş
// yapamazdı.
func normalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validateEmail, biçim kontrolü yapar.
//
// Bu kontrol adresin GERÇEK olduğunu söylemez — onun tek yolu doğrulama
// e-postası göndermek (bu projede dış e-posta servisi yok, bkz. docs/0.0-constraints.md §2).
// Amaç, yazım hatalarını erken yakalamak.
func validateEmail(v *validator, email string) {
	switch {
	case email == "":
		v.fail("email", "zorunlu")
	case len(email) > maxEmailLength:
		v.fail("email", fmt.Sprintf("en fazla %d karakter olabilir", maxEmailLength))
	default:
		addr, err := mail.ParseAddress(email)
		// ParseAddress "Ad <a@b.c>" biçimini de kabul ediyor; sadece adresin
		// kendisini istiyoruz.
		if err != nil || addr.Address != email {
			v.fail("email", "geçerli bir e-posta adresi olmalı")
			return
		}

		// ParseAddress "ali@localhost" gibi noktasız alan adlarını da geçerli
		// sayıyor; kullanıcı hesabı için gerçek bir alan adı bekliyoruz.
		_, domain, _ := strings.Cut(email, "@")
		if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
			v.fail("email", "geçerli bir e-posta adresi olmalı")
		}
	}
}

func validateName(v *validator, name string) {
	switch {
	case name == "":
		v.fail("name", "zorunlu")
	case utf8.RuneCountInString(name) > maxNameLength:
		v.fail("name", fmt.Sprintf("en fazla %d karakter olabilir", maxNameLength))
	}
}
