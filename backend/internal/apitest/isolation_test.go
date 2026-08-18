package apitest

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// tenant, tek bir organizasyonun testte kullanılan tüm kaynakları.
//
// Rota gezme testinin (aşağıda) yol parametrelerini GERÇEK kimliklerle
// doldurması gerekiyor: uydurma bir uuid ile gelen istek 404'ü "kaynak yok"
// yüzünden alabilir ve izolasyonun çalıştığını kanıtlamazdı.
type tenant struct {
	owner   User
	org     Org
	monitor Monitor
	// Kimlikler yol parametrelerinin karşılıkları.
	incidentID string
	windowID   string
	channelID  string
}

func newTenant(t *testing.T, h *Harness, target string) tenant {
	t.Helper()

	owner := h.Register()
	org := h.CreateOrg(owner)
	monitor := h.CreateMonitor(owner, org, "Ana API", target)

	resp := h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/orgs/%s/incidents", org.ID),
		Token:  owner.Token,
		Body: fmt.Sprintf(
			`{"title":"Elle açılmış kesinti","impact":"major","monitorIds":[%q],"body":"İnceleniyor"}`,
			monitor.ID),
	}), http.StatusCreated)

	var incident struct {
		Incident struct {
			ID string `json:"id"`
		} `json:"incident"`
	}
	h.JSON(resp, &incident)

	starts := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ends := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)

	resp = h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/orgs/%s/maintenance-windows", org.ID),
		Token:  owner.Token,
		Body: fmt.Sprintf(
			`{"title":"Planlı bakım","startsAt":%q,"endsAt":%q,"monitorIds":[%q]}`,
			starts, ends, monitor.ID),
	}), http.StatusCreated)

	var window struct {
		Window struct {
			ID string `json:"id"`
		} `json:"maintenanceWindow"`
	}
	h.JSON(resp, &window)

	resp = h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/orgs/%s/notification-channels", org.ID),
		Token:  owner.Token,
		Body:   `{"kind":"email","name":"Nöbetçi","target":"oncall@example.test"}`,
	}), http.StatusCreated)

	var channel struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	h.JSON(resp, &channel)

	return tenant{
		owner:      owner,
		org:        org,
		monitor:    monitor,
		incidentID: incident.Incident.ID,
		windowID:   window.Window.ID,
		channelID:  channel.Channel.ID,
	}
}

// resolve, bir rota kalıbını bu kiracının gerçek kimlikleriyle doldurur.
func (tn tenant) resolve(route string) string {
	replacer := strings.NewReplacer(
		"{orgID}", tn.org.ID.String(),
		"{monitorID}", tn.monitor.ID.String(),
		"{incidentID}", tn.incidentID,
		"{windowID}", tn.windowID,
		"{channelID}", tn.channelID,
	)

	path := replacer.Replace(route)

	// chi alt-router'ların dizinini `/…/monitors/` gibi bir eğik çizgiyle
	// veriyor; api bu yolu da tanıyor ama testin gönderdiği adres gerçek
	// istemcinin gönderdiğine benzesin.
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}

	return path
}

// orgScopedRoutes, router'ın KAYDETTİĞİ tüm org-scoped rotaları döner.
//
// Liste elle tutulmuyor: bu testin bütün değeri, YARIN EKLENECEK bir ucun da
// kendiliğinden kapsanmasında. Elle yazılmış bir listede yeni bir uç eklemek
// ve listeye yazmayı unutmak, izolasyonu test edilmemiş bir uç bırakırdı —
// canny-mini'de yetki kontrolünün handler'lara dağılmasıyla doğan sessiz
// sızıntının aynısı (1.4).
func orgScopedRoutes(t *testing.T, h *Harness) map[string][]string {
	t.Helper()

	// Gezilen router, isteklerin gittiği router'ın TA KENDİSİ: ayrıca
	// kurulan bir kopya, gerçek sunucuda olmayan bir rotayı test edebilirdi.
	routes, ok := h.Handler.(chi.Routes)
	if !ok {
		t.Fatal("router chi.Routes değil; bu test yolları gezemiyor")
	}

	found := make(map[string][]string)

	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.Contains(route, "{orgID}") {
			return nil
		}
		found[route] = append(found[route], method)

		return nil
	})
	if err != nil {
		t.Fatalf("rotalar gezilemedi: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("hiç org-scoped rota bulunamadı — bu test bir şey ölçmüyor demektir")
	}

	return found
}

