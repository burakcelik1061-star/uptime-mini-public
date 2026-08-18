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

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/incidents"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeIncidentService, incident servisinin HTTP katmanı için sahtesi.
//
// SAKLAMA sahte, DOĞRULAMA gerçek (fakeMonitorService ile aynı disiplin):
// sahte servis `incidents.ValidateUpdate`i çağırıyor, yani buradaki testler
// kuralların KENDİSİNİ değil handler'ın 422/409/403 çevirimini ölçüyor.
// Kapanmış incident'a yazmanın engellenmesi ve org izolasyonunun SQL tarafı
// internal/incidents'ın gerçek veritabanı testlerinde.
type fakeIncidentService struct {
	// byOrg: orgID → incidentID → kayıt.
	byOrg    map[uuid.UUID]map[uuid.UUID]*fakeIncident
	failWith error
}

type fakeIncident struct {
	incident store.Incident
	monitors []incidents.AffectedMonitor
	updates  []store.IncidentUpdate
}

func newFakeIncidentService() *fakeIncidentService {
	return &fakeIncidentService{byOrg: map[uuid.UUID]map[uuid.UUID]*fakeIncident{}}
}

// seed, organizasyona bir incident ekler (worker'ın açtığı kaydın karşılığı).
func (f *fakeIncidentService) seed(orgID uuid.UUID, title string, resolved bool) store.Incident {
	now := time.Now().UTC()
	incident := store.Incident{
		ID:            uuid.New(),
		OrgID:         orgID,
		Title:         title,
		Status:        store.IncidentStatusInvestigating,
		Impact:        store.IncidentImpactMajor,
		IsAutoCreated: true,
		StartedAt:     now.Add(-time.Hour),
		CreatedAt:     now.Add(-time.Hour),
		UpdatedAt:     now.Add(-time.Hour),
	}

	if resolved {
		resolvedAt := now.Add(-30 * time.Minute)
		incident.Status = store.IncidentStatusResolved
		incident.ResolvedAt = &resolvedAt
	}

	if f.byOrg[orgID] == nil {
		f.byOrg[orgID] = map[uuid.UUID]*fakeIncident{}
	}

	f.byOrg[orgID][incident.ID] = &fakeIncident{
		incident: incident,
		monitors: []incidents.AffectedMonitor{
			{ID: uuid.New(), Name: "Ödeme API", Status: store.MonitorStatusDown},
		},
		updates: []store.IncidentUpdate{{
			ID:         uuid.New(),
			IncidentID: incident.ID,
			IsSystem:   true,
			Status:     store.IncidentStatusInvestigating,
			Body:       "Otomatik kontroller yanıt alamıyor.",
			CreatedAt:  incident.StartedAt,
		}},
	}

	return incident
}

func (f *fakeIncidentService) List(
	_ context.Context, orgID uuid.UUID, filter incidents.Filter, _ int32,
) ([]incidents.ListItem, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	items := make([]incidents.ListItem, 0, len(f.byOrg[orgID]))
	for _, row := range f.byOrg[orgID] {
		open := row.incident.ResolvedAt == nil
		if (filter == incidents.FilterOpen && !open) || (filter == incidents.FilterResolved && open) {
			continue
		}

		items = append(items, incidents.ListItem{Incident: row.incident, Monitors: row.monitors})
	}

	// Gerçek sorgunun sırası: yeniden eskiye.
	sort.Slice(items, func(i, j int) bool {
		return items[i].Incident.StartedAt.After(items[j].Incident.StartedAt)
	})

	return items, nil
}

func (f *fakeIncidentService) Get(_ context.Context, orgID, incidentID uuid.UUID) (incidents.Detail, error) {
	if f.failWith != nil {
		return incidents.Detail{}, f.failWith
	}

	row, ok := f.byOrg[orgID][incidentID]
	if !ok {
		return incidents.Detail{}, incidents.ErrNotFound
	}

	return incidents.Detail{Incident: row.incident, Monitors: row.monitors, Updates: row.updates}, nil
}

