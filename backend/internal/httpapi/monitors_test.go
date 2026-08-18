package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/monitors"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/plans"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeMonitorService, monitor servisinin HTTP katmanı için sahtesi.
//
// SAKLAMA sahte, DOĞRULAMA gerçek: sahte servis monitors.Validate'i
// çağırıyor. Doğrulama kurallarını burada yeniden yazsaydım, handler'ın 422
// çevirimini kendi taklidime karşı test etmiş olurdum. SQL'in org izolasyonu
// ise burada değil, internal/monitors'un gerçek veritabanı testlerinde
// ölçülüyor.
type fakeMonitorService struct {
	// byOrg: orgID → monitorID → kayıt.
	byOrg    map[uuid.UUID]map[uuid.UUID]store.Monitor
	failWith error
	// now, listenin döndüğü an (8.6). Gerçek serviste veritabanından geliyor;
	// testler bayatlık senaryosunu buradan kuruyor.
	now time.Time
}

func newFakeMonitorService() *fakeMonitorService {
	return &fakeMonitorService{
		byOrg: map[uuid.UUID]map[uuid.UUID]store.Monitor{},
		now:   time.Now().UTC(),
	}
}

func (f *fakeMonitorService) Create(_ context.Context, orgID uuid.UUID, in monitors.Settings) (store.Monitor, error) {
	if f.failWith != nil {
		return store.Monitor{}, f.failWith
	}

	in = monitors.Normalize(in)
	if err := monitors.Validate(in); err != nil {
		return store.Monitor{}, err
	}

	status := store.MonitorStatusPending
	if !in.IsActive {
		status = store.MonitorStatusPaused
	}

	now := time.Now().UTC()
	monitor := store.Monitor{
		ID:                   uuid.New(),
		OrgID:                orgID,
		Name:                 in.Name,
		Url:                  in.URL,
		Method:               in.Method,
		CheckIntervalSeconds: in.CheckIntervalSeconds,
		TimeoutMs:            in.TimeoutMs,
		ExpectedStatusMin:    in.ExpectedStatusMin,
		ExpectedStatusMax:    in.ExpectedStatusMax,
		DegradedResponseMs:   in.DegradedResponseMs,
		FailureThreshold:     in.FailureThreshold,
		RecoveryThreshold:    in.RecoveryThreshold,
		IsActive:             in.IsActive,
		CurrentStatus:        status,
		StatusChangedAt:      now,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	if f.byOrg[orgID] == nil {
		f.byOrg[orgID] = map[uuid.UUID]store.Monitor{}
	}
	f.byOrg[orgID][monitor.ID] = monitor

	return monitor, nil
}

func (f *fakeMonitorService) List(_ context.Context, orgID uuid.UUID) (monitors.Listing, error) {
	if f.failWith != nil {
		return monitors.Listing{}, f.failWith
	}

	rows := make([]store.Monitor, 0, len(f.byOrg[orgID]))
	for _, monitor := range f.byOrg[orgID] {
		rows = append(rows, monitor)
	}

	// `now` sahtede de AÇIKÇA veriliyor: gerçek serviste veritabanından
	// geliyor ve bayatlık kararı ona dayanıyor (8.6). Sahtenin `time.Now()`a
	// düşmesi, testin ölçtüğü şeyi belirsizleştirirdi.
	return monitors.Listing{Monitors: rows, Now: f.now}, nil
}

func (f *fakeMonitorService) Get(_ context.Context, orgID, monitorID uuid.UUID) (store.Monitor, error) {
	if f.failWith != nil {
		return store.Monitor{}, f.failWith
	}

	monitor, ok := f.byOrg[orgID][monitorID]
	if !ok {
		return store.Monitor{}, monitors.ErrNotFound
	}

	return monitor, nil
}

func (f *fakeMonitorService) Update(
	ctx context.Context, orgID, monitorID uuid.UUID, patch monitors.Patch,
) (store.Monitor, error) {
	if patch.IsEmpty() {
		return store.Monitor{}, &monitors.ValidationError{
			Problems: []monitors.Problem{{Message: "en az bir alan gönderilmeli"}},
		}
	}

	current, err := f.Get(ctx, orgID, monitorID)
	if err != nil {
		return store.Monitor{}, err
	}

	merged := monitors.SettingsOf(current)
	if patch.Name != nil {
		merged.Name = *patch.Name
	}
	if patch.URL != nil {
		merged.URL = *patch.URL
	}
	if patch.Method != nil {
		merged.Method = *patch.Method
	}
	if patch.CheckIntervalSeconds != nil {
		merged.CheckIntervalSeconds = *patch.CheckIntervalSeconds
	}
	if patch.TimeoutMs != nil {
		merged.TimeoutMs = *patch.TimeoutMs
	}
	if patch.ExpectedStatusMin != nil {
		merged.ExpectedStatusMin = *patch.ExpectedStatusMin
	}
	if patch.ExpectedStatusMax != nil {
		merged.ExpectedStatusMax = *patch.ExpectedStatusMax
	}
	if patch.FailureThreshold != nil {
		merged.FailureThreshold = *patch.FailureThreshold
	}
	if patch.RecoveryThreshold != nil {
		merged.RecoveryThreshold = *patch.RecoveryThreshold
	}
	if patch.IsActive != nil {
		merged.IsActive = *patch.IsActive
	}
	if patch.SetDegradedResponseMs {
		merged.DegradedResponseMs = patch.DegradedResponseMs
	}

	merged = monitors.Normalize(merged)
	if err := monitors.Validate(merged); err != nil {
		return store.Monitor{}, err
	}

	current.Name = merged.Name
	current.Url = merged.URL
	current.Method = merged.Method
	current.CheckIntervalSeconds = merged.CheckIntervalSeconds
	current.TimeoutMs = merged.TimeoutMs
	current.ExpectedStatusMin = merged.ExpectedStatusMin
	current.ExpectedStatusMax = merged.ExpectedStatusMax
	current.DegradedResponseMs = merged.DegradedResponseMs
	current.FailureThreshold = merged.FailureThreshold
	current.RecoveryThreshold = merged.RecoveryThreshold
	if merged.IsActive != current.IsActive {
		current.CurrentStatus = store.MonitorStatusPending
		if !merged.IsActive {
			current.CurrentStatus = store.MonitorStatusPaused
		}
	}
	current.IsActive = merged.IsActive
	current.UpdatedAt = time.Now().UTC()

	f.byOrg[orgID][monitorID] = current

	return current, nil
}

func (f *fakeMonitorService) Delete(_ context.Context, orgID, monitorID uuid.UUID) error {
	if f.failWith != nil {
		return f.failWith
	}

	if _, ok := f.byOrg[orgID][monitorID]; !ok {
		return monitors.ErrNotFound
	}

	delete(f.byOrg[orgID], monitorID)

	return nil
}

// --- Test kurulumu -----------------------------------------------------------

// monitorFixture, monitor uçlarının ortak kurulumu: aynı organizasyonda bir
// owner ve bir member, artı başka bir organizasyonun kullanıcısı.
type monitorFixture struct {
	router  http.Handler
	service *fakeMonitorService
	// orgs, planı ayarlayabilmek için (8.5).
	orgs     *fakeOrgService
	orgID    uuid.UUID
	ownerTok string
	// memberTok, salt-okunur rolü olan kullanıcının token'ı.
	memberTok string
	outsider  string
}

func newMonitorFixture(t *testing.T) monitorFixture {
	t.Helper()

	return newMonitorFixtureWithDeps(t, newTestDeps(t))
}

// newMonitorFixtureWithDeps, bağımlılıkları değiştirilmiş bir router kurar
// (bkz. checks_test.go: gerçek Checker ile uçtan uca deneme).
func newMonitorFixtureWithDeps(t *testing.T, deps Deps) monitorFixture {
	t.Helper()

	orgSvc, ok := deps.Orgs.(*fakeOrgService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	monitorSvc, ok := deps.Monitors.(*fakeMonitorService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	router := NewRouter(deps)

	ownerTok := authenticate(t, router, testEmail)
	memberTok := authenticate(t, router, "uye@example.com")
	outsider := authenticate(t, router, "yabanci@example.com")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	orgID, err := uuid.Parse(decodeOrganization(t, rec.Body.Bytes()).ID)
	if err != nil {
		t.Fatalf("org id çözümlenemedi: %v", err)
	}

	// Üyeyi organizasyona `member` rolüyle ekle (davet akışı MVP dışı).
	orgSvc.addMember(orgID, userIDOf(t, memberTok), store.OrgRoleMember)

	return monitorFixture{
		router:    router,
		service:   monitorSvc,
		orgs:      orgSvc,
		orgID:     orgID,
		ownerTok:  ownerTok,
		memberTok: memberTok,
		outsider:  outsider,
	}
}

// userIDOf, token'daki kullanıcı kimliğini çözer.
func userIDOf(t *testing.T, token string) uuid.UUID {
	t.Helper()

	identity, err := auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute).Verify(token)
	if err != nil {
		t.Fatalf("token doğrulanamadı: %v", err)
	}

	return identity.UserID
}

func (f monitorFixture) path(suffix string) string {
	return "/v1/orgs/" + f.orgID.String() + "/monitors" + suffix
}

const validMonitorBody = `{"name":"API","url":"https://example.com/health"}`

func decodeMonitor(t *testing.T, body []byte) monitorResponse {
	t.Helper()

	var wrapper struct {
		Monitor monitorResponse `json:"monitor"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Monitor
}

func decodeMonitors(t *testing.T, body []byte) []monitorResponse {
	t.Helper()

	var wrapper struct {
		Monitors []monitorResponse `json:"monitors"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Monitors
}

// createMonitor, testlerin kullandığı hazır bir monitor bırakır.
func (f monitorFixture) createMonitor(t *testing.T, body string) monitorResponse {
	t.Helper()

	rec := do(t, f.router, http.MethodPost, f.path(""), body, bearer(f.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("monitor oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	return decodeMonitor(t, rec.Body.Bytes())
}

// --- Yetki -------------------------------------------------------------------

func TestMonitorEndpointsRequireAuthentication(t *testing.T) {
	fixture := newMonitorFixture(t)

	for _, tt := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "", ""},
		{http.MethodPost, "", validMonitorBody},
		{http.MethodGet, "/" + uuid.NewString(), ""},
		{http.MethodPatch, "/" + uuid.NewString(), `{"name":"x"}`},
		{http.MethodDelete, "/" + uuid.NewString(), ""},
	} {
		rec := do(t, fixture.router, tt.method, fixture.path(tt.path), tt.body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, beklenen 401", tt.method, tt.path, rec.Code)
		}
	}
}

// Başka organizasyonun kullanıcısı monitor uçlarını hiç göremez: cevap
// üyelik middleware'inden geliyor, yani "böyle bir organizasyon yok".
func TestMonitorEndpointsAreOrgScoped(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	for _, tt := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "", ""},
		{http.MethodPost, "", validMonitorBody},
		{http.MethodGet, "/" + monitor.ID, ""},
		{http.MethodPatch, "/" + monitor.ID, `{"name":"ele geçirildi"}`},
		{http.MethodDelete, "/" + monitor.ID, ""},
	} {
		rec := do(t, fixture.router, tt.method, fixture.path(tt.path), tt.body, bearer(fixture.outsider))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, beklenen 404 (%s)", tt.method, tt.path, rec.Code, rec.Body.String())
		}
	}

	// Kayıt bozulmamış olmalı.
	if got := fixture.service.byOrg[fixture.orgID][uuid.MustParse(monitor.ID)]; got.Name != "API" {
		t.Errorf("monitor değişmiş: %+v", got)
	}
}

// `member` rolü OKUYABİLİR ama YAZAMAZ (0.1 §2: salt-okunur pano).
func TestMemberCanReadButNotWriteMonitors(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	reads := []struct {
		method string
		path   string
	}{
		{http.MethodGet, ""},
		{http.MethodGet, "/" + monitor.ID},
	}
	for _, tt := range reads {
		rec := do(t, fixture.router, tt.method, fixture.path(tt.path), "", bearer(fixture.memberTok))
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status = %d, beklenen 200 (%s)", tt.method, tt.path, rec.Code, rec.Body.String())
		}
	}

	writes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "", validMonitorBody},
		{http.MethodPatch, "/" + monitor.ID, `{"name":"yeni"}`},
		{http.MethodDelete, "/" + monitor.ID, ""},
	}
	for _, tt := range writes {
		rec := do(t, fixture.router, tt.method, fixture.path(tt.path), tt.body, bearer(fixture.memberTok))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, beklenen 403 (%s)", tt.method, tt.path, rec.Code, rec.Body.String())
		}
		if code := decodeErrorResponse(t, rec).Error.Code; code != codeForbidden {
			t.Errorf("%s %s: code = %q", tt.method, tt.path, code)
		}
	}

	// Yazma denemeleri gerçekten yazmamış olmalı.
	if len(fixture.service.byOrg[fixture.orgID]) != 1 {
		t.Errorf("kayıt sayısı = %d, beklenen 1", len(fixture.service.byOrg[fixture.orgID]))
	}
}