// Org-scoped ağacın TAMAMI yabancı bir kullanıcıya kapalı olmalı.
//
// 1.4'ün garantisi "kontrolü atlamak için ağacın dışına çıkmak gerekir" idi;
// bu test o garantinin uç uç değil, ROTA LİSTESİ üzerinden ölçülmüş hâli.
func TestEveryOrgScopedRouteIsClosedToOutsiders(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)
	victim := newTenant(t, h, target)

	// Yabancı: kendi organizasyonu OLAN ama bu organizasyona üye olmayan
	// gerçek bir kullanıcı. Oturumsuz bir istek yalnızca `requireAuth`ı
	// ölçerdi; buradaki soru üyelik denetimi.
	outsider := h.Register()
	h.CreateOrg(outsider)

	for route, methods := range orgScopedRoutes(t, h) {
		path := victim.resolve(route)

		for _, method := range methods {
			t.Run(method+" "+route, func(t *testing.T) {
				resp := h.Do(Request{
					Method: method,
					Path:   path,
					Token:  outsider.Token,
					// Gövde BİLİNÇLİ OLARAK BOŞ: üyelik denetimi gövde
					// ayrıştırılmadan ÖNCE çalışmalı. 422 dönseydi, sunucu
					// yabancı bir organizasyonun isteğini doğrulamaya kadar
					// taşımış olurdu.
				})

				if resp.Status != http.StatusNotFound {
					t.Errorf("durum = %d, beklenen 404 (gövde: %s)", resp.Status, resp.Body)
				}

				// Kaynağın ADI da sızmamalı: 404'ün gövdesi, denenen
				// kimliğin gerçekten var olduğunu ele vermemeli.
				if BodyContains(resp.Body, victim.monitor.Name) {
					t.Errorf("cevap yabancı organizasyonun monitor adını içeriyor: %s", resp.Body)
				}
			})
		}
	}
}

// Aynı ağaç oturumsuz isteğe de kapalı: yeni bir uç `requireAuth` grubunun
// dışına düşerse burada yakalanır.
func TestEveryOrgScopedRouteRequiresAuthentication(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)
	victim := newTenant(t, h, target)

	for route, methods := range orgScopedRoutes(t, h) {
		path := victim.resolve(route)

		for _, method := range methods {
			t.Run(method+" "+route, func(t *testing.T) {
				resp := h.Do(Request{Method: method, Path: path})

				if resp.Status != http.StatusUnauthorized {
					t.Errorf("durum = %d, beklenen 401 (gövde: %s)", resp.Status, resp.Body)
				}
			})
		}
	}
}

// "Organizasyon yok" ile "üye değilsin" GERÇEK veritabanına karşı da
// ayırt edilemiyor (1.4). Bugüne kadar bu yalnızca sahte bir org servisiyle
// ölçülüyordu — asıl kararın sahibi ise SQL sorgusu (tek JOIN).
func TestMissingOrgAndForeignOrgAreIndistinguishable(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)
	victim := newTenant(t, h, target)

	outsider := h.Register()

	foreign := h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/monitors", victim.org.ID),
		Token:  outsider.Token,
	})
	missing := h.Do(Request{
		Method: http.MethodGet,
		Path:   "/v1/orgs/00000000-0000-0000-0000-000000000000/monitors",
		Token:  outsider.Token,
	})
	malformed := h.Do(Request{
		Method: http.MethodGet,
		Path:   "/v1/orgs/bu-bir-uuid-degil/monitors",
		Token:  outsider.Token,
	})

	for name, resp := range map[string]Response{
		"yabancı org": foreign, "olmayan org": missing, "biçimsiz uuid": malformed,
	} {
		if resp.Status != http.StatusNotFound {
			t.Errorf("%s: durum = %d, beklenen 404", name, resp.Status)
		}
	}

	// Gövdeler de aynı olmalı — `requestId` hariç, o her istekte farklı.
	if code := h.ErrorCode(foreign); code != h.ErrorCode(missing) || code != h.ErrorCode(malformed) {
		t.Errorf("hata kodları ayrışıyor: %q / %q / %q",
			h.ErrorCode(foreign), h.ErrorCode(missing), h.ErrorCode(malformed))
	}
}

