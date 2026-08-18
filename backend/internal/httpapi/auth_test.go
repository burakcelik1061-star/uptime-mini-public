package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/results"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/subscribers"
)

const (
	testJWTSecret = "test-imzalama-anahtari-32-karakterden-uzun"
	testPassword  = "gecerli-parola-123"
	testEmail     = "ali@example.com"
)

// fakeUserStore, UserStore'un bellek içi karşılığı.
//
// Gerçek veritabanı yerine bu kullanılıyor: handler'ların işi (doğrulama,
// hash'leme, token üretme, hata çevirimi) veritabanı olmadan da tam olarak
// ölçülebiliyor. SQL'in kendisi 7.1'in integration testlerinde ölçülecek.
type fakeUserStore struct {
	byEmail map[string]store.User
	byID    map[uuid.UUID]store.User
	// failWith, veritabanı hatası yolunu ölçmek için.
	failWith error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byEmail: map[string]store.User{},
		byID:    map[uuid.UUID]store.User{},
	}
}

func (f *fakeUserStore) CreateUser(_ context.Context, arg store.CreateUserParams) (store.User, error) {
	if f.failWith != nil {
		return store.User{}, f.failWith
	}

	if _, exists := f.byEmail[arg.Email]; exists {
		// Postgres'in gerçekte döndürdüğü hatanın aynısı: handler'ın çeviri
		// yolu (postgres.IsUniqueViolation) gerçekten test ediliyor.
		return store.User{}, &pgconn.PgError{
			Code:           "23505",
			ConstraintName: usersEmailUniqueConstraint,
			Message:        `duplicate key value violates unique constraint "users_email_key"`,
		}
	}

	user := store.User{
		ID:           uuid.New(),
		Email:        arg.Email,
		PasswordHash: arg.PasswordHash,
		Name:         arg.Name,
		CreatedAt:    time.Now().UTC(),
	}
	f.byEmail[user.Email] = user
	f.byID[user.ID] = user

	return user, nil
}

func (f *fakeUserStore) GetUserByEmail(_ context.Context, email string) (store.User, error) {
	if f.failWith != nil {
		return store.User{}, f.failWith
	}

	user, ok := f.byEmail[email]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}

	return user, nil
}

func (f *fakeUserStore) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	if f.failWith != nil {
		return store.User{}, f.failWith
	}

	user, ok := f.byID[id]
	if !ok {
		return store.User{}, pgx.ErrNoRows
	}

	return user, nil
}

// testHasher, üretimdeki 19 MiB'lik maliyeti her testte ödememek için ucuz
// parametreler kullanır.
func testHasher() auth.Hasher {
	return auth.Hasher{Params: auth.Argon2Params{
		MemoryKiB:   64,
		Iterations:  1,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}}
}

