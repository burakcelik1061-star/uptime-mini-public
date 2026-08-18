package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/statuspage"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeStatusPageService, status page servisinin HTTP katmanı için sahtesi.
// SAKLAMA sahte, DOĞRULAMA gerçek (`statuspage.ValidateItems`).
//
// SIZINTI KURALI BURADA ÖLÇÜLMÜYOR: onu uygulayan şey sorgular, testi de
// gerçek veritabanına karşı (internal/statuspage). Buradaki testler
// sözleşmenin HTTP tarafını ölçüyor — yetki, doğrulama, 404.
type fakeStatusPageService struct {
	pages map[uuid.UUID]*fakeStatusPage
	// bySlug, public uç için: yayımlanmış sayfaların slug dizini.
	bySlug   map[string]uuid.UUID
	failWith error
}

type fakeStatusPage struct {
	page   store.StatusPage
	groups []string
	items  []statuspage.Item
}

func newFakeStatusPageService() *fakeStatusPageService {
	return &fakeStatusPageService{
		pages:  map[uuid.UUID]*fakeStatusPage{},
		bySlug: map[string]uuid.UUID{},
	}
}

// register, testin org için bir sayfa kurmasını sağlar.
func (f *fakeStatusPageService) register(orgID uuid.UUID, slug string) {
	f.pages[orgID] = &fakeStatusPage{page: store.StatusPage{
		ID:        uuid.New(),
		OrgID:     orgID,
		Slug:      slug,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}}
	f.bySlug[slug] = orgID
}

func (f *fakeStatusPageService) lookup(orgID uuid.UUID) (*fakeStatusPage, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}

	page, ok := f.pages[orgID]
	if !ok {
		return nil, statuspage.ErrNotFound
	}

	return page, nil
}

func (f *fakeStatusPageService) Get(_ context.Context, orgID uuid.UUID) (statuspage.Settings, error) {
	page, err := f.lookup(orgID)
	if err != nil {
		return statuspage.Settings{}, err
	}

	return statuspage.Settings{Page: page.page, Items: page.items}, nil
}

// UpdateSettings, gerçek servisin PATCH birleştirmesini taklit ediyor: sahte
// olan SAKLAMA, doğrulama gerçek.
func (f *fakeStatusPageService) UpdateSettings(
	_ context.Context, orgID uuid.UUID, patch statuspage.SettingsPatch,
) (statuspage.Settings, error) {
	page, err := f.lookup(orgID)
	if err != nil {
		return statuspage.Settings{}, err
	}

	if patch.IsEmpty() {
		return statuspage.Settings{}, &statuspage.ValidationError{
			Problems: []statuspage.Problem{{Field: "body", Message: "en az bir alan gönderilmeli"}},
		}
	}

	if patch.SetThemeColor && patch.ThemeColor != nil && !themeColorLooksValid(*patch.ThemeColor) {
		return statuspage.Settings{}, &statuspage.ValidationError{
			Problems: []statuspage.Problem{{Field: "themeColor", Message: "#rrggbb biçiminde bir renk olmalı"}},
		}
	}

	if patch.IsPublished != nil {
		page.page.IsPublished = *patch.IsPublished
	}
	if patch.SetTitle {
		page.page.Title = patch.Title
	}
	if patch.SetDescription {
		page.page.Description = patch.Description
	}
	if patch.SetLogoURL {
		page.page.LogoUrl = patch.LogoURL
	}
	if patch.SetThemeColor {
		page.page.ThemeColor = patch.ThemeColor
	}

	page.page.UpdatedAt = time.Now().UTC()

	return statuspage.Settings{Page: page.page, Groups: page.groups, Items: page.items}, nil
}

// themeColorLooksValid, sahte servisin renk denetimi (gerçek kural
// statuspage.settingsValues.validate içinde ve orada test ediliyor).
func themeColorLooksValid(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}

	for _, char := range value[1:] {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}

	return true
}

func (f *fakeStatusPageService) ReplaceServices(
	_ context.Context, orgID uuid.UUID, in statuspage.ServicesInput,
) (statuspage.Settings, error) {
	page, err := f.lookup(orgID)
	if err != nil {
		return statuspage.Settings{}, err
	}

	in = statuspage.NormalizeServices(in)
	if err := statuspage.ValidateServices(in); err != nil {
		return statuspage.Settings{}, err
	}

	items := make([]statuspage.Item, 0, len(in.Items))
	for _, input := range in.Items {
		item := statuspage.Item{
			MonitorID: input.MonitorID,
			Name:      "API",
			URL:       "https://example.com/health",
			Status:    store.MonitorStatusUp,
		}

		if input.Group != "" {
			group := input.Group
			item.Group = &group
		}

		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].MonitorID.String() < items[j].MonitorID.String()
	})

	page.groups = in.Groups
	page.items = items

	return statuspage.Settings{Page: page.page, Groups: in.Groups, Items: items}, nil
}