// `member` OKUYABİLİR ama YAZAMAZ: rol kuralı gerçek üyelik satırına ve
// gerçek middleware zincirine karşı ölçülüyor.
func TestMemberCanReadButNotWrite(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)
	tn := newTenant(t, h, target)

	member := h.Register()
	h.AddMember(tn.org, member, store.OrgRoleMember)

	// Okuma: 200.
	h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/monitors", tn.org.ID),
		Token:  member.Token,
	}), http.StatusOK)

	// Yazma uçlarının hepsi 403 — ve 404 DEĞİL: üye olduğunu zaten biliyor,
	// gizlenecek bir varlık bilgisi yok (1.4).
	writes := []Request{
		{Method: http.MethodPost, Path: fmt.Sprintf("/v1/orgs/%s/monitors", tn.org.ID),
			Body: `{"name":"Yeni","url":"https://example.test"}`},
		{Method: http.MethodPatch, Path: fmt.Sprintf("/v1/orgs/%s/monitors/%s", tn.org.ID, tn.monitor.ID),
			Body: `{"name":"Başka ad"}`},
		{Method: http.MethodDelete, Path: fmt.Sprintf("/v1/orgs/%s/monitors/%s", tn.org.ID, tn.monitor.ID)},
		{Method: http.MethodPost, Path: fmt.Sprintf("/v1/orgs/%s/monitors/%s/check", tn.org.ID, tn.monitor.ID)},
		{Method: http.MethodPost, Path: fmt.Sprintf("/v1/orgs/%s/incidents", tn.org.ID),
			Body: `{"title":"X","impact":"minor","monitorIds":[],"body":"y"}`},
		{Method: http.MethodPatch, Path: fmt.Sprintf("/v1/orgs/%s/status-page", tn.org.ID),
			Body: `{"isPublished":true}`},
		{Method: http.MethodPut, Path: fmt.Sprintf("/v1/orgs/%s/status-page/services", tn.org.ID),
			Body: `{"groups":[],"items":[]}`},
		{Method: http.MethodPost, Path: fmt.Sprintf("/v1/orgs/%s/notification-channels", tn.org.ID),
			Body: `{"kind":"email","name":"X","target":"a@example.test"}`},
	}

	for _, write := range writes {
		write.Token = member.Token

		resp := h.Do(write)
		if resp.Status != http.StatusForbidden {
			t.Errorf("%s %s: durum = %d, beklenen 403 (gövde: %s)",
				write.Method, write.Path, resp.Status, resp.Body)
		}
	}

	// Ve yazma GERÇEKTEN olmamış olmalı: 403 dönüp yan etkiyi uygulamak
	// en kötü hata olurdu.
	h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/monitors/%s", tn.org.ID, tn.monitor.ID),
		Token:  tn.owner.Token,
	}), http.StatusOK)
}

// Timeline'a yazmak ÜYEYE açık, kapatmak admin+ (4.2). İki kademeli yetkinin
// gerçek zincirdeki karşılığı.
func TestMemberWritesTimelineButCannotResolve(t *testing.T) {
	h := New(t)

	target := newTargetServer(t, http.StatusOK)
	tn := newTenant(t, h, target)

	member := h.Register()
	h.AddMember(tn.org, member, store.OrgRoleMember)

	path := fmt.Sprintf("/v1/orgs/%s/incidents/%s/updates", tn.org.ID, tn.incidentID)

	h.Expect(h.Do(Request{
		Method: http.MethodPost,
		Path:   path,
		Token:  member.Token,
		Body:   `{"status":"identified","body":"Sebep bulundu"}`,
	}), http.StatusCreated)

	resp := h.Do(Request{
		Method: http.MethodPost,
		Path:   path,
		Token:  member.Token,
		Body:   `{"status":"resolved","body":"Bitti"}`,
	})
	if resp.Status != http.StatusForbidden {
		t.Fatalf("kapatma durumu = %d, beklenen 403 (gövde: %s)", resp.Status, resp.Body)
	}

	// Kesinti gerçekten AÇIK kalmalı.
	detail := h.Expect(h.Do(Request{
		Method: http.MethodGet,
		Path:   fmt.Sprintf("/v1/orgs/%s/incidents/%s", tn.org.ID, tn.incidentID),
		Token:  tn.owner.Token,
	}), http.StatusOK)

	var payload struct {
		Incident struct {
			Status     string  `json:"status"`
			ResolvedAt *string `json:"resolvedAt"`
		} `json:"incident"`
	}
	h.JSON(detail, &payload)

	if payload.Incident.ResolvedAt != nil {
		t.Errorf("403'e rağmen kesinti kapanmış: resolvedAt = %v", *payload.Incident.ResolvedAt)
	}
	if payload.Incident.Status == "resolved" {
		t.Error("403'e rağmen durum resolved olmuş")
	}
}
