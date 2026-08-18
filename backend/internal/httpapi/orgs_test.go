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

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/orgs"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeOrgService, organizasyon servisinin HTTP katmanı için sahtesi.
//
// Slug üretimi ve çakışma çözümü burada DEĞİL, internal/orgs'un gerçek
// veritabanına karşı koşan testlerinde ölçülüyor (çakışmayı çözen şey
// veritabanının UNIQUE kısıtı). Buradaki konu sözleşme: yetki, doğrulama,
// durum kodu, gövde.
type fakeOrgService struct {
	byUser    map[uuid.UUID][]store.ListOrganizationsForUserRow
	createdBy []uuid.UUID
	createErr error
	listErr   error

	// memberships: orgID → userID → rol.
	memberships   map[uuid.UUID]map[uuid.UUID]store.OrgRole
	organizations map[uuid.UUID]store.Organization
	membershipErr error
	membersErr    error
}

func newFakeOrgService() *fakeOrgService {
	return &fakeOrgService{
		byUser:        map[uuid.UUID][]store.ListOrganizationsForUserRow{},
		memberships:   map[uuid.UUID]map[uuid.UUID]store.OrgRole{},
		organizations: map[uuid.UUID]store.Organization{},
	}
}

// setPlan, sahte organizasyonun planını ayarlar (8.5).
func (f *fakeOrgService) setPlan(orgID uuid.UUID, plan store.OrgPlan) {
	org := f.organizations[orgID]
	org.Plan = plan
	f.organizations[orgID] = org
}

// addMember, sahte servise üyelik ekler (davet akışı MVP dışı olduğu için
// gerçek bir uç yok).
func (f *fakeOrgService) addMember(orgID, userID uuid.UUID, role store.OrgRole) {
	if f.memberships[orgID] == nil {
		f.memberships[orgID] = map[uuid.UUID]store.OrgRole{}
	}
	f.memberships[orgID][userID] = role
}

func (f *fakeOrgService) MembershipFor(_ context.Context, orgID, userID uuid.UUID) (orgs.Membership, error) {
	if f.membershipErr != nil {
		return orgs.Membership{}, f.membershipErr
	}

	role, ok := f.memberships[orgID][userID]
	if !ok {
		// Organizasyon yok da olabilir, kullanıcı üye değil de — gerçek
		// servis gibi tek hata.
		return orgs.Membership{}, orgs.ErrNoAccess
	}

	org := f.organizations[orgID]

	return orgs.Membership{
		OrgID:   orgID,
		UserID:  userID,
		Role:    role,
		OrgName: org.Name,
		OrgSlug: org.Slug,
		// PLAN da üyelikten geliyor (8.5): monitor oluşturmanın varsayılan
		// kontrol aralığı buna bakıyor. Sahte org'un planı yazılmamışsa
		// `plans.For` en DAR planı (FREE) uyguluyor — gerçek servisin
		// davranışının aynısı.
		OrgPlan:  org.Plan,
		OrgSince: org.CreatedAt,
	}, nil
}

func (f *fakeOrgService) ListMembers(_ context.Context, orgID uuid.UUID) ([]store.ListOrganizationMembersRow, error) {
	if f.membersErr != nil {
		return nil, f.membersErr
	}

	rows := make([]store.ListOrganizationMembersRow, 0, len(f.memberships[orgID]))
	for userID, role := range f.memberships[orgID] {
		rows = append(rows, store.ListOrganizationMembersRow{
			ID:       userID,
			Email:    userID.String() + "@example.com",
			Name:     "Üye",
			Role:     role,
			JoinedAt: time.Now().UTC(),
		})
	}

	return rows, nil
}

func (f *fakeOrgService) Create(_ context.Context, ownerID uuid.UUID, name string) (store.Organization, error) {
	if f.createErr != nil {
		return store.Organization{}, f.createErr
	}

	f.createdBy = append(f.createdBy, ownerID)

	org := store.Organization{
		ID:        uuid.New(),
		Name:      name,
		Slug:      "org-" + uuid.NewString()[:8],
		CreatedAt: time.Now().UTC(),
	}

	f.organizations[org.ID] = org
	f.addMember(org.ID, ownerID, store.OrgRoleOwner)

	f.byUser[ownerID] = append(f.byUser[ownerID], store.ListOrganizationsForUserRow{
		ID:        org.ID,
		Name:      org.Name,
		Slug:      org.Slug,
		CreatedAt: org.CreatedAt,
		Role:      store.OrgRoleOwner,
	})

	return org, nil
}