// PublishedSlug, public WebSocket kanalının kapısı (6.2) — okuma ucuyla AYNI
// kararı veriyor: yayımlanmamış slug ile olmayan slug ayırt edilemiyor.
func (f *fakeStatusPageService) PublishedSlug(ctx context.Context, slug string) error {
	if _, err := f.Public(ctx, slug); err != nil {
		return err
	}

	return nil
}

func (f *fakeStatusPageService) Public(_ context.Context, slug string) (statuspage.PublicPage, error) {
	if f.failWith != nil {
		return statuspage.PublicPage{}, f.failWith
	}

	orgID, ok := f.bySlug[statuspage.NormalizeSlug(slug)]
	if !ok {
		return statuspage.PublicPage{}, statuspage.ErrNotFound
	}

	page := f.pages[orgID]
	if !page.page.IsPublished {
		// Yayımlanmamış sayfa ile olmayan slug AYNI hatayı veriyor.
		return statuspage.PublicPage{}, statuspage.ErrNotFound
	}

	services := make([]statuspage.PublicService, 0, len(page.items))
	for _, item := range page.items {
		services = append(services, statuspage.PublicService{
			ID:              item.MonitorID,
			Name:            item.Name,
			Status:          item.Status,
			StatusChangedAt: time.Now().UTC(),
		})
	}

	title := "Acme Corp"
	if page.page.Title != nil {
		title = *page.page.Title
	}

	return statuspage.PublicPage{
		Slug:      page.page.Slug,
		Title:     title,
		Groups:    page.groups,
		Overall:   statuspage.Summarize(services),
		Services:  services,
		Incidents: []statuspage.PublicIncident{},
	}, nil
}

// --- Test kurulumu -----------------------------------------------------------

type statusPageFixture struct {
	router    http.Handler
	service   *fakeStatusPageService
	orgID     uuid.UUID
	slug      string
	ownerTok  string
	memberTok string
	outsider  string
}