func (f *fakeIncidentService) AddUpdate(
	_ context.Context, orgID, incidentID uuid.UUID, in incidents.UpdateInput,
) (store.IncidentUpdate, error) {
	if f.failWith != nil {
		return store.IncidentUpdate{}, f.failWith
	}

	in = incidents.NormalizeUpdate(in)
	if err := incidents.ValidateUpdate(in); err != nil {
		return store.IncidentUpdate{}, err
	}

	row, ok := f.byOrg[orgID][incidentID]
	if !ok {
		return store.IncidentUpdate{}, incidents.ErrNotFound
	}

	if row.incident.ResolvedAt != nil {
		return store.IncidentUpdate{}, incidents.ErrResolved
	}

	author := in.AuthorID
	update := store.IncidentUpdate{
		ID:         uuid.New(),
		IncidentID: incidentID,
		AuthorID:   &author,
		Status:     in.Status,
		Body:       in.Body,
		CreatedAt:  time.Now().UTC(),
	}

	row.updates = append(row.updates, update)
	row.incident.Status = in.Status

	if in.Status == store.IncidentStatusResolved {
		resolvedAt := update.CreatedAt
		row.incident.ResolvedAt = &resolvedAt
	}

	return update, nil
}

func (f *fakeIncidentService) CreateManual(
	_ context.Context, orgID uuid.UUID, in incidents.ManualInput,
) (incidents.Detail, error) {
	if f.failWith != nil {
		return incidents.Detail{}, f.failWith
	}

	// Doğrulama GERÇEK: handler'ın 422 çevirimi ölçülüyor, kuralların kendisi
	// internal/incidents'ın testlerinde.
	in = incidents.NormalizeManual(in)
	if err := incidents.ValidateManual(in); err != nil {
		return incidents.Detail{}, err
	}

	now := time.Now().UTC()
	incident := store.Incident{
		ID:        uuid.New(),
		OrgID:     orgID,
		Title:     in.Title,
		Status:    store.IncidentStatusInvestigating,
		Impact:    in.Impact,
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}

	author := in.AuthorID
	detail := incidents.Detail{
		Incident: incident,
		Updates: []store.IncidentUpdate{{
			ID:         uuid.New(),
			IncidentID: incident.ID,
			AuthorID:   &author,
			Status:     store.IncidentStatusInvestigating,
			Body:       in.Body,
			CreatedAt:  now,
		}},
	}

	for _, id := range in.MonitorIDs {
		detail.Monitors = append(detail.Monitors, incidents.AffectedMonitor{
			ID: id, Name: "Ödeme API", Status: store.MonitorStatusDown,
		})
	}

	if f.byOrg[orgID] == nil {
		f.byOrg[orgID] = map[uuid.UUID]*fakeIncident{}
	}
	f.byOrg[orgID][incident.ID] = &fakeIncident{
		incident: incident, monitors: detail.Monitors, updates: detail.Updates,
	}

	return detail, nil
}

// --- Test kurulumu -----------------------------------------------------------

type incidentFixture struct {
	router    http.Handler
	service   *fakeIncidentService
	orgID     uuid.UUID
	ownerTok  string
	memberTok string
	outsider  string
}