// newTestDeps, GERÇEK router'ı kurmak için gereken tüm bağımlılıkların
// test karşılığı. Testler kendi "yarım uygulama" kopyasını tutmuyor: prod'da
// olan bir middleware testte eksik kalmasın (bkz. router.go).
func newTestDeps(t *testing.T) Deps {
	t.Helper()

	realtimeHub := realtime.NewHub(slog.New(slog.NewTextHandler(io.Discard, nil)))

	return Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		StartedAt:     time.Now(),
		DB:            stubPinger{},
		Users:         newFakeUserStore(),
		Hasher:        testHasher(),
		Tokens:        auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute),
		Sessions:      newFakeSessionService(),
		Orgs:          newFakeOrgService(),
		Monitors:      newFakeMonitorService(),
		Checks:        &fakeCheckRunner{result: successResult()},
		Results:       results.NewService(newFakeResultStore(), slog.New(slog.NewTextHandler(io.Discard, nil))),
		Incidents:     newFakeIncidentService(),
		Maintenance:   newFakeMaintenanceService(),
		StatusPages:   newFakeStatusPageService(),
		Notifications: newFakeNotificationService(),
		// Abonelik uçları (6.4) OTURUMSUZ ve hız sınırının en hassas yüzeyi
		// (7.4): sahtesi olmadan sınır testleri handler'a hiç ulaşamadan
		// panikle 500'e düşerdi ve "429 mü değil mi" sorusu ölçülemezdi.
		Subscribers: stubSubscriberService{},
		// Canlı yayın (6.1): hub gerçeği, taşıma yolu sahte. Sahte yayıncı
		// Postgres'e gitmek yerine olayı doğrudan hub'a veriyor — taşımanın
		// kendisi gerçek veritabanına karşı internal/realtime'da ölçülüyor,
		// buradaki testlerin konusu bağlantı ve yetkilendirme.
		Realtime: realtimeHub,
		Tickets:  auth.NewTicketService([]byte(testJWTSecret), auth.DefaultTicketTTL),
		Notifier: &loopbackNotifier{hub: realtimeHub},
		// Manuel incident hareketlerinin duyurusu (6.2). Sahte kapı: hangi
		// kanalların hedeflendiğini kaydediyor, sızıntı kuralının kendisi
		// internal/statuspage'de gerçek veritabanına karşı ölçülüyor.
		Broadcaster: &recordingBroadcaster{},
		// Tarayıcı testleri için tek izinli origin (bkz. cors_test.go).
		WebOrigins: []string{testWebOrigin},
	}
}

// stubSubscriberService, abonelik uçlarının sınır testleri için en küçük
// karşılık: her çağrı "bulunamadı" diyor. Enumeration davranışının kendisi
// internal/subscribers'da GERÇEK veritabanına karşı ölçülüyor (6.4).
type stubSubscriberService struct{}

func (stubSubscriberService) Subscribe(context.Context, string, string) error { return nil }

func (stubSubscriberService) Confirm(context.Context, string) error {
	return subscribers.ErrNotFound
}

func (stubSubscriberService) Unsubscribe(context.Context, string) (string, error) {
	return "", subscribers.ErrNotFound
}

func newAuthTestRouter(t *testing.T) (http.Handler, *fakeUserStore) {
	t.Helper()

	router, users, _ := newAuthTestRouterWithSessions(t)

	return router, users
}

func newAuthTestRouterWithSessions(t *testing.T) (http.Handler, *fakeUserStore, *fakeSessionService) {
	t.Helper()

	deps := newTestDeps(t)
	users, ok := deps.Users.(*fakeUserStore)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	sessionSvc, ok := deps.Sessions.(*fakeSessionService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	return NewRouter(deps), users, sessionSvc
}

func do(t *testing.T, router http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

func postJSON(t *testing.T, router http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	return do(t, router, http.MethodPost, path, body, nil)
}

func decodeAuthResponse(t *testing.T, rec *httptest.ResponseRecorder) authResponse {
	t.Helper()

	var body authResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, rec.Body.String())
	}

	return body
}

func decodeErrorResponse(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()

	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("hata gövdesi çözümlenemedi: %v (%s)", err, rec.Body.String())
	}

	return body
}

func registerBody(email, password, name string) string {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password, "name": name})

	return string(payload)
}

func loginBody(email, password string) string {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})

	return string(payload)
}

// --- Kayıt -------------------------------------------------------------------

func TestRegisterCreatesUserAndReturnsToken(t *testing.T) {
	router, users := newAuthTestRouter(t)

	rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali Veli"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	body := decodeAuthResponse(t, rec)
	if body.User.Email != testEmail || body.User.Name != "Ali Veli" {
		t.Errorf("kullanıcı = %+v", body.User)
	}
	if body.AccessToken == "" {
		t.Fatal("accessToken boş")
	}
	if body.ExpiresIn != int64((15 * time.Minute).Seconds()) {
		t.Errorf("expiresIn = %d", body.ExpiresIn)
	}

	// Token gerçekten bu kullanıcıya ait olmalı.
	identity, err := auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute).Verify(body.AccessToken)
	if err != nil {
		t.Fatalf("üretilen token doğrulanamadı: %v", err)
	}
	if identity.UserID.String() != body.User.ID {
		t.Errorf("token'daki kullanıcı = %s, beklenen %s", identity.UserID, body.User.ID)
	}

	// Parola düz metin saklanmamalı; saklanan hash doğrulanabilmeli.
	stored := users.byEmail[testEmail]
	if stored.PasswordHash == testPassword {
		t.Fatal("parola düz metin olarak saklandı")
	}
	if err := testHasher().Verify(stored.PasswordHash, testPassword); err != nil {
		t.Errorf("saklanan hash doğrulanamadı: %v", err)
	}
}