func newStatusPageFixture(t *testing.T) statusPageFixture {
	t.Helper()

	deps := newTestDeps(t)
	pageSvc, ok := deps.StatusPages.(*fakeStatusPageService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}
	orgSvc, ok := deps.Orgs.(*fakeOrgService)
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	router := NewRouter(deps)

	ownerTok := authenticate(t, router, testEmail)
	memberTok := authenticate(t, router, "sayfa-uye@example.com")
	outsider := authenticate(t, router, "sayfa-yabanci@example.com")

	rec := do(t, router, http.MethodPost, "/v1/orgs", `{"name":"Acme Corp"}`, bearer(ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("organizasyon oluşturulamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	orgID, err := uuid.Parse(decodeOrganization(t, rec.Body.Bytes()).ID)
	if err != nil {
		t.Fatalf("org id çözümlenemedi: %v", err)
	}

	orgSvc.addMember(orgID, userIDOf(t, memberTok), store.OrgRoleMember)

	slug := "acme-corp"
	pageSvc.register(orgID, slug)

	return statusPageFixture{
		router:    router,
		service:   pageSvc,
		orgID:     orgID,
		slug:      slug,
		ownerTok:  ownerTok,
		memberTok: memberTok,
		outsider:  outsider,
	}
}

func (f statusPageFixture) path(suffix string) string {
	return "/v1/orgs/" + f.orgID.String() + "/status-page" + suffix
}

type statusPageBodyShape struct {
	StatusPage statusPageResponse       `json:"statusPage"`
	Groups     []string                 `json:"groups"`
	Items      []statusPageItemResponse `json:"items"`
}

func decodeStatusPage(t *testing.T, body []byte) statusPageBodyShape {
	t.Helper()

	var wrapper statusPageBodyShape
	if err := json.Unmarshal(body, &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return wrapper
}

func decodePublicPage(t *testing.T, body []byte) publicPageResponse {
	t.Helper()

	var page publicPageResponse
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v (%s)", err, body)
	}

	return page
}

// --- Admin uçları ------------------------------------------------------------

func TestStatusPageGetReturnsSlugAndPublicPath(t *testing.T) {
	f := newStatusPageFixture(t)

	rec := do(t, f.router, http.MethodGet, f.path(""), "", bearer(f.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}

	body := decodeStatusPage(t, rec.Body.Bytes())
	if body.StatusPage.Slug != f.slug {
		t.Errorf("slug = %q", body.StatusPage.Slug)
	}
	if body.StatusPage.IsPublished {
		t.Error("yeni sayfa yayımda görünüyor — varsayılan yayımlanmamış olmalı")
	}
	// Yol sunucudan dönüyor ki istemci "/status/" önekini kendi kurmasın.
	if body.StatusPage.PublicPath != "/status/"+f.slug {
		t.Errorf("publicPath = %q", body.StatusPage.PublicPath)
	}
	if body.Items == nil {
		t.Error("boş seçim null döndü, [] olmalı")
	}
}

func TestStatusPageReadIsOpenToMembers(t *testing.T) {
	f := newStatusPageFixture(t)

	rec := do(t, f.router, http.MethodGet, f.path(""), "", bearer(f.memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("üye okuma durumu = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestStatusPageWritesRequireAdmin(t *testing.T) {
	f := newStatusPageFixture(t)

	monitorID := uuid.New()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"yayımlama", http.MethodPatch, f.path(""), `{"isPublished":true}`},
		{"servis seçimi", http.MethodPut, f.path("/services"),
			`{"groups":[],"items":[{"monitorId":"` + monitorID.String() + `","group":null}]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, f.router, tc.method, tc.path, tc.body, bearer(f.memberTok))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("member için durum = %d (%s), beklenen 403", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestStatusPageHiddenFromNonMembers(t *testing.T) {
	f := newStatusPageFixture(t)

	// Yabancı kullanıcı organizasyonun VARLIĞINI bile öğrenmemeli: 404,
	// 403 değil (1.4'ün kararı).
	rec := do(t, f.router, http.MethodGet, f.path(""), "", bearer(f.outsider))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("durum = %d (%s), beklenen 404", rec.Code, rec.Body.String())
	}
}

func TestStatusPageReplaceServicesReplacesSelection(t *testing.T) {
	f := newStatusPageFixture(t)

	first, second := uuid.New(), uuid.New()

	rec := do(t, f.router, http.MethodPut, f.path("/services"),
		`{"groups":["API"],"items":[{"monitorId":"`+first.String()+`","group":"API"},`+
			`{"monitorId":"`+second.String()+`","group":null}]}`, bearer(f.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeStatusPage(t, rec.Body.Bytes())
	if len(body.Items) != 2 {
		t.Fatalf("servis sayısı = %d, beklenen 2", len(body.Items))
	}
	if len(body.Groups) != 1 || body.Groups[0] != "API" {
		t.Fatalf("gruplar = %v", body.Groups)
	}

	// Boş liste geçerli: yayımdan kaldırmadan servisleri geri çekmek.
	rec = do(t, f.router, http.MethodPut, f.path("/services"), `{"groups":[],"items":[]}`, bearer(f.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}
	if items := decodeStatusPage(t, rec.Body.Bytes()).Items; len(items) != 0 {
		t.Fatalf("servis sayısı = %d, beklenen 0", len(items))
	}
}

func TestStatusPageReplaceServicesRejectsBadInput(t *testing.T) {
	f := newStatusPageFixture(t)
	id := uuid.New().String()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"biçimsiz kimlik", `{"groups":[],"items":[{"monitorId":"abc","group":null}]}`,
			http.StatusUnprocessableEntity},
		{"yinelenen kimlik",
			`{"groups":[],"items":[{"monitorId":"` + id + `","group":null},` +
				`{"monitorId":"` + id + `","group":null}]}`, http.StatusUnprocessableEntity},
		{"tanınmayan grup",
			`{"groups":["API"],"items":[{"monitorId":"` + id + `","group":"Web"}]}`,
			http.StatusUnprocessableEntity},
		{"yinelenen grup", `{"groups":["API","API"],"items":[]}`, http.StatusUnprocessableEntity},
		{"tanımsız alan", `{"groups":[],"items":[],"title":"x"}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, f.router, http.MethodPut, f.path("/services"), tc.body, bearer(f.ownerTok))
			if rec.Code != tc.want {
				t.Fatalf("durum = %d (%s), beklenen %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestStatusPageUpdateRejectsEmptyBody(t *testing.T) {
	f := newStatusPageFixture(t)

	// Boş gövde sessizce "hiçbir şey yapma" olmamalı: istemci ne istediğini
	// söylemeli.
	rec := do(t, f.router, http.MethodPatch, f.path(""), `{}`, bearer(f.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("durum = %d (%s), beklenen 422", rec.Code, rec.Body.String())
	}
}

// Görünüm alanları ÜÇ DURUMLU: gönderilmedi · null (temizle) · değer (5.3).
func TestStatusPageUpdateAppliesAppearanceFields(t *testing.T) {
	f := newStatusPageFixture(t)

	rec := do(t, f.router, http.MethodPatch, f.path(""),
		`{"title":"Acme durumu","themeColor":"#aabbcc"}`, bearer(f.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}

	page := decodeStatusPage(t, rec.Body.Bytes()).StatusPage
	if page.Title == nil || *page.Title != "Acme durumu" {
		t.Fatalf("başlık = %v", page.Title)
	}

	// `null` göndermek alanı TEMİZLİYOR; gönderilmeyen alan duruyor.
	rec = do(t, f.router, http.MethodPatch, f.path(""), `{"title":null}`, bearer(f.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}

	page = decodeStatusPage(t, rec.Body.Bytes()).StatusPage
	if page.Title != nil {
		t.Errorf("başlık = %v, temizlenmeliydi", *page.Title)
	}
	if page.ThemeColor == nil || *page.ThemeColor != "#aabbcc" {
		t.Errorf("tema rengi = %v, dokunulmamalıydı", page.ThemeColor)
	}

	// Biçimsiz renk 422.
	rec = do(t, f.router, http.MethodPatch, f.path(""), `{"themeColor":"kirmizi"}`, bearer(f.ownerTok))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("biçimsiz renk için durum = %d (%s)", rec.Code, rec.Body.String())
	}
}

// --- Public uç ---------------------------------------------------------------

func TestPublicStatusNeedsNoToken(t *testing.T) {
	f := newStatusPageFixture(t)

	monitorID := uuid.New()
	if rec := do(t, f.router, http.MethodPut, f.path("/services"),
		`{"groups":[],"items":[{"monitorId":"`+monitorID.String()+`","group":null}]}`,
		bearer(f.ownerTok)); rec.Code != http.StatusOK {
		t.Fatalf("servisler yayımlanamadı: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := do(t, f.router, http.MethodPatch, f.path(""),
		`{"isPublished":true}`, bearer(f.ownerTok)); rec.Code != http.StatusOK {
		t.Fatalf("sayfa yayımlanamadı: %d (%s)", rec.Code, rec.Body.String())
	}

	// TOKEN YOK: bu ucun tamamı oturumsuz.
	rec := do(t, f.router, http.MethodGet, "/v1/public/status/"+f.slug, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("durum = %d (%s)", rec.Code, rec.Body.String())
	}

	page := decodePublicPage(t, rec.Body.Bytes())
	if page.Slug != f.slug || page.Title != "Acme Corp" {
		t.Errorf("sayfa = %+v", page)
	}
	if page.Overall != string(statuspage.OverallOperational) {
		t.Errorf("genel durum = %q", page.Overall)
	}
	if len(page.Services) != 1 || page.Services[0].ID != monitorID.String() {
		t.Fatalf("servisler = %+v", page.Services)
	}
	if page.Incidents == nil || page.Maintenance == nil {
		t.Error("boş listeler null döndü, [] olmalı")
	}
}

func TestPublicStatusUnpublishedAndUnknownAreIdentical(t *testing.T) {
	f := newStatusPageFixture(t)

	// Sayfa var ama yayımlanmamış.
	unpublished := do(t, f.router, http.MethodGet, "/v1/public/status/"+f.slug, "", nil)
	// Böyle bir slug hiç yok.
	unknown := do(t, f.router, http.MethodGet, "/v1/public/status/boyle-bir-sey-yok", "", nil)

	if unpublished.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("durumlar = %d / %d, ikisi de 404 olmalı", unpublished.Code, unknown.Code)
	}

	// Cevaplar da AYNI olmalı (requestId hariç): fark üretmek, hangi
	// slug'ların kayıtlı olduğunu dışarıya söylemek olurdu.
	unpublishedErr := decodeErrorResponse(t, unpublished).Error
	unknownErr := decodeErrorResponse(t, unknown).Error

	if unpublishedErr.Code != unknownErr.Code || unpublishedErr.Message != unknownErr.Message {
		t.Errorf("yayımlanmamış sayfa (%s/%q) ile olmayan sayfa (%s/%q) farklı cevap verdi",
			unpublishedErr.Code, unpublishedErr.Message, unknownErr.Code, unknownErr.Message)
	}
}

func TestPublicStatusIgnoresAuthorizationHeader(t *testing.T) {
	f := newStatusPageFixture(t)

	// Geçersiz bir token public ucu ETKİLEMEMELİ: uç kimlik doğrulamıyor.
	rec := do(t, f.router, http.MethodGet, "/v1/public/status/"+f.slug, "",
		map[string]string{"Authorization": "Bearer gecersiz.token.degeri"})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("durum = %d (%s), beklenen 404 (sayfa yayımda değil)", rec.Code, rec.Body.String())
	}
}
