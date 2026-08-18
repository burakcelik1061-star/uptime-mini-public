package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/maintenance"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeMaintenanceService, bakım penceresi servisinin HTTP katmanı için
// sahtesi. SAKLAMA sahte, DOĞRULAMA gerçek (`maintenance.Validate`).
type fakeMaintenanceService struct {
	byOrg map[uuid.UUID]map[uuid.UUID]maintenance.Window
	// now, "başlamış pencere silinemez" kuralını sabit bir saatle ölçmek için.
	now      func() time.Time
	failWith error
}

func newFakeMaintenanceService() *fakeMaintenanceService {
	return &fakeMaintenanceService{
		byOrg: map[uuid.UUID]map[uuid.UUID]maintenance.Window{},
		now:   func() time.Time { return time.Now().UTC() },
	}
}

func (f *fakeMaintenanceService) Create(
	_ context.Context, orgID uuid.UUID, in maintenance.Input,
) (maintenance.Window, error) {
	if f.failWith != nil {
		return maintenance.Window{}, f.failWith
	}

	in = maintenance.Normalize(in)
	if err := maintenance.NewService(nil, nil, nil).Validate(in); err != nil {
		return maintenance.Window{}, err
	}

	window := maintenance.Window{
		Window: store.MaintenanceWindow{
			ID:          uuid.New(),
			OrgID:       orgID,
			Title:       in.Title,
			Description: in.Description,
			StartsAt:    in.StartsAt,
			EndsAt:      in.EndsAt,
			CreatedAt:   f.now(),
		},
	}

	for _, id := range in.MonitorIDs {
		window.Monitors = append(window.Monitors, maintenance.AffectedMonitor{
			ID:     id,
			Name:   "API",
			Status: store.MonitorStatusUp,
		})
	}

	if f.byOrg[orgID] == nil {
		f.byOrg[orgID] = map[uuid.UUID]maintenance.Window{}
	}
	f.byOrg[orgID][window.Window.ID] = window

	return window, nil
}

func (f *fakeMaintenanceService) List(
	_ context.Context, orgID uuid.UUID, _ int32,
) ([]maintenance.Window, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	items := make([]maintenance.Window, 0, len(f.byOrg[orgID]))
	for _, window := range f.byOrg[orgID] {
		items = append(items, window)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Window.StartsAt.After(items[j].Window.StartsAt)
	})

	return items, nil
}

func (f *fakeMaintenanceService) Delete(_ context.Context, orgID, windowID uuid.UUID) error {
	if f.failWith != nil {
		return f.failWith
	}

	window, ok := f.byOrg[orgID][windowID]
	if !ok {
		return maintenance.ErrNotFound
	}

	if !window.Window.StartsAt.After(f.now()) {
		return maintenance.ErrStarted
	}

	delete(f.byOrg[orgID], windowID)

	return nil
}

// --- Test kurulumu -----------------------------------------------------------

type maintenanceFixture struct {
	router    http.Handler
	service   *fakeMaintenanceService
	orgID     uuid.UUID
	ownerTok  string
	memberTok string
	outsider  string
}