// Kayıt cevabında parola hash'i ASLA görünmemeli. store.User'ı doğrudan
// serileştirmek bu hatayı sessizce yapardı; ayrı bir userResponse tipi
// olmasının sebebi bu.
func TestResponsesNeverLeakPasswordHash(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	register := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali Veli"))
	token := decodeAuthResponse(t, register).AccessToken

	login := postJSON(t, router, "/v1/auth/login", loginBody(testEmail, testPassword))
	me := do(t, router, http.MethodGet, "/v1/me", "", map[string]string{"Authorization": "Bearer " + token})

	for name, rec := range map[string]*httptest.ResponseRecorder{"register": register, "login": login, "me": me} {
		body := rec.Body.String()
		for _, needle := range []string{"$argon2id$", "passwordHash", "password_hash", testPassword} {
			if strings.Contains(body, needle) {
				t.Errorf("%s cevabı %q içeriyor: %s", name, needle, body)
			}
		}
	}
}

// "Ali@Example.COM" ile kaydolan biri "ali@example.com" ile giriş
// yapabilmeli: normalleştirme tek yerde ve her iki uçta da aynı.
func TestRegisterNormalizesEmail(t *testing.T) {
	router, users := newAuthTestRouter(t)

	rec := postJSON(t, router, "/v1/auth/register", registerBody("  Ali@Example.COM ", testPassword, "  Ali Veli  "))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	if _, ok := users.byEmail[testEmail]; !ok {
		t.Fatalf("email normalleştirilmeden saklandı: %v", users.byEmail)
	}
	if got := decodeAuthResponse(t, rec).User.Name; got != "Ali Veli" {
		t.Errorf("name = %q, boşluklar kırpılmalıydı", got)
	}

	login := postJSON(t, router, "/v1/auth/login", loginBody("ALI@EXAMPLE.COM", testPassword))
	if login.Code != http.StatusOK {
		t.Errorf("farklı harf büyüklüğüyle giriş yapılamadı: %d (%s)", login.Code, login.Body.String())
	}
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	if rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali")); rec.Code != http.StatusCreated {
		t.Fatalf("ilk kayıt başarısız: %d", rec.Code)
	}

	// Harf büyüklüğü farklı olsa da aynı hesap.
	rec := postJSON(t, router, "/v1/auth/register", registerBody("ALI@example.com", testPassword, "Başkası"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, beklenen 409 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeEmailTaken {
		t.Errorf("code = %q, beklenen %q", code, codeEmailTaken)
	}
}

func TestRegisterValidatesFields(t *testing.T) {
	cases := map[string]struct {
		body  string
		field string
	}{
		"email yok":       {registerBody("", testPassword, "Ali"), "email"},
		"email biçimsiz":  {registerBody("ali-at-example", testPassword, "Ali"), "email"},
		"email alansız":   {registerBody("ali@localhost", testPassword, "Ali"), "email"},
		"parola yok":      {registerBody(testEmail, "", "Ali"), "password"},
		"parola kısa":     {registerBody(testEmail, "kisa12", "Ali"), "password"},
		"parola çok uzun": {registerBody(testEmail, strings.Repeat("x", auth.MaxPasswordBytes+1), "Ali"), "password"},
		"isim yok":        {registerBody(testEmail, testPassword, "   "), "name"},
		"isim çok uzun":   {registerBody(testEmail, testPassword, strings.Repeat("a", 121)), "name"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router, users := newAuthTestRouter(t)

			rec := postJSON(t, router, "/v1/auth/register", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
			}

			body := decodeErrorResponse(t, rec)
			if body.Error.Code != codeValidationFailed {
				t.Errorf("code = %q", body.Error.Code)
			}

			var found bool
			for _, field := range body.Error.Fields {
				if field.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("hatalı alan %q bildirilmedi: %+v", tc.field, body.Error.Fields)
			}

			if len(users.byEmail) != 0 {
				t.Error("doğrulama başarısızken kullanıcı oluşturuldu")
			}
		})
	}
}

