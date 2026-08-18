package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
	"github.com/burakcelik1061-star/uptime-mini/backend/internal/store"
)

// fakeNotifyStore, `pg_notify` çağrılarını kaydeder (taşımanın kendisi
// transport_test.go'da gerçek veritabanına karşı ölçülüyor).
type fakeNotifyStore struct {
	payloads []string
	failWith error
}

func (f *fakeNotifyStore) PublishRealtimeEvent(_ context.Context, arg store.PublishRealtimeEventParams) error {
	if f.failWith != nil {
		return f.failWith
	}

	f.payloads = append(f.payloads, arg.Payload)

	return nil
}

// fakeResolver, public kanal kapısının sahtesi (gerçeği internal/statuspage,
// 5.1'in kendi sorgularıyla).
type fakeResolver struct {
	slug     string
	ok       bool
	failWith error
	// seen, kapıya sorulan monitor listesini kaydeder.
	seen []uuid.UUID
}

func (f *fakeResolver) PublicSlugFor(
	_ context.Context, _ uuid.UUID, monitorIDs []uuid.UUID,
) (string, bool, error) {
	f.seen = append(f.seen, monitorIDs...)

	if f.failWith != nil {
		return "", false, f.failWith
	}

	return f.slug, f.ok, nil
}

// channelsIn, kaydedilen payload'lardaki hedef kanalları döner.
func channelsIn(t *testing.T, payloads []string) []string {
	t.Helper()

	channels := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		channels = append(channels, channelOf(t, payload))
	}

	return channels
}

func TestBroadcastReachesOrgAndPublicChannel(t *testing.T) {
	t.Parallel()

	notifyStore := &fakeNotifyStore{}
	resolver := &fakeResolver{slug: "acme", ok: true}

	orgID, monitorID := uuid.New(), uuid.New()

	realtime.NewBroadcaster(realtime.NewNotifier(notifyStore, testLogger()), resolver, testLogger()).
		Changed(t.Context(), orgID, []uuid.UUID{monitorID}, realtime.ReasonMonitorStatus)

	got := channelsIn(t, notifyStore.payloads)
	want := []string{realtime.OrgChannel(orgID), realtime.PublicChannel("acme")}

	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("kanallar = %v, beklenen %v", got, want)
	}

	// Kapıya SORULAN liste, yayının dayandığı liste olmalı.
	if len(resolver.seen) != 1 || resolver.seen[0] != monitorID {
		t.Errorf("kapıya sorulan monitor listesi = %v", resolver.seen)
	}
}

// ASIL GARANTİ: kapı hayır dediğinde public kanala HİÇBİR ŞEY çıkmıyor —
// gövdesiz bir sinyal bile. Org kanalı ise etkilenmiyor.
func TestBroadcastSkipsPublicChannelWhenNotPublished(t *testing.T) {
	t.Parallel()

	notifyStore := &fakeNotifyStore{}
	orgID := uuid.New()

	realtime.NewBroadcaster(realtime.NewNotifier(notifyStore, testLogger()), &fakeResolver{}, testLogger()).
		Changed(t.Context(), orgID, []uuid.UUID{uuid.New()}, realtime.ReasonMonitorStatus)

	got := channelsIn(t, notifyStore.payloads)
	if len(got) != 1 || got[0] != realtime.OrgChannel(orgID) {
		t.Fatalf("kanallar = %v, beklenen yalnızca org kanalı", got)
	}
}

// Kapı HATA verdiğinde de public kanala çıkılmıyor: "bilmiyorum" ile "evet"
// arasındaki farkı yayın lehine yorumlamak, sızıntının kendisi olurdu.
func TestBroadcastSkipsPublicChannelWhenGateFails(t *testing.T) {
	t.Parallel()

	notifyStore := &fakeNotifyStore{}
	orgID := uuid.New()
	resolver := &fakeResolver{slug: "acme", ok: true, failWith: errors.New("veritabanı yok")}

	realtime.NewBroadcaster(realtime.NewNotifier(notifyStore, testLogger()), resolver, testLogger()).
		Changed(t.Context(), orgID, []uuid.UUID{uuid.New()}, realtime.ReasonIncidentOpened)

	got := channelsIn(t, notifyStore.payloads)
	if len(got) != 1 || got[0] != realtime.OrgChannel(orgID) {
		t.Fatalf("kanallar = %v, beklenen yalnızca org kanalı", got)
	}
}

// Yayın hatası çağıranı ETKİLEMİYOR (imzada hata yok): bir kesinti kaydı
// yazıldıysa yazılmıştır, duyurusunun kaybolması onu geri almaz.
func TestBroadcastSwallowsPublishErrors(t *testing.T) {
	t.Parallel()

	notifyStore := &fakeNotifyStore{failWith: errors.New("bağlantı yok")}

	realtime.NewBroadcaster(realtime.NewNotifier(notifyStore, testLogger()), &fakeResolver{}, testLogger()).
		Changed(t.Context(), uuid.New(), []uuid.UUID{uuid.New()}, realtime.ReasonMonitorStatus)
}

// OLAY GÖVDESİZ: taşınan tek şey "değişti" sinyali ve tanı amaçlı sebep.
// Durumun kendisi taşınsaydı, panonun okuduğu cevabın ikinci bir kopyası
// doğardı (bkz. broadcast.go).
func TestBroadcastEventCarriesNoDomainState(t *testing.T) {
	t.Parallel()

	notifyStore := &fakeNotifyStore{}

	realtime.NewBroadcaster(realtime.NewNotifier(notifyStore, testLogger()), &fakeResolver{}, testLogger()).
		Changed(t.Context(), uuid.New(), []uuid.UUID{uuid.New()}, realtime.ReasonIncidentOpened)

	if len(notifyStore.payloads) != 1 {
		t.Fatalf("payload sayısı = %d", len(notifyStore.payloads))
	}

	event := eventOf(t, notifyStore.payloads[0])

	if event.Type != realtime.EventStatusChanged {
		t.Errorf("olay türü = %q, beklenen %q", event.Type, realtime.EventStatusChanged)
	}

	if got := string(event.Data); got != `{"reason":"`+realtime.ReasonIncidentOpened+`"}` {
		t.Errorf("gövde = %s — sebep dışında bir alan taşınıyor", got)
	}
}

// envelopeOf, `pg_notify` payload'ının dışa görünen biçimi. Zarf tipi paket
// içinde kapalı; test onu SÖZLEŞME olarak, dışarıdan çözerek okuyor —
// dinleyicinin yaptığı da tam olarak bu.
type envelopeOf struct {
	Channel string         `json:"channel"`
	Event   realtime.Event `json:"event"`
}

func decodeEnvelope(t *testing.T, payload string) envelopeOf {
	t.Helper()

	var env envelopeOf
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		t.Fatalf("zarf çözümlenemedi: %v (%s)", err, payload)
	}

	return env
}

func channelOf(t *testing.T, payload string) string {
	t.Helper()

	return decodeEnvelope(t, payload).Channel
}

func eventOf(t *testing.T, payload string) realtime.Event {
	t.Helper()

	return decodeEnvelope(t, payload).Event
}