// --- Oluşturma ---------------------------------------------------------------

func TestCreateMonitorAppliesDefaults(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t, validMonitorBody)

	if monitor.Method != string(store.MonitorMethodGET) {
		t.Errorf("metot = %q, beklenen GET", monitor.Method)
	}
	// VARSAYILAN ARALIK PLANA GÖRE (8.5): sahte organizasyonun planı
	// yazılmamış, yani en dar plan (FREE) geçerli ve onun alt sınırı ürünün
	// 60 saniyelik varsayılanından YÜKSEK. Sabit 60 dönseydi, yeni bir FREE
	// kullanıcının hiçbir alan doldurmadan eklediği ilk monitor reddedilirdi.
	if want := plans.For(store.OrgPlanFree).MinCheckIntervalSeconds; monitor.CheckIntervalSeconds != want {
		t.Errorf("aralık = %d, beklenen %d", monitor.CheckIntervalSeconds, want)
	}
	if monitor.TimeoutMs != monitors.DefaultTimeoutMs {
		t.Errorf("timeout = %d", monitor.TimeoutMs)
	}
	if monitor.ExpectedStatusMin != 200 || monitor.ExpectedStatusMax != 299 {
		t.Errorf("status aralığı = %d-%d", monitor.ExpectedStatusMin, monitor.ExpectedStatusMax)
	}
	if !monitor.IsActive {
		t.Error("varsayılan aktif olmalı")
	}
	// İlk kontrol yapılana kadar 'pending'.
	if monitor.Status != string(store.MonitorStatusPending) {
		t.Errorf("durum = %q, beklenen pending", monitor.Status)
	}
}