// Uzun parola (72 byte üstü) REDDEDİLMİYOR: bcrypt'in sınırı bu projede yok.
func TestRegisterAcceptsPasswordLongerThanBcryptLimit(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	long := strings.Repeat("uzun-parola-", 10) // 120 byte
	if rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, long, "Ali")); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	if rec := postJSON(t, router, "/v1/auth/login", loginBody(testEmail, long)); rec.Code != http.StatusOK {
		t.Fatalf("uzun parolayla giriş yapılamadı: %d", rec.Code)
	}

	// İlk 72 byte'ı aynı olan parola girişi REDDEDİLMELİ.
	if rec := postJSON(t, router, "/v1/auth/login", loginBody(testEmail, long[:72])); rec.Code != http.StatusUnauthorized {
		t.Fatalf("kırpılmış parola kabul edildi: %d", rec.Code)
	}
}

// --- Gövde doğrulama sözleşmesi ----------------------------------------------

func TestRequestBodyContract(t *testing.T) {
	cases := map[string]struct {
		body        string
		contentType string
		status      int
		code        string
	}{
		"bozuk json":          {`{"email":`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		"boş gövde":           {``, "application/json", http.StatusBadRequest, codeInvalidJSON},
		"tanımsız alan":       {`{"email":"a@b.co","password":"12345678","name":"A","rol":"admin"}`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		"yanlış alan tipi":    {`{"email":123,"password":"12345678","name":"A"}`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		"iki nesne":           {`{"email":"a@b.co","password":"12345678","name":"A"}{}`, "application/json", http.StatusBadRequest, codeInvalidJSON},
		"yanlış content-type": {`{"email":"a@b.co","password":"12345678","name":"A"}`, "text/plain", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		// BAŞLIK HİÇ YOKSA DA REDDEDİLİYOR (7.5). Sözleşme 1.1'den beri
		// "Content-Type zorunlu" diyordu ama kod yalnızca başlık VARSA
		// doğruluyordu; başlıksız bir istek gövdesi sessizce ayrıştırılıyordu.
		"content-type yok": {`{"email":"a@b.co","password":"12345678","name":"A"}`, "", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		// Form biçimleri de reddediliyor: tarayıcı formunun gönderebildiği üç
		// medya tipi bunlar ve hiçbiri JSON uçlarına giremiyor.
		"form gövdesi":    {`email=a@b.co`, "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		"multipart gövde": {`--x`, "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType, codeUnsupportedMedia},
		"çok büyük gövde": {
			`{"email":"a@b.co","password":"12345678","name":"` + strings.Repeat("a", maxRequestBody) + `"}`,
			"application/json", http.StatusRequestEntityTooLarge, codePayloadTooLarge,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			router, _ := newAuthTestRouter(t)

			rec := do(t, router, http.MethodPost, "/v1/auth/register", tc.body,
				map[string]string{"Content-Type": tc.contentType})

			if rec.Code != tc.status {
				t.Fatalf("status = %d, beklenen %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if code := decodeErrorResponse(t, rec).Error.Code; code != tc.code {
				t.Errorf("code = %q, beklenen %q", code, tc.code)
			}
		})
	}
}

// Hata zarfı her yerde aynı: kod, mesaj ve isteği loga bağlayan requestId.
func TestErrorEnvelopeIsConsistent(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	cases := map[string]*httptest.ResponseRecorder{
		"bilinmeyen yol": do(t, router, http.MethodGet, "/v1/yok", "", nil),
		"yanlış metot":   do(t, router, http.MethodGet, "/v1/auth/login", "", nil),
		"doğrulama":      postJSON(t, router, "/v1/auth/register", registerBody("", "", "")),
		"kimlik yok":     do(t, router, http.MethodGet, "/v1/me", "", nil),
	}

	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("content-type = %q", ct)
			}

			body := decodeErrorResponse(t, rec)
			if body.Error.Code == "" || body.Error.Message == "" {
				t.Errorf("hata gövdesi eksik: %+v", body)
			}
			if body.Error.RequestID == "" {
				t.Error("requestId boş")
			}
		})
	}
}

func TestMethodNotAllowedUsesStandardBody(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	rec := do(t, router, http.MethodGet, "/v1/auth/login", "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, beklenen 405", rec.Code)
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeMethodNotAllowed {
		t.Errorf("code = %q", code)
	}
}

// --- Giriş -------------------------------------------------------------------

func TestLoginReturnsToken(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	if rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali")); rec.Code != http.StatusCreated {
		t.Fatalf("kayıt başarısız: %d", rec.Code)
	}

	rec := postJSON(t, router, "/v1/auth/login", loginBody(testEmail, testPassword))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	if decodeAuthResponse(t, rec).AccessToken == "" {
		t.Error("accessToken boş")
	}
}

