package realtime_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/realtime"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustEvent(t *testing.T, eventType string, data any) realtime.Event {
	t.Helper()

	event, err := realtime.NewEvent(eventType, time.Unix(0, 0).UTC(), data)
	if err != nil {
		t.Fatalf("olay üretilemedi: %v", err)
	}

	return event
}

// receive, abonelikten tek bir olay bekler.
func receive(t *testing.T, sub *realtime.Subscription) realtime.Event {
	t.Helper()

	select {
	case event := <-sub.Events():
		return event
	case <-sub.Dropped():
		t.Fatalf("abonelik düşürüldü: %s", sub.DropReason())
	case <-time.After(time.Second):
		t.Fatal("olay beklenirken zaman aşımı")
	}

	return realtime.Event{}
}

func TestHubDeliversToChannelSubscribers(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	first := hub.Subscribe("org:a")
	second := hub.Subscribe("org:a")
	defer first.Close()
	defer second.Close()

	if got := hub.Publish("org:a", mustEvent(t, "test", nil)); got != 2 {
		t.Fatalf("delivered = %d, beklenen 2", got)
	}

	for i, sub := range []*realtime.Subscription{first, second} {
		if event := receive(t, sub); event.Type != "test" {
			t.Errorf("%d. abone olayı = %q, beklenen %q", i, event.Type, "test")
		}
	}
}

// 6.1'in ASIL GARANTİSİ: bir org'un olayı başka bir org'a sızmıyor.
func TestHubDoesNotLeakAcrossChannels(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	orgA, orgB := uuid.New(), uuid.New()

	subA := hub.Subscribe(realtime.OrgChannel(orgA))
	subB := hub.Subscribe(realtime.OrgChannel(orgB))
	defer subA.Close()
	defer subB.Close()

	hub.Publish(realtime.OrgChannel(orgA), mustEvent(t, "sadece-a", nil))

	if event := receive(t, subA); event.Type != "sadece-a" {
		t.Errorf("A'nın olayı = %q", event.Type)
	}

	select {
	case event := <-subB.Events():
		t.Fatalf("B başka org'un olayını aldı: %q", event.Type)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHubPublishToUnknownChannelIsNoop(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	if got := hub.Publish("org:yok", mustEvent(t, "test", nil)); got != 0 {
		t.Fatalf("delivered = %d, beklenen 0", got)
	}
}

// YAVAŞ İSTEMCİ: tamponu dolan abone BIRAKILIYOR ve yayıncı bloklanmıyor.
// Bloklasaydı, yazamayan tek bir tarayıcı sekmesi tüm org'un canlı akışını
// durdururdu.
func TestHubDropsSlowSubscriberWithoutBlocking(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	slow := hub.Subscribe("org:a")
	defer slow.Close()

	healthy := hub.Subscribe("org:b")
	defer healthy.Close()

	// Tamponu doldur (hiç okumadan) ve bir tane daha gönder.
	for i := range realtime.DefaultSubscriberBuffer + 1 {
		hub.Publish("org:a", mustEvent(t, "dolduran", map[string]int{"i": i}))
	}

	select {
	case <-slow.Dropped():
		if reason := slow.DropReason(); reason != realtime.DropSlow {
			t.Errorf("sebep = %q, beklenen %q", reason, realtime.DropSlow)
		}
	case <-time.After(time.Second):
		t.Fatal("yavaş abone bırakılmadı")
	}

	// Düşürülen abone kanaldan ÇIKARILMIŞ olmalı: aksi hâlde her yayında
	// yeniden "yavaş" diye işlenirdi.
	if got := hub.Subscribers("org:a"); got != 0 {
		t.Errorf("kanaldaki abone sayısı = %d, beklenen 0", got)
	}

	// Yayıncı bloklanmadıysa sağlıklı abone hâlâ olay alabiliyor.
	hub.Publish("org:b", mustEvent(t, "devam", nil))

	if event := receive(t, healthy); event.Type != "devam" {
		t.Errorf("sağlıklı abonenin olayı = %q", event.Type)
	}
}

func TestHubCloseRemovesSubscription(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	sub := hub.Subscribe("org:a")

	if got := hub.Subscribers("org:a"); got != 1 {
		t.Fatalf("abone sayısı = %d, beklenen 1", got)
	}

	sub.Close()

	if got := hub.Subscribers("org:a"); got != 0 {
		t.Fatalf("kapanıştan sonra abone sayısı = %d, beklenen 0", got)
	}

	// İkinci kapatma güvenli olmalı (bağlantı hem hata yolunda hem defer'da
	// kapatabiliyor).
	sub.Close()

	if got := hub.Publish("org:a", mustEvent(t, "test", nil)); got != 0 {
		t.Fatalf("kapanmış aboneye teslim edildi: %d", got)
	}
}

func TestHubShutdownDropsEveryone(t *testing.T) {
	t.Parallel()

	hub := realtime.NewHub(testLogger())

	first := hub.Subscribe("org:a")
	second := hub.Subscribe("org:b")

	hub.Shutdown()

	for i, sub := range []*realtime.Subscription{first, second} {
		select {
		case <-sub.Dropped():
			if reason := sub.DropReason(); reason != realtime.DropShutdown {
				t.Errorf("%d. abonenin sebebi = %q, beklenen %q", i, reason, realtime.DropShutdown)
			}
		case <-time.After(time.Second):
			t.Fatalf("%d. abone kapanmada bırakılmadı", i)
		}
	}

	// Kapanmış hub'a abone olmak nil DÖNMÜYOR, hemen düşürülmüş bir abonelik
	// dönüyor: çağıran tarafta unutulan bir nil kontrolü panik demekti.
	late := hub.Subscribe("org:c")
	select {
	case <-late.Dropped():
	case <-time.After(time.Second):
		t.Fatal("kapanmış hub'a abone olan bağlantı bırakılmadı")
	}
}

func TestNewEventEncodesData(t *testing.T) {
	t.Parallel()

	event := mustEvent(t, "test", map[string]string{"a": "b"})

	if string(event.Data) != `{"a":"b"}` {
		t.Errorf("Data = %s", event.Data)
	}

	empty := mustEvent(t, "test", nil)
	if empty.Data != nil {
		t.Errorf("gövdesiz olayda Data = %s, beklenen nil", empty.Data)
	}
}

func TestOrgChannelContainsID(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()

	if got, want := realtime.OrgChannel(orgID), "org:"+orgID.String(); got != want {
		t.Errorf("OrgChannel = %q, beklenen %q", got, want)
	}
}