func newMaintenanceFixture(t *testing.T) maintenanceFixture {
	t.Helper()

	deps := newTestDeps(t)
	windowSvc, ok := deps.Maintenance.(*fakeMaintenanceService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	orgSvc, ok := deps.Orgs.(*fakeOrgService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	router := NewRouter(deps)

	ownerTok := authenticate(t, router, testEmail)
	memberTok := authenticate(t, router, "bakim-uye@example.com")
	outsider := authenticate(t, router, "bakim-yabanci@example.com")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	orgID, err := uuid.Parse(decodeOrganization(t, rec.Body.Bytes()).ID)
	if err != nil {
		t.Fatalf("org id çözümlenemedi: %v", err)
	}

	orgSvc.addMember(orgID, userIDOf(t, memberTok), store.OrgRoleMember)

	return maintenanceFixture{
		router:    router,
		service:   windowSvc,
		orgID:     orgID,
		ownerTok:  ownerTok,
		memberTok: memberTok,
		outsider:  outsider,
	}
}

func (f maintenanceFixture) path(suffix string) string {
	return "/v1/orgs/" + f.orgID.String() + "/maintenance-windows" + suffix
}

// windowBody, yarın başlayan iki saatlik bir pencerenin gövdesi.
func windowBody(monitorIDs ...uuid.UUID) string {
	start := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)

	ids := make([]string, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		ids = append(ids, id.String())
	}

	payload, _ := json.Marshal(map[string]any{
		"title":      "Veritabanı yükseltmesi",
		"startsAt":   start.Format(time.RFC3339),
		"endsAt":     start.Add(2 * time.Hour).Format(time.RFC3339),
		"monitorIds": ids,
	})

	return string(payload)
}

func decodeWindows(t *testing.T, body []byte) []maintenanceWindowResponse {
	t.Helper()

	var wrapper struct {
		MaintenanceWindows []maintenanceWindowResponse `json:"maintenanceWindows"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.MaintenanceWindows
}

func decodeWindow(t *testing.T, body []byte) maintenanceWindowResponse {
	t.Helper()

	var wrapper struct {
		MaintenanceWindow maintenanceWindowResponse `json:"maintenanceWindow"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.MaintenanceWindow
}

// --- Testler -----------------------------------------------------------------

func TestCreateMaintenanceWindow(t *testing.T) {
	f := newMaintenanceFixture(t)
	monitorID := uuid.New()

	rec := do(t, f.router, http.MethodPost, f.path("/"), windowBody(monitorID), bearer(f.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("durum = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	window := decodeWindow(t, rec.Body.Bytes())
	if window.Title != "Veritabanı yükseltmesi" {
		t.Errorf("başlık = %q", window.Title)
	}
	if len(window.Monitors) != 1 || window.Monitors[0].ID != monitorID.String() {
		t.Errorf("servisler = %+v", window.Monitors)
	}
	if !window.EndsAt.After(window.StartsAt) {
		t.Errorf("aralık ters: %v → %v", window.StartsAt, window.EndsAt)
	}
}

// BAKIM PENCERESİ TANIMLAMAK ADMIN İŞİ (0.1 §2): pencere otomatik
// incident'ları bastırıyor, yani yanlış kullanımı gerçek bir kesintiyi
// görünmez yapabilir.
func TestCreateMaintenanceWindowRequiresAdmin(t *testing.T) {
	f := newMaintenanceFixture(t)

	rec := do(t, f.router, http.MethodPost, f.path("/"), windowBody(uuid.New()), bearer(f.memberTok))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("durum = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeForbidden {
		t.Errorf("kod = %q, beklenen %q", code, codeForbidden)
	}
}

// Okuma üyeye açık: "ne zaman bakım var" salt-okunur panonun parçası.
func TestListMaintenanceWindowsAllowsMember(t *testing.T) {
	f := newMaintenanceFixture(t)

	if rec := do(t, f.router, http.MethodPost, f.path("/"), windowBody(uuid.New()),
		bearer(f.ownerTok)); rec.Code != http.StatusCreated {
		t.Fatalf("pencere oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	rec := do(t, f.router, http.MethodGet, f.path("/"), "", bearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	if got := len(decodeWindows(t, rec.Body.Bytes())); got != 1 {
		t.Errorf("pencere sayısı = %d, beklenen 1", got)
	}
}

func TestMaintenanceWindowValidation(t *testing.T) {
	f := newMaintenanceFixture(t)
	start := time.Now().UTC().Add(24 * time.Hour)

	cases := []struct {
		name   string
		body   string
		status int
		code   string
		field  string
	}{
		{
			name: "başlık boş",
			body: fmt.Sprintf(`{"title":"  ","startsAt":%q,"endsAt":%q,"monitorIds":[%q]}`,
				start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339), uuid.NewString()),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "title",
		},
		{
			name: "servis seçilmemiş",
			body: fmt.Sprintf(`{"title":"x","startsAt":%q,"endsAt":%q,"monitorIds":[]}`,
				start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339)),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "monitorIds",
		},
		{
			// Geçmişe dönük pencere: uptime geçmişini yeniden yazamaz.
			name: "geçmiş pencere",
			body: fmt.Sprintf(`{"title":"x","startsAt":%q,"endsAt":%q,"monitorIds":[%q]}`,
				time.Now().UTC().Add(-3*time.Hour).Format(time.RFC3339),
				time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), uuid.NewString()),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "endsAt",
		},
		{
			name: "bozuk zaman",
			body: fmt.Sprintf(`{"title":"x","startsAt":"yarin","endsAt":%q,"monitorIds":[%q]}`,
				start.Add(time.Hour).Format(time.RFC3339), uuid.NewString()),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "startsAt",
		},
		{
			name: "bozuk servis kimliği",
			body: fmt.Sprintf(`{"title":"x","startsAt":%q,"endsAt":%q,"monitorIds":["abc"]}`,
				start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339)),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "monitorIds",
		},
		{
			name: "tanımsız alan",
			body: fmt.Sprintf(`{"title":"x","startsAt":%q,"endsAt":%q,"monitorIds":[%q],"tekrar":true}`,
				start.Format(time.RFC3339), start.Add(time.Hour).Format(time.RFC3339), uuid.NewString()),
			status: http.StatusBadRequest,
			code:   codeInvalidJSON,
			field:  "tekrar",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, f.router, http.MethodPost, f.path("/"), tc.body, bearer(f.ownerTok))
			if rec.Code != tc.status {
				t.Fatalf("durum = %d, beklenen %d (%s)", rec.Code, tc.status, rec.Body.String())
			}

			body := decodeErrorResponse(t, rec)
			if body.Error.Code != tc.code {
				t.Errorf("kod = %q, beklenen %q", body.Error.Code, tc.code)
			}
			if len(body.Error.Fields) == 0 || body.Error.Fields[0].Field != tc.field {
				t.Errorf("alanlar = %+v, beklenen %q", body.Error.Fields, tc.field)
			}
		})
	}
}