// Sıfır değer ile "gönderilmedi" AYRI: `0` varsayılana düşmüyor, hata alıyor.
func TestCreateMonitorRejectsExplicitZero(t *testing.T) {
	fixture := newMonitorFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.path(""),
		`{"name":"API","url":"https://example.com","checkIntervalSeconds":0}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
	}

	fields := decodeErrorResponse(t, rec).Error.Fields
	if len(fields) != 1 || fields[0].Field != "checkIntervalSeconds" {
		t.Errorf("alan hataları = %+v", fields)
	}
}

func TestCreateMonitorValidationFailure(t *testing.T) {
	fixture := newMonitorFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.path(""),
		`{"name":"","url":"example.com"}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
	}

	body := decodeErrorResponse(t, rec)
	if body.Error.Code != codeValidationFailed {
		t.Errorf("code = %q", body.Error.Code)
	}

	seen := map[string]bool{}
	for _, f := range body.Error.Fields {
		seen[f.Field] = true
	}
	if !seen["name"] || !seen["url"] {
		t.Errorf("alan hataları = %+v", body.Error.Fields)
	}
}

// Gövde sözleşmesi 1.1'de standartlaştı: tanımsız alan reddedilir.
func TestCreateMonitorRejectsUnknownField(t *testing.T) {
	fixture := newMonitorFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.path(""),
		`{"name":"API","url":"https://example.com","interval":60}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, beklenen 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeInvalidJSON {
		t.Errorf("code = %q", code)
	}
}

// URL normalleştirmesi sözleşmede görünüyor: istemci ne yazarsa yazsın
// kaydedilen ve dönen değer aynı biçimde.
func TestCreateMonitorNormalizesURL(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t, `{"name":"API","url":"HTTPS://Example.COM:443/health#bolum"}`)
	if monitor.URL != "https://example.com/health" {
		t.Errorf("url = %q", monitor.URL)
	}
}

// Metot büyük/küçük harften bağımsız kabul ediliyor; tanınmayan değer 422.
func TestCreateMonitorMethodHandling(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t, `{"name":"API","url":"https://example.com","method":"head"}`)
	if monitor.Method != string(store.MonitorMethodHEAD) {
		t.Errorf("metot = %q", monitor.Method)
	}

	rec := do(t, fixture.router, http.MethodPost, fixture.path(""),
		`{"name":"API","url":"https://example.com","method":"DELETE"}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateInactiveMonitorStartsPaused(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t, `{"name":"API","url":"https://example.com","isActive":false}`)
	if monitor.Status != string(store.MonitorStatusPaused) {
		t.Errorf("durum = %q, beklenen paused", monitor.Status)
	}
}