// Kayıtlı olmayan e-posta ile yanlış parola AYNI cevabı vermeli: fark,
// hangi e-postaların sistemde olduğunu tek tek öğrenmeye izin verirdi.
func TestLoginDoesNotRevealWhetherEmailExists(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	if rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali")); rec.Code != http.StatusCreated {
		t.Fatalf("kayıt başarısız: %d", rec.Code)
	}

	wrongPassword := postJSON(t, router, "/v1/auth/login", loginBody(testEmail, "bambaska-parola-1"))
	unknownEmail := postJSON(t, router, "/v1/auth/login", loginBody("yok@example.com", testPassword))

	if wrongPassword.Code != http.StatusUnauthorized || unknownEmail.Code != http.StatusUnauthorized {
		t.Fatalf("status'lar = %d / %d, ikisi de 401 olmalı", wrongPassword.Code, unknownEmail.Code)
	}

	first := decodeErrorResponse(t, wrongPassword).Error
	second := decodeErrorResponse(t, unknownEmail).Error

	if first.Code != codeInvalidCredentials || second.Code != codeInvalidCredentials {
		t.Errorf("kodlar = %q / %q", first.Code, second.Code)
	}
	if first.Message != second.Message {
		t.Errorf("mesajlar farklı: %q vs %q", first.Message, second.Message)
	}
	if len(first.Fields) != 0 || len(second.Fields) != 0 {
		t.Error("giriş hatasında alan ayrıntısı dönmemeli")
	}
}

func TestLoginRejectsEmptyCredentials(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	for name, body := range map[string]string{
		"email yok":  loginBody("", testPassword),
		"parola yok": loginBody(testEmail, ""),
	} {
		t.Run(name, func(t *testing.T) {
			rec := postJSON(t, router, "/v1/auth/login", body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, beklenen 401", rec.Code)
			}
			// Girişte alan doğrulama ayrıntısı verilmiyor.
			if code := decodeErrorResponse(t, rec).Error.Code; code != codeInvalidCredentials {
				t.Errorf("code = %q", code)
			}
		})
	}
}

// --- Korumalı uç ve middleware ------------------------------------------------