func (f *fakeOrgService) ListForUser(_ context.Context, userID uuid.UUID) ([]store.ListOrganizationsForUserRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}

	return f.byUser[userID], nil
}

// authenticate, kayıt olup token döndürür (korumalı uçları çağırmak için).
func authenticate(t *testing.T, router http.Handler, email string) string {
	t.Helper()

	rec := postJSON(t, router, "/v1/auth/register", registerBody(email, testPassword, "Ali"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("kayıt başarısız: %d (%s)", rec.Code, rec.Body.String())
	}

	return decodeAuthResponse(t, rec).AccessToken
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func newOrgTestRouter(t *testing.T) (http.Handler, *fakeOrgService) {
	t.Helper()

	router, orgSvc, _ := newOrgTestRouterWithDeps(t)

	return router, orgSvc
}

// newOrgTestRouterWithDeps, bağımlılıkları da döner: 6.2'nin testleri sahte
// yayıncıya ve status page servisine erişmek zorunda.
func newOrgTestRouterWithDeps(t *testing.T) (http.Handler, *fakeOrgService, Deps) {
	t.Helper()

	deps := newTestDeps(t)
	orgSvc, ok := deps.Orgs.(*fakeOrgService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	return NewRouter(deps), orgSvc, deps
}

func decodeOrganization(t *testing.T, body []byte) organizationResponse {
	t.Helper()

	var wrapper struct {
		Organization organizationResponse `json:"organization"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Organization
}

func decodeOrganizations(t *testing.T, body []byte) []organizationResponse {
	t.Helper()

	var wrapper struct {
		Organizations []organizationResponse `json:"organizations"`
	}
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper.Organizations
}

// --- Oluşturma ---------------------------------------------------------------

func TestCreateOrganizationRequiresAuthentication(t *testing.T) {
	router, orgSvc := newOrgTestRouter(t)

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
	}
	if len(orgSvc.createdBy) != 0 {
		t.Error("kimlik doğrulanmadan organizasyon oluşturuldu")
	}
}

func TestCreateOrganizationReturnsOwnerRole(t *testing.T) {
	router, orgSvc := newOrgTestRouter(t)
	token := authenticate(t, router, testEmail)

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	org := decodeOrganization(t, rec.Body.Bytes())
	if org.Name != "Acme Corp" {
		t.Errorf("ad = %q", org.Name)
	}
	if org.Slug == "" {
		t.Error("slug boş — sunucu üretmeliydi")
	}
	if org.Role != string(store.OrgRoleOwner) {
		t.Errorf("rol = %q, beklenen owner", org.Role)
	}
	if org.ID == "" {
		t.Error("id boş")
	}

	// Organizasyon, isteği yapan kullanıcı adına açılmalı.
	if len(orgSvc.createdBy) != 1 {
		t.Fatalf("servis %d kez çağrıldı", len(orgSvc.createdBy))
	}
	me := do(t, router, http.MethodGet, "/v1/me", "", bearer(token))
	var meBody struct {
		User userResponse `json:"user"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &meBody); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}
	if orgSvc.createdBy[0].String() != meBody.User.ID {
		t.Errorf("sahip = %s, beklenen %s", orgSvc.createdBy[0], meBody.User.ID)
	}
}

func TestCreateOrganizationValidatesName(t *testing.T) {
	cases := map[string]string{
		"ad yok":        `{"name":""}`,
		"yalnız boşluk": `{"name":"   "}`,
		"çok uzun":      `{"name":"` + strings.Repeat("a", 121) + `"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router, orgSvc := newOrgTestRouter(t)
			token := authenticate(t, router, testEmail)

			rec := do(t, router, http.MethodPost, "/v1/orgs", body, bearer(token))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
			}

			fields := decodeErrorResponse(t, rec).Error.Fields
			if len(fields) == 0 || fields[0].Field != "name" {
				t.Errorf("hatalı alan bildirilmedi: %+v", fields)
			}
			if len(orgSvc.createdBy) != 0 {
				t.Error("doğrulama başarısızken organizasyon oluşturuldu")
			}
		})
	}
}

// Slug istemciden alınmıyor: gövdede gönderilirse tanımsız alan olarak
// reddedilmeli, sessizce yok sayılmamalı.
func TestCreateOrganizationRejectsClientProvidedSlug(t *testing.T) {
	router, _ := newOrgTestRouter(t)
	token := authenticate(t, router, testEmail)

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme","slug":"benim-slugim"}`, bearer(token))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, beklenen 400 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeInvalidJSON {
		t.Errorf("code = %q", code)
	}
}

func TestCreateOrganizationHidesInternalErrors(t *testing.T) {
	router, orgSvc := newOrgTestRouter(t)
	token := authenticate(t, router, testEmail)
	orgSvc.createErr = errors.New("slug üretilemedi: bağlantı koptu")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme"}`, bearer(token))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "bağlantı koptu") {
		t.Error("iç hata metni istemciye sızdı")
	}
}

// --- Listeleme ---------------------------------------------------------------

func TestListOrganizationsRequiresAuthentication(t *testing.T) {
	router, _ := newOrgTestRouter(t)

	if rec := do(t, router, http.MethodGet, "/v1/orgs", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401", rec.Code)
	}
}

func TestListOrganizationsReturnsOnlyOwnMemberships(t *testing.T) {
	router, orgSvc := newOrgTestRouter(t)

	mine := authenticate(t, router, testEmail)
	theirs := authenticate(t, router, "baskasi@example.com")

	if rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Benim"}`, bearer(mine)); rec.Code != http.StatusCreated {
		t.Fatalf("oluşturma başarısız: %d", rec.Code)
	}
	if rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Onunki"}`, bearer(theirs)); rec.Code != http.StatusCreated {
		t.Fatalf("oluşturma başarısız: %d", rec.Code)
	}

	rec := do(t, router, http.MethodGet, "/v1/orgs", "", bearer(mine))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	organizations := decodeOrganizations(t, rec.Body.Bytes())
	if len(organizations) != 1 {
		t.Fatalf("organizasyon sayısı = %d, beklenen 1 (%+v)", len(organizations), organizations)
	}
	if organizations[0].Name != "Benim" {
		t.Errorf("başka kullanıcının organizasyonu listelendi: %q", organizations[0].Name)
	}
	if organizations[0].Role != string(store.OrgRoleOwner) {
		t.Errorf("rol = %q", organizations[0].Role)
	}

	// Sahte servis iki ayrı kullanıcı için çağrılmış olmalı.
	if len(orgSvc.createdBy) != 2 || orgSvc.createdBy[0] == orgSvc.createdBy[1] {
		t.Errorf("beklenmeyen sahip listesi: %v", orgSvc.createdBy)
	}
}

// Boş liste JSON'da `null` değil `[]` olmalı: istemci "yok" ile "boş"
// ayrımını özel duruma çevirmesin.
func TestListOrganizationsReturnsEmptyArrayNotNull(t *testing.T) {
	router, _ := newOrgTestRouter(t)
	token := authenticate(t, router, testEmail)

	rec := do(t, router, http.MethodGet, "/v1/orgs", "", bearer(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), `"organizations":[]`) {
		t.Errorf("boş liste `[]` olarak dönmedi: %s", rec.Body.String())
	}
}

func TestListOrganizationsHidesInternalErrors(t *testing.T) {
	router, orgSvc := newOrgTestRouter(t)
	token := authenticate(t, router, testEmail)
	orgSvc.listErr = errors.New("bağlantı koptu")

	rec := do(t, router, http.MethodGet, "/v1/orgs", "", bearer(token))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "bağlantı koptu") {
		t.Error("iç hata metni istemciye sızdı")
	}
}