// --- Okuma -------------------------------------------------------------------

func TestListMonitorsReturnsEmptyArray(t *testing.T) {
	fixture := newMonitorFixture(t)

	rec := do(t, fixture.router, http.MethodGet, fixture.path(""), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	// Boş liste `null` değil `[]` olmalı: istemcide gereksiz özel durum
	// doğmasın.
	if !strings.Contains(rec.Body.String(), `"monitors":[]`) {
		t.Errorf("gövde = %s", rec.Body.String())
	}
}

func TestGetMonitorUnknownIDIsNotFound(t *testing.T) {
	fixture := newMonitorFixture(t)

	unknown := do(t, fixture.router, http.MethodGet, fixture.path("/"+uuid.NewString()), "", bearer(fixture.ownerTok))
	malformed := do(t, fixture.router, http.MethodGet, fixture.path("/bu-bir-uuid-degil"), "", bearer(fixture.ownerTok))

	if unknown.Code != http.StatusNotFound || malformed.Code != http.StatusNotFound {
		t.Fatalf("status = %d / %d, ikisi de 404 olmalı", unknown.Code, malformed.Code)
	}

	// Biçimsiz kimlik ile olmayan kimlik AYNI cevabı almalı (org kimliğinde
	// olduğu gibi — bkz. orgscope.go).
	if a, b := errorBodyWithoutRequestID(t, unknown.Body.Bytes()),
		errorBodyWithoutRequestID(t, malformed.Body.Bytes()); a != b {
		t.Errorf("cevaplar farklı:\n%s\n%s", a, b)
	}
}

// --- Güncelleme / silme -------------------------------------------------------

func TestUpdateMonitorAppliesPartialChange(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	rec := do(t, fixture.router, http.MethodPatch, fixture.path("/"+monitor.ID),
		`{"isActive":false}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	updated := decodeMonitor(t, rec.Body.Bytes())
	if updated.IsActive {
		t.Error("isActive false olmalıydı")
	}
	if updated.Status != string(store.MonitorStatusPaused) {
		t.Errorf("durum = %q, beklenen paused", updated.Status)
	}
	// Gönderilmeyen alanlar korunmalı.
	if updated.Name != monitor.Name || updated.URL != monitor.URL {
		t.Errorf("dokunulmayan alanlar değişti: %+v", updated)
	}
}

func TestUpdateMonitorRejectsEmptyBody(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	rec := do(t, fixture.router, http.MethodPatch, fixture.path("/"+monitor.ID), `{}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
	}
}

func TestDeleteMonitorReturnsNoContentThenNotFound(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	rec := do(t, fixture.router, http.MethodDelete, fixture.path("/"+monitor.ID), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, beklenen 204 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 gövdesi boş olmalı: %s", rec.Body.String())
	}

	again := do(t, fixture.router, http.MethodDelete, fixture.path("/"+monitor.ID), "", bearer(fixture.ownerTok))
	if again.Code != http.StatusNotFound {
		t.Errorf("ikinci silme: status = %d, beklenen 404", again.Code)
	}
}

// Beklenmeyen veritabanı hatası istemciye ayrıntı sızdırmadan 500 olmalı.
func TestMonitorServiceErrorBecomesInternalError(t *testing.T) {
	fixture := newMonitorFixture(t)
	fixture.service.failWith = errors.New("bağlantı koptu")

	rec := do(t, fixture.router, http.MethodGet, fixture.path(""), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}

	body := decodeErrorResponse(t, rec)
	if body.Error.Code != codeInternalServerError {
		t.Errorf("code = %q", body.Error.Code)
	}
	if strings.Contains(body.Error.Message, "bağlantı koptu") {
		t.Errorf("iç hata metni sızdı: %q", body.Error.Message)
	}
}

// Listenin gövdesi organizasyon kimliğini TAŞIMIYOR: istemci hangi org'u
// sorduğunu zaten biliyor, iç kimlikleri cevaba koymak gereksiz yüzey.
func TestMonitorResponseHasNoOrgID(t *testing.T) {
	fixture := newMonitorFixture(t)
	fixture.createMonitor(t, validMonitorBody)

	rec := do(t, fixture.router, http.MethodGet, fixture.path(""), "", bearer(fixture.ownerTok))
	if strings.Contains(rec.Body.String(), fixture.orgID.String()) {
		t.Errorf("cevapta org kimliği var: %s", rec.Body.String())
	}

	if list := decodeMonitors(t, rec.Body.Bytes()); len(list) != 1 {
		t.Errorf("liste uzunluğu = %d", len(list))
	}
}

// Durum makinesinin eşikleri SÖZLEŞMEDE: gönderilmezse asimetrik varsayılan
// (kesinti ilan etmek pahalı, kesintinin bittiğini söylemek ucuz).
func TestCreateMonitorAppliesThresholdDefaults(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t, validMonitorBody)

	if monitor.FailureThreshold != monitors.DefaultFailureThreshold {
		t.Errorf("failureThreshold = %d, beklenen %d",
			monitor.FailureThreshold, monitors.DefaultFailureThreshold)
	}
	if monitor.RecoveryThreshold != monitors.DefaultRecoveryThreshold {
		t.Errorf("recoveryThreshold = %d, beklenen %d",
			monitor.RecoveryThreshold, monitors.DefaultRecoveryThreshold)
	}
	// Hiç kontrol edilmemiş monitor: son kontrol yok.
	if monitor.LastCheckedAt != nil {
		t.Errorf("lastCheckedAt = %v, beklenen null", monitor.LastCheckedAt)
	}
}