func TestMeReturnsAuthenticatedUser(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	register := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali Veli"))
	registered := decodeAuthResponse(t, register)

	rec := do(t, router, http.MethodGet, "/v1/me", "", map[string]string{
		"Authorization": "Bearer " + registered.AccessToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		User userResponse `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}

	if body.User.ID != registered.User.ID || body.User.Email != testEmail {
		t.Errorf("kullanıcı = %+v, beklenen %+v", body.User, registered.User)
	}
}

func TestMeRejectsMissingOrMalformedAuthorization(t *testing.T) {
	router, _ := newAuthTestRouter(t)

	headers := map[string]string{
		"başlık yok":      "",
		"başka şema":      "Basic YWxpOjEyMw==",
		"şema tek başına": "Bearer",
		"boş token":       "Bearer    ",
		"çöp token":       "Bearer abc.def.ghi",
	}

	for name, value := range headers {
		t.Run(name, func(t *testing.T) {
			var h map[string]string
			if value != "" {
				h = map[string]string{"Authorization": value}
			}

			rec := do(t, router, http.MethodGet, "/v1/me", "", h)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if code := decodeErrorResponse(t, rec).Error.Code; code != codeUnauthorized {
				t.Errorf("code = %q", code)
			}
		})
	}
}

// Süresi dolmuş token ayrı bir kodla bildiriliyor: istemci yeniden giriş
// istemek yerine token tazeleyebileceğini bilsin (Faz 1.2).
func TestMeReportsExpiredTokenSeparately(t *testing.T) {
	router, users := newAuthTestRouter(t)

	user, err := users.CreateUser(t.Context(), store.CreateUserParams{
		Email: testEmail, PasswordHash: "x", Name: "Ali",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	// Aynı sırla ama çok kısa ömürle imzalanmış bir token.
	shortLived := auth.NewTokenService([]byte(testJWTSecret), time.Millisecond)
	token, err := shortLived.Issue(user.ID)
	if err != nil {
		t.Fatalf("token üretilemedi: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	rec := do(t, router, http.MethodGet, "/v1/me", "", map[string]string{"Authorization": "Bearer " + token.Value})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401", rec.Code)
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeTokenExpired {
		t.Errorf("code = %q, beklenen %q", code, codeTokenExpired)
	}
}

func TestMeRejectsTokenSignedWithAnotherSecret(t *testing.T) {
	router, users := newAuthTestRouter(t)

	user, err := users.CreateUser(t.Context(), store.CreateUserParams{
		Email: testEmail, PasswordHash: "x", Name: "Ali",
	})
	if err != nil {
		t.Fatalf("kullanıcı oluşturulamadı: %v", err)
	}

	forged, err := auth.NewTokenService([]byte("bambaska-bir-imzalama-anahtari-32+"), time.Hour).Issue(user.ID)
	if err != nil {
		t.Fatalf("token üretilemedi: %v", err)
	}

	rec := do(t, router, http.MethodGet, "/v1/me", "", map[string]string{"Authorization": "Bearer " + forged.Value})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
	}
}

// Token geçerli ama kullanıcı artık yok: kimlik token'dan değil
// veritabanından okunduğu için bu durum yakalanabiliyor.
func TestMeRejectsTokenOfDeletedUser(t *testing.T) {
	router, users := newAuthTestRouter(t)

	register := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	token := decodeAuthResponse(t, register).AccessToken

	users.byEmail = map[string]store.User{}
	users.byID = map[uuid.UUID]store.User{}

	rec := do(t, router, http.MethodGet, "/v1/me", "", map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
	}
}

// Veritabanı hatası istemciye iç ayrıntı sızdırmamalı.
func TestDatabaseFailureReturnsGenericError(t *testing.T) {
	router, users := newAuthTestRouter(t)
	users.failWith = pgx.ErrTxClosed

	rec := postJSON(t, router, "/v1/auth/register", registerBody(testEmail, testPassword, "Ali"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}

	body := decodeErrorResponse(t, rec)
	if body.Error.Code != codeInternalServerError {
		t.Errorf("code = %q", body.Error.Code)
	}
	if strings.Contains(rec.Body.String(), pgx.ErrTxClosed.Error()) {
		t.Error("iç hata metni istemciye sızdı")
	}
}
