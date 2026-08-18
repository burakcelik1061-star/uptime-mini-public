package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/auth"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// orgScopeFixture, org-scoped testlerin ortak kurulumu: kayıtlı bir
// kullanıcı, onun organizasyonu ve dışarıdan bir başka kullanıcı.
type orgScopeFixture struct {
	router   http.Handler
	orgs     *fakeOrgService
	ownerTok string
	ownerID  uuid.UUID
	orgID    uuid.UUID
	outsider string
	deps     Deps
}

// memberToken, organizasyona `member` rolüyle eklenmiş yeni bir kullanıcının
// token'ını döner (yetki testleri için).
func (f orgScopeFixture) memberToken(t *testing.T) string {
	t.Helper()

	token := authenticate(t, f.router, "uye-"+uuid.NewString()+"@example.com")
	f.orgs.addMember(f.orgID, userIDOf(t, token), store.OrgRoleMember)

	return token
}

// broadcaster, sahte canlı yayın yayıncısını döner (6.2).
func (f orgScopeFixture) broadcaster() (*recordingBroadcaster, bool) {
	b, ok := f.deps.Broadcaster.(*recordingBroadcaster)

	return b, ok
}

func newOrgScopeFixture(t *testing.T) orgScopeFixture {
	t.Helper()

	router, orgSvc, deps := newOrgTestRouterWithDeps(t)

	ownerTok := authenticate(t, router, testEmail)
	outsider := authenticate(t, router, "yabanci@example.com")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	orgID, err := uuid.Parse(decodeOrganization(t, rec.Body.Bytes()).ID)
	if err != nil {
		t.Fatalf("org id çözümlenemedi: %v", err)
	}

	return orgScopeFixture{
		router:   router,
		orgs:     orgSvc,
		ownerTok: ownerTok,
		ownerID:  orgSvc.createdBy[0],
		orgID:    orgID,
		outsider: outsider,
		deps:     deps,
	}
}

// errorBodyWithoutRequestID, iki cevabın AYNI olup olmadığını karşılaştırmak
// için requestId'yi ayıklar (o her istekte farklı).
func errorBodyWithoutRequestID(t *testing.T, body []byte) string {
	t.Helper()

	var parsed ErrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("hata gövdesi çözümlenemedi: %v (%s)", err, body)
	}
	parsed.Error.RequestID = ""

	normalized, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("gövde yeniden kodlanamadı: %v", err)
	}

	return string(normalized)
}

func TestOrgScopedEndpointRequiresAuthentication(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	rec := do(t, fixture.router, http.MethodGet, "/v1/orgs/"+fixture.orgID.String(), "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, beklenen 401 (%s)", rec.Code, rec.Body.String())
	}
}

func TestOrgScopedEndpointServesMember(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	rec := do(t, fixture.router, http.MethodGet, "/v1/orgs/"+fixture.orgID.String(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	org := decodeOrganization(t, rec.Body.Bytes())
	if org.ID != fixture.orgID.String() {
		t.Errorf("organizasyon = %s, beklenen %s", org.ID, fixture.orgID)
	}
	if org.Role != string(store.OrgRoleOwner) {
		t.Errorf("rol = %q, beklenen owner", org.Role)
	}
}

// BU ADIMIN ASIL TESTİ: "organizasyon yok" ile "üye değilsin" dışarıdan
// AYIRT EDİLEMEMELİ. Fark, saldırganın kimlikleri tek tek deneyerek varlık
// haritası çıkarmasına izin verirdi.
func TestNonMemberAndMissingOrganizationAreIndistinguishable(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	// Var olan organizasyon, üye olmayan kullanıcı.
	existing := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String(), "", bearer(fixture.outsider))

	// Hiç var olmayan organizasyon.
	missing := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+uuid.NewString(), "", bearer(fixture.outsider))

	// Biçimsiz kimlik.
	malformed := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/bu-bir-uuid-degil", "", bearer(fixture.outsider))

	for name, rec := range map[string]*http.Response{
		"üye değil":        existing.Result(),
		"organizasyon yok": missing.Result(),
		"biçimsiz uuid":    malformed.Result(),
	} {
		if rec.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, beklenen 404", name, rec.StatusCode)
		}
	}

	first := errorBodyWithoutRequestID(t, existing.Body.Bytes())
	second := errorBodyWithoutRequestID(t, missing.Body.Bytes())
	third := errorBodyWithoutRequestID(t, malformed.Body.Bytes())

	if first != second {
		t.Errorf("üye olmayan ile olmayan organizasyon farklı cevap aldı:\n%s\n%s", first, second)
	}
	if first != third {
		t.Errorf("biçimsiz uuid farklı cevap aldı:\n%s\n%s", first, third)
	}
}