func TestDeleteUpcomingMaintenanceWindow(t *testing.T) {
	f := newMaintenanceFixture(t)

	created := do(t, f.router, http.MethodPost, f.path("/"), windowBody(uuid.New()), bearer(f.ownerTok))
	window := decodeWindow(t, created.Body.Bytes())

	rec := do(t, f.router, http.MethodDelete, f.path("/"+window.ID), "", bearer(f.ownerTok))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("durum = %d, beklenen 204 (%s)", rec.Code, rec.Body.String())
	}

	list := do(t, f.router, http.MethodGet, f.path("/"), "", bearer(f.ownerTok))
	if got := len(decodeWindows(t, list.Body.Bytes())); got != 0 {
		t.Errorf("pencere sayısı = %d, beklenen 0", got)
	}
}

// BAŞLAMIŞ PENCERE SİLİNEMİYOR (409): geçmişi yeniden yazmamak için.
func TestDeleteStartedMaintenanceWindowConflicts(t *testing.T) {
	f := newMaintenanceFixture(t)

	created := do(t, f.router, http.MethodPost, f.path("/"), windowBody(uuid.New()), bearer(f.ownerTok))
	window := decodeWindow(t, created.Body.Bytes())

	// Saati pencerenin içine taşı.
	f.service.now = func() time.Time { return window.StartsAt.Add(time.Minute) }

	rec := do(t, f.router, http.MethodDelete, f.path("/"+window.ID), "", bearer(f.ownerTok))
	if rec.Code != http.StatusConflict {
		t.Fatalf("durum = %d, beklenen 409 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeMaintenanceStarted {
		t.Errorf("kod = %q, beklenen %q", code, codeMaintenanceStarted)
	}
}

func TestDeleteMaintenanceWindowRequiresAdmin(t *testing.T) {
	f := newMaintenanceFixture(t)

	created := do(t, f.router, http.MethodPost, f.path("/"), windowBody(uuid.New()), bearer(f.ownerTok))
	window := decodeWindow(t, created.Body.Bytes())

	rec := do(t, f.router, http.MethodDelete, f.path("/"+window.ID), "", bearer(f.memberTok))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("durum = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}
}

// Olmayan ve biçimsiz kimlik birebir aynı cevabı alıyor.
func TestDeleteMaintenanceWindowNotFound(t *testing.T) {
	f := newMaintenanceFixture(t)

	for _, id := range []string{uuid.NewString(), "bu-bir-uuid-degil"} {
		rec := do(t, f.router, http.MethodDelete, f.path("/"+id), "", bearer(f.ownerTok))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s → %d, beklenen 404", id, rec.Code)
		}

		if code := decodeErrorResponse(t, rec).Error.Code; code != codeNotFound {
			t.Errorf("kod = %q, beklenen %q", code, codeNotFound)
		}
	}
}

// Yabancı organizasyon uçların hiçbirini göremiyor (1.4'ün middleware'i).
func TestMaintenanceWindowsHiddenFromOutsiders(t *testing.T) {
	f := newMaintenanceFixture(t)

	cases := []struct {
		method string
		body   string
	}{
		{method: http.MethodGet},
		{method: http.MethodPost, body: windowBody(uuid.New())},
	}

	for _, tc := range cases {
		rec := do(t, f.router, tc.method, f.path("/"), tc.body, bearer(f.outsider))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s → %d, beklenen 404", tc.method, rec.Code)
		}
	}
}

func TestMaintenanceEndpointsRequireAuth(t *testing.T) {
	f := newMaintenanceFixture(t)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: f.path("/")},
		{method: http.MethodPost, path: f.path("/"), body: windowBody(uuid.New())},
		{method: http.MethodDelete, path: f.path("/" + uuid.NewString())},
	}

	for _, tc := range cases {
		rec := do(t, f.router, tc.method, tc.path, tc.body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s → %d, beklenen 401", tc.method, tc.path, rec.Code)
		}
	}
}