func TestCreateMonitorAcceptsCustomThresholds(t *testing.T) {
	fixture := newMonitorFixture(t)

	monitor := fixture.createMonitor(t,
		`{"name":"API","url":"https://example.com","failureThreshold":5,"recoveryThreshold":1}`)

	if monitor.FailureThreshold != 5 || monitor.RecoveryThreshold != 1 {
		t.Errorf("eşikler = %d/%d, beklenen 5/1", monitor.FailureThreshold, monitor.RecoveryThreshold)
	}
}

func TestCreateMonitorRejectsOutOfRangeThreshold(t *testing.T) {
	fixture := newMonitorFixture(t)

	rec := do(t, fixture.router, http.MethodPost, fixture.path(""),
		`{"name":"API","url":"https://example.com","failureThreshold":0,"recoveryThreshold":99}`,
		bearer(fixture.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
	}

	fields := decodeErrorResponse(t, rec).Error.Fields
	if len(fields) != 2 {
		t.Fatalf("alan hataları = %+v, beklenen iki alan", fields)
	}
	if fields[0].Field != "failureThreshold" || fields[1].Field != "recoveryThreshold" {
		t.Errorf("alan adları = %s / %s", fields[0].Field, fields[1].Field)
	}
}

func TestUpdateMonitorChangesThreshold(t *testing.T) {
	fixture := newMonitorFixture(t)
	monitor := fixture.createMonitor(t, validMonitorBody)

	rec := do(t, fixture.router, http.MethodPatch, fixture.path("/"+monitor.ID),
		`{"failureThreshold":7}`, bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}

	updated := decodeMonitor(t, rec.Body.Bytes())
	if updated.FailureThreshold != 7 {
		t.Errorf("failureThreshold = %d, beklenen 7", updated.FailureThreshold)
	}
	// Gönderilmeyen eşik korunmalı.
	if updated.RecoveryThreshold != monitors.DefaultRecoveryThreshold {
		t.Errorf("recoveryThreshold = %d, korunmalıydı", updated.RecoveryThreshold)
	}
}

// PRO PLANDA VARSAYILAN ARALIK ÜRÜNÜN VARSAYILANI (8.5).
//
// Bir önceki testin ikizi: orada plan FREE (300 sn alt sınır) olduğu için
// varsayılan yükseliyordu, burada PRO'nun alt sınırı (30 sn) ürünün 60
// saniyelik varsayılanının ALTINDA, yani varsayılan olduğu gibi kalıyor.
// İkisi birlikte kuralı tam olarak pinliyor: max(ürün varsayılanı, plan
// alt sınırı).
func TestCreateMonitorKeepsProductDefaultOnProPlan(t *testing.T) {
	fixture := newMonitorFixture(t)
	fixture.orgs.setPlan(fixture.orgID, store.OrgPlanPro)

	monitor := fixture.createMonitor(t, validMonitorBody)

	if monitor.CheckIntervalSeconds != monitors.DefaultCheckIntervalSeconds {
		t.Errorf("aralık = %d, beklenen %d",
			monitor.CheckIntervalSeconds, monitors.DefaultCheckIntervalSeconds)
	}
}