func newIncidentFixture(t *testing.T) incidentFixture {
	t.Helper()

	deps := newTestDeps(t)
	incidentSvc, ok := deps.Incidents.(*fakeIncidentService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	orgSvc, ok := deps.Orgs.(*fakeOrgService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	router := NewRouter(deps)

	ownerTok := authenticate(t, router, testEmail)
	memberTok := authenticate(t, router, "nobetci@example.com")
	outsider := authenticate(t, router, "yabanci-incident@example.com")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	orgID, err := uuid.Parse(decodeOrganization(t, rec.Body.Bytes()).ID)
	if err != nil {
		t.Fatalf("org id çözümlenemedi: %v", err)
	}

	orgSvc.addMember(orgID, userIDOf(t, memberTok), store.OrgRoleMember)

	return incidentFixture{
		router:    router,
		service:   incidentSvc,
		orgID:     orgID,
		ownerTok:  ownerTok,
		memberTok: memberTok,
		outsider:  outsider,
	}
}

func (f incidentFixture) path(suffix string) string {
	return "/v1/orgs/" + f.orgID.String() + "/incidents" + suffix
}

func decodeIncidents(t *testing.T, body []byte) []incidentResponse {
	t.Helper()

	var wrapper struct {
		Incidents []incidentResponse `json:"incidents"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Incidents
}

func decodeIncidentDetail(t *testing.T, body []byte) (incidentResponse, []incidentUpdateResponse) {
	t.Helper()

	var wrapper struct {
		Incident incidentResponse         `json:"incident"`
		Updates  []incidentUpdateResponse `json:"updates"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Incident, wrapper.Updates
}

// --- Testler -----------------------------------------------------------------

// OKUMA ÜYEYE AÇIK (0.1 §2): `member` rolü incident geçmişini görebiliyor.
func TestListIncidentsAllowsMember(t *testing.T) {
	f := newIncidentFixture(t)
	f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	rec := do(t, f.router, http.MethodGet, f.path("/"), "", bearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	list := decodeIncidents(t, rec.Body.Bytes())
	if len(list) != 1 {
		t.Fatalf("incident sayısı = %d, beklenen 1", len(list))
	}

	if !list[0].IsAutoCreated {
		t.Error("isAutoCreated = false, beklenen true")
	}
	if list[0].ResolvedAt != nil || list[0].DurationSeconds != nil {
		t.Error("açık incident kapanmış görünüyor")
	}
	if len(list[0].Monitors) != 1 || list[0].Monitors[0].Name != "Ödeme API" {
		t.Errorf("etkilenen servisler = %+v", list[0].Monitors)
	}
}

// Kapanmış incident SÜRESİYLE dönüyor (hesap sunucuda).
func TestListIncidentsReturnsDuration(t *testing.T) {
	f := newIncidentFixture(t)
	f.service.seed(f.orgID, "Kapanmış kesinti", true)

	rec := do(t, f.router, http.MethodGet, f.path("/"), "", bearer(f.ownerTok))
	list := decodeIncidents(t, rec.Body.Bytes())

	if len(list) != 1 || list[0].DurationSeconds == nil {
		t.Fatalf("süre dönmedi: %+v", list)
	}

	if *list[0].DurationSeconds != int64(30*time.Minute/time.Second) {
		t.Errorf("süre = %d sn, beklenen 1800", *list[0].DurationSeconds)
	}
}

func TestListIncidentsFilters(t *testing.T) {
	f := newIncidentFixture(t)
	f.service.seed(f.orgID, "Açık", false)
	f.service.seed(f.orgID, "Kapalı", true)

	cases := map[string]int{"": 2, "?status=all": 2, "?status=open": 1, "?status=resolved": 1}

	for query, want := range cases {
		t.Run("filtre"+query, func(t *testing.T) {
			rec := do(t, f.router, http.MethodGet, f.path("/")+query, "", bearer(f.ownerTok))
			if rec.Code != http.StatusOK {
				t.Fatalf("durum = %d, beklenen 200", rec.Code)
			}

			if got := len(decodeIncidents(t, rec.Body.Bytes())); got != want {
				t.Errorf("satır sayısı = %d, beklenen %d", got, want)
			}
		})
	}
}

// Tanınmayan süzgeç SESSİZCE "hepsi"ne düşmüyor: 400.
func TestListIncidentsRejectsUnknownFilter(t *testing.T) {
	f := newIncidentFixture(t)

	rec := do(t, f.router, http.MethodGet, f.path("/")+"?status=opne", "", bearer(f.ownerTok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("durum = %d, beklenen 400 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeInvalidQuery {
		t.Errorf("kod = %q, beklenen %q", code, codeInvalidQuery)
	}
}

// Detay timeline'ı BİRLİKTE döndürüyor (ikinci bir istek gerekmiyor).
func TestIncidentDetailIncludesTimeline(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	rec := do(t, f.router, http.MethodGet, f.path("/"+incident.ID.String()), "", bearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	detail, updates := decodeIncidentDetail(t, rec.Body.Bytes())
	if detail.ID != incident.ID.String() {
		t.Errorf("id = %q, beklenen %q", detail.ID, incident.ID)
	}
	if len(updates) != 1 || !updates[0].IsSystem || updates[0].AuthorID != nil {
		t.Fatalf("timeline = %+v, beklenen tek sistem satırı", updates)
	}
}

// Yabancı organizasyon: org-scoped middleware 404 veriyor (1.4).
func TestIncidentsHiddenFromOutsiders(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Gizli", false)

	paths := []string{f.path("/"), f.path("/" + incident.ID.String())}

	for _, path := range paths {
		rec := do(t, f.router, http.MethodGet, path, "", bearer(f.outsider))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s → %d, beklenen 404", path, rec.Code)
		}
	}
}

// Olmayan ve BİÇİMSİZ kimlik birebir aynı cevabı alıyor.
func TestIncidentDetailNotFound(t *testing.T) {
	f := newIncidentFixture(t)

	for _, id := range []string{uuid.NewString(), "bu-bir-uuid-degil"} {
		rec := do(t, f.router, http.MethodGet, f.path("/"+id), "", bearer(f.ownerTok))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s → %d, beklenen 404", id, rec.Code)
		}

		if code := decodeErrorResponse(t, rec).Error.Code; code != codeNotFound {
			t.Errorf("kod = %q, beklenen %q", code, codeNotFound)
		}
	}
}

// TIMELINE'A YAZMA ÜYEYE AÇIK: nöbetçi mühendis senaryosu.
func TestAddUpdateAllowsMember(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	rec := do(t, f.router, http.MethodPost, f.path("/"+incident.ID.String()+"/updates"),
		`{"status":"identified","body":"Bağlantı havuzu doldu."}`, bearer(f.memberTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("durum = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	detail, updates := decodeIncidentDetail(t, rec.Body.Bytes())
	if detail.Status != string(store.IncidentStatusIdentified) {
		t.Errorf("incident durumu = %q, beklenen identified", detail.Status)
	}
	if len(updates) != 2 {
		t.Fatalf("timeline uzunluğu = %d, beklenen 2", len(updates))
	}

	last := updates[len(updates)-1]
	if last.IsSystem || last.AuthorID == nil {
		t.Errorf("kullanıcı satırı sistem gibi görünüyor: %+v", last)
	}
	if *last.AuthorID != userIDOf(t, f.memberTok).String() {
		t.Errorf("yazar = %q, beklenen %q", *last.AuthorID, userIDOf(t, f.memberTok))
	}
}

// AMA KAPATMAK YÖNETİM İŞİ: `member` incident'ı çözemiyor (0.1 §2).
func TestMemberCannotResolveIncident(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	rec := do(t, f.router, http.MethodPost, f.path("/"+incident.ID.String()+"/updates"),
		`{"status":"resolved","body":"Bitti."}`, bearer(f.memberTok))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("durum = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeForbidden {
		t.Errorf("kod = %q, beklenen %q", code, codeForbidden)
	}

	// Reddedilen istek timeline'a hiçbir şey yazmamış olmalı.
	detail, _ := f.service.Get(t.Context(), f.orgID, incident.ID)
	if len(detail.Updates) != 1 {
		t.Errorf("timeline uzunluğu = %d, beklenen 1", len(detail.Updates))
	}
}

func TestOwnerCanResolveIncident(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	rec := do(t, f.router, http.MethodPost, f.path("/"+incident.ID.String()+"/updates"),
		`{"status":"resolved","body":"Servis normale döndü."}`, bearer(f.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("durum = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	detail, _ := decodeIncidentDetail(t, rec.Body.Bytes())
	if detail.Status != string(store.IncidentStatusResolved) || detail.ResolvedAt == nil {
		t.Fatalf("incident kapanmadı: %+v", detail)
	}
	if detail.DurationSeconds == nil {
		t.Error("kapanan incident süresiz döndü")
	}
}

// Kapanmış incident'a yazmak 409: istek biçimsel olarak geçerli, kaydın
// DURUMU elvermiyor (yeniden açma yok).
func TestAddUpdateToResolvedIncidentConflicts(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Kapanmış kesinti", true)

	rec := do(t, f.router, http.MethodPost, f.path("/"+incident.ID.String()+"/updates"),
		`{"status":"monitoring","body":"Yeniden açalım."}`, bearer(f.ownerTok))
	if rec.Code != http.StatusConflict {
		t.Fatalf("durum = %d, beklenen 409 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeIncidentResolved {
		t.Errorf("kod = %q, beklenen %q", code, codeIncidentResolved)
	}
}

func TestAddUpdateValidation(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)
	path := f.path("/" + incident.ID.String() + "/updates")

	cases := []struct {
		name   string
		body   string
		status int
		code   string
		field  string
	}{
		{
			name:   "boş metin",
			body:   `{"status":"monitoring","body":"   "}`,
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "body",
		},
		{
			name:   "tanınmayan durum",
			body:   `{"status":"kapandi","body":"x"}`,
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "status",
		},
		{
			name:   "durum yok",
			body:   `{"body":"x"}`,
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "status",
		},
		{
			// 1.1'in gövde sözleşmesi: tanımsız alan sessizce yok sayılmıyor.
			name:   "tanımsız alan",
			body:   `{"status":"monitoring","body":"x","yazar":"ben"}`,
			status: http.StatusBadRequest,
			code:   codeInvalidJSON,
			field:  "yazar",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, f.router, http.MethodPost, path, tc.body, bearer(f.ownerTok))
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

// Kimliksiz istek incident uçlarına hiç ulaşmıyor.
func TestIncidentEndpointsRequireAuth(t *testing.T) {
	f := newIncidentFixture(t)
	incident := f.service.seed(f.orgID, "Ödeme API yanıt vermiyor", false)

	cases := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: f.path("/")},
		{method: http.MethodGet, path: f.path("/" + incident.ID.String())},
		{
			method: http.MethodPost,
			path:   f.path("/" + incident.ID.String() + "/updates"),
			body:   `{"status":"monitoring","body":"x"}`,
		},
	}

	for _, tc := range cases {
		rec := do(t, f.router, tc.method, tc.path, tc.body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s → %d, beklenen 401", tc.method, tc.path, rec.Code)
		}
	}
}

// MANUEL INCIDENT OLUŞTURMAK ADMIN İŞİ (0.1 §2).
func TestCreateManualIncident(t *testing.T) {
	f := newIncidentFixture(t)
	first, second := uuid.New(), uuid.New()

	body := fmt.Sprintf(
		`{"title":"Veritabanı bakımı kesintiye yol açtı","impact":"critical","monitorIds":[%q,%q],"body":"Sorunu araştırıyoruz."}`,
		first, second)

	rec := do(t, f.router, http.MethodPost, f.path("/"), body, bearer(f.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("durum = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	incident, updates := decodeIncidentDetail(t, rec.Body.Bytes())

	if incident.IsAutoCreated {
		t.Error("manuel incident isAutoCreated=true geldi")
	}
	if incident.Impact != "critical" {
		t.Errorf("etki = %q, beklenen critical", incident.Impact)
	}
	// Manuel incident BİRDEN FAZLA servisi kapsayabiliyor (0.1 §5).
	if len(incident.Monitors) != 2 {
		t.Errorf("servis sayısı = %d, beklenen 2", len(incident.Monitors))
	}
	if incident.ResolvedAt != nil {
		t.Error("yeni incident kapalı geldi")
	}

	// İlk timeline satırı kaydın kendisiyle birlikte doğuyor.
	if len(updates) != 1 || updates[0].IsSystem || updates[0].Body != "Sorunu araştırıyoruz." {
		t.Fatalf("timeline = %+v", updates)
	}
	if updates[0].AuthorID == nil || *updates[0].AuthorID != userIDOf(t, f.ownerTok).String() {
		t.Errorf("yazar = %v", updates[0].AuthorID)
	}
}

func TestCreateManualIncidentRequiresAdmin(t *testing.T) {
	f := newIncidentFixture(t)

	body := fmt.Sprintf(`{"title":"x","impact":"minor","monitorIds":[%q],"body":"y"}`, uuid.NewString())

	rec := do(t, f.router, http.MethodPost, f.path("/"), body, bearer(f.memberTok))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("durum = %d, beklenen 403 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateManualIncidentValidation(t *testing.T) {
	f := newIncidentFixture(t)
	monitorID := uuid.NewString()

	cases := []struct {
		name   string
		body   string
		status int
		code   string
		field  string
	}{
		{
			name:   "başlık boş",
			body:   fmt.Sprintf(`{"title":"  ","impact":"minor","monitorIds":[%q],"body":"y"}`, monitorID),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "title",
		},
		{
			name:   "tanınmayan etki",
			body:   fmt.Sprintf(`{"title":"x","impact":"felaket","monitorIds":[%q],"body":"y"}`, monitorID),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "impact",
		},
		{
			name:   "servis yok",
			body:   `{"title":"x","impact":"minor","monitorIds":[],"body":"y"}`,
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "monitorIds",
		},
		{
			name: "aynı servis iki kez",
			body: fmt.Sprintf(`{"title":"x","impact":"minor","monitorIds":[%q,%q],"body":"y"}`,
				monitorID, monitorID),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "monitorIds",
		},
		{
			name:   "bozuk servis kimliği",
			body:   `{"title":"x","impact":"minor","monitorIds":["abc"],"body":"y"}`,
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "monitorIds",
		},
		{
			// İlk timeline satırı zorunlu: "ne oluyor" cümlesi kayıtla birlikte
			// doğmalı.
			name:   "ilk güncelleme boş",
			body:   fmt.Sprintf(`{"title":"x","impact":"minor","monitorIds":[%q],"body":"   "}`, monitorID),
			status: http.StatusUnprocessableEntity,
			code:   codeValidationFailed,
			field:  "body",
		},
		{
			name: "tanımsız alan",
			body: fmt.Sprintf(
				`{"title":"x","impact":"minor","monitorIds":[%q],"body":"y","startedAt":"2026-01-01T00:00:00Z"}`,
				monitorID),
			status: http.StatusBadRequest,
			code:   codeInvalidJSON,
			field:  "startedAt",
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