// Bir organizasyonun verisine başka organizasyonun kullanıcısı asla
// erişememeli — üye listesi dahil.
func TestMembersEndpointIsIsolatedPerOrganization(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	// Dışarıdaki kullanıcının kendi organizasyonu var; bu ona diğerine
	// erişim hakkı vermemeli.
	if rec := do(t, fixture.router, http.MethodPost, "/v1/orgs", `{"name":"Yabancı A.Ş."}`,
		bearer(fixture.outsider)); rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d", rec.Code)
	}

	rec := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String()+"/members", "", bearer(fixture.outsider))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, beklenen 404 (%s)", rec.Code, rec.Body.String())
	}

	// Üye ise görebilmeli.
	rec = do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String()+"/members", "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	var body struct {
		Members []memberResponse `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}
	if len(body.Members) != 1 || body.Members[0].Role != string(store.OrgRoleOwner) {
		t.Errorf("üye listesi = %+v", body.Members)
	}
}

// Üyelik doğrulaması veritabanına gidemiyorsa istek AÇILMIYOR: yetki
// kontrolü belirsizlikte "izin ver" tarafına düşmemeli.
func TestOrgScopeFailsClosedOnDatabaseError(t *testing.T) {
	fixture := newOrgScopeFixture(t)
	fixture.orgs.membershipErr = errors.New("bağlantı koptu")

	rec := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String(), "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
	if code := decodeErrorResponse(t, rec).Error.Code; code != codeInternalServerError {
		t.Errorf("code = %q", code)
	}
}

// --- Rol gereksinimi ----------------------------------------------------------
//
// Üretimde bugün admin+ gerektiren bir uç YOK (monitor CRUD'u Faz 2.1'de
// gelecek). Mekanizma yine de burada, ÜRETİMDEKİ middleware'lerin aynısıyla
// kurulan bir alt-router üzerinde ölçülüyor — Faz 2'de ilk kullanan uç
// yazıldığında davranışın doğru olduğu zaten biliniyor olacak.

func newRoleGatedRouter(t *testing.T, service OrgService, required store.OrgRole) http.Handler {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens := auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute)

	r := chi.NewRouter()
	r.NotFound(notFoundHandler())
	r.Route("/v1/orgs/{orgID}", func(r chi.Router) {
		r.Use(requireAuth(tokens))
		r.Use(requireOrgMembership(service, logger))

		r.Group(func(r chi.Router) {
			r.Use(requireOrgRole(required, logger))

			r.Get("/ayarlar", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
		})
	})

	return r
}

func TestRequireOrgRoleAllowsEqualOrHigherRoles(t *testing.T) {
	cases := map[store.OrgRole]int{
		store.OrgRoleOwner:  http.StatusOK,
		store.OrgRoleAdmin:  http.StatusOK,
		store.OrgRoleMember: http.StatusForbidden,
	}

	for role, want := range cases {
		t.Run(string(role), func(t *testing.T) {
			service := newFakeOrgService()
			orgID, userID := uuid.New(), uuid.New()
			service.organizations[orgID] = store.Organization{ID: orgID, Name: "Acme", Slug: "acme"}
			service.addMember(orgID, userID, role)

			router := newRoleGatedRouter(t, service, store.OrgRoleAdmin)

			token, err := auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute).Issue(userID)
			if err != nil {
				t.Fatalf("token üretilemedi: %v", err)
			}

			rec := do(t, router, http.MethodGet, "/v1/orgs/"+orgID.String()+"/ayarlar", "", bearer(token.Value))
			if rec.Code != want {
				t.Fatalf("status = %d, beklenen %d (%s)", rec.Code, want, rec.Body.String())
			}

			if want == http.StatusForbidden {
				if code := decodeErrorResponse(t, rec).Error.Code; code != codeForbidden {
					t.Errorf("code = %q, beklenen %q", code, codeForbidden)
				}
			}
		})
	}
}

// Rolü yetmeyen ÜYE 403 alıyor, 404 değil: kullanıcı organizasyonun üyesi
// olduğunu zaten biliyor, gizlenecek varlık bilgisi yok.
func TestInsufficientRoleIsForbiddenNotHidden(t *testing.T) {
	service := newFakeOrgService()
	orgID, memberID, outsiderID := uuid.New(), uuid.New(), uuid.New()
	service.organizations[orgID] = store.Organization{ID: orgID, Name: "Acme", Slug: "acme"}
	service.addMember(orgID, memberID, store.OrgRoleMember)

	router := newRoleGatedRouter(t, service, store.OrgRoleOwner)
	tokens := auth.NewTokenService([]byte(testJWTSecret), 15*time.Minute)

	memberToken, err := tokens.Issue(memberID)
	if err != nil {
		t.Fatalf("token üretilemedi: %v", err)
	}
	outsiderToken, err := tokens.Issue(outsiderID)
	if err != nil {
		t.Fatalf("token üretilemedi: %v", err)
	}

	member := do(t, router, http.MethodGet, "/v1/orgs/"+orgID.String()+"/ayarlar", "", bearer(memberToken.Value))
	if member.Code != http.StatusForbidden {
		t.Errorf("üye için status = %d, beklenen 403", member.Code)
	}

	outsider := do(t, router, http.MethodGet, "/v1/orgs/"+orgID.String()+"/ayarlar", "", bearer(outsiderToken.Value))
	if outsider.Code != http.StatusNotFound {
		t.Errorf("üye olmayan için status = %d, beklenen 404", outsider.Code)
	}
}

// Rol kontrolü üyelik doğrulaması olmadan takılırsa istek AÇILMAMALI:
// yapılandırma hatası sessiz bir yetki açığına dönüşmesin.
func TestRequireOrgRoleWithoutMembershipMiddlewareFailsClosed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	r := chi.NewRouter()
	r.With(requireOrgRole(store.OrgRoleMember, logger)).
		Get("/yanlis-kurulmus", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

	rec := do(t, r, http.MethodGet, "/yanlis-kurulmus", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, beklenen 500 (%s)", rec.Code, rec.Body.String())
	}
}
