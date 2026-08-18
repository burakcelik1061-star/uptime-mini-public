package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeNotificationService, bildirim uçlarının bellek içi karşılığı.
//
// DOĞRULAMA GERÇEK: `notifications.Normalize` + `Validate` çağrılıyor, yani
// handler'ın 422 çevirimi ölçülüyor; kuralların kendisi internal/notifications
// testlerinde (diğer sahte servislerle aynı desen).
type fakeNotificationService struct {
	byOrg map[uuid.UUID][]store.NotificationChannel
	// tested, test bildirimi gönderilen kanalları kaydeder.
	tested []uuid.UUID
}

func newFakeNotificationService() *fakeNotificationService {
	return &fakeNotificationService{byOrg: map[uuid.UUID][]store.NotificationChannel{}}
}

func (f *fakeNotificationService) List(
	_ context.Context, orgID uuid.UUID,
) ([]notifications.ChannelWithStats, error) {
	result := make([]notifications.ChannelWithStats, 0, len(f.byOrg[orgID]))
	for _, channel := range f.byOrg[orgID] {
		result = append(result, notifications.ChannelWithStats{Channel: channel})
	}

	return result, nil
}

func (f *fakeNotificationService) Create(
	_ context.Context, orgID uuid.UUID, in notifications.ChannelInput,
) (store.NotificationChannel, error) {
	in = notifications.Normalize(in)
	if err := notifications.Validate(in); err != nil {
		return store.NotificationChannel{}, err
	}

	for _, existing := range f.byOrg[orgID] {
		if existing.Kind == in.Kind && existing.Target == in.Target {
			return store.NotificationChannel{}, notifications.ErrDuplicate
		}
	}

	channel := store.NotificationChannel{
		ID:        uuid.New(),
		OrgID:     orgID,
		Kind:      in.Kind,
		Name:      in.Name,
		Target:    in.Target,
		IsActive:  true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	f.byOrg[orgID] = append(f.byOrg[orgID], channel)

	return channel, nil
}

func (f *fakeNotificationService) Delete(_ context.Context, orgID, channelID uuid.UUID) error {
	for i, channel := range f.byOrg[orgID] {
		if channel.ID == channelID {
			f.byOrg[orgID] = append(f.byOrg[orgID][:i], f.byOrg[orgID][i+1:]...)

			return nil
		}
	}

	return notifications.ErrNotFound
}

func (f *fakeNotificationService) SendTest(_ context.Context, orgID, channelID uuid.UUID, _ string) error {
	for _, channel := range f.byOrg[orgID] {
		if channel.ID == channelID {
			f.tested = append(f.tested, channelID)

			return nil
		}
	}

	return notifications.ErrNotFound
}

func (f orgScopeFixture) notificationChannels() (*fakeNotificationService, bool) {
	svc, ok := f.deps.Notifications.(*fakeNotificationService)

	return svc, ok
}

func createChannel(t *testing.T, fixture orgScopeFixture, body string) notificationChannelResponse {
	t.Helper()

	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/notification-channels", body, bearer(fixture.ownerTok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, beklenen 201 (%s)", rec.Code, rec.Body.String())
	}

	var wrapper struct {
		Channel notificationChannelResponse `json:"channel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("gövde çözümlenemedi: %v", err)
	}

	return wrapper.Channel
}

func TestCreateNotificationChannel(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	channel := createChannel(t, fixture,
		`{"kind":"email","name":"Nöbetçi","target":"Nobetci@Example.com"}`)

	// Normalleştirme servise ait ve cevapta görünüyor.
	if channel.Target != "nobetci@example.com" {
		t.Errorf("hedef = %q", channel.Target)
	}

	rec := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String()+"/notification-channels", "", bearer(fixture.ownerTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("liste status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateNotificationChannelValidates(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	cases := map[string]string{
		"biçimsiz e-posta": `{"kind":"email","name":"Ad","target":"adres-degil"}`,
		"http webhook":     `{"kind":"webhook","name":"Ad","target":"http://ornek.example/h"}`,
		"bilinmeyen tür":   `{"kind":"sms","name":"Ad","target":"+905551112233"}`,
		"ad boş":           `{"kind":"email","name":"","target":"a@b.com"}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := do(t, fixture.router, http.MethodPost,
				"/v1/orgs/"+fixture.orgID.String()+"/notification-channels", body, bearer(fixture.ownerTok))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, beklenen 422 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// Aynı hedefi iki kez eklemek her kesintide iki bildirim demek olurdu.
func TestCreateNotificationChannelRejectsDuplicate(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	body := `{"kind":"email","name":"Nöbetçi","target":"nobetci@example.com"}`
	createChannel(t, fixture, body)

	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/notification-channels", body, bearer(fixture.ownerTok))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, beklenen 409 (%s)", rec.Code, rec.Body.String())
	}

	if code := decodeErrorResponse(t, rec).Error.Code; code != codeChannelExists {
		t.Errorf("kod = %q", code)
	}
}

func TestNotificationChannelTestEndpoint(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	channel := createChannel(t, fixture,
		`{"kind":"email","name":"Nöbetçi","target":"nobetci@example.com"}`)

	rec := do(t, fixture.router, http.MethodPost,
		"/v1/orgs/"+fixture.orgID.String()+"/notification-channels/"+channel.ID+"/test",
		"", bearer(fixture.ownerTok))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, beklenen 204 (%s)", rec.Code, rec.Body.String())
	}

	svc, ok := fixture.notificationChannels()
	if !ok {
		t.Fatal("test bağımlılıkları beklenen tipte değil")
	}

	if len(svc.tested) != 1 {
		t.Fatalf("test bildirimi sayısı = %d", len(svc.tested))
	}
}

func TestNotificationChannelNotFound(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	for _, path := range []string{
		"/notification-channels/" + uuid.NewString(),
		"/notification-channels/bu-bir-uuid-degil",
	} {
		rec := do(t, fixture.router, http.MethodDelete,
			"/v1/orgs/"+fixture.orgID.String()+path, "", bearer(fixture.ownerTok))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, beklenen 404 (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

// YETKİ İKİ KADEMELİ (0.1 §2): okuma üyeye açık, yazma admin+.
// Bir kanal eklemek organizasyonun kesinti bilgisini dışarıya akıtmak demek.
func TestNotificationChannelWritesRequireAdmin(t *testing.T) {
	fixture := newOrgScopeFixture(t)

	channel := createChannel(t, fixture,
		`{"kind":"email","name":"Nöbetçi","target":"nobetci@example.com"}`)

	memberTok := fixture.memberToken(t)

	// Okuma: üye görebiliyor.
	rec := do(t, fixture.router, http.MethodGet,
		"/v1/orgs/"+fixture.orgID.String()+"/notification-channels", "", bearer(memberTok))
	if rec.Code != http.StatusOK {
		t.Fatalf("üye okuma status = %d, beklenen 200 (%s)", rec.Code, rec.Body.String())
	}

	// Yazma uçlarının hepsi 403.
	writes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/notification-channels", `{"kind":"email","name":"Y","target":"y@example.com"}`},
		{http.MethodDelete, "/notification-channels/" + channel.ID, ""},
		{http.MethodPost, "/notification-channels/" + channel.ID + "/test", ""},
	}

	for _, write := range writes {
		rec := do(t, fixture.router, write.method,
			"/v1/orgs/"+fixture.orgID.String()+write.path, write.body, bearer(memberTok))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, beklenen 403 (%s)",
				write.method, write.path, rec.Code, rec.Body.String())
		}
	}
}
