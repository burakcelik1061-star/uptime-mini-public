package realtime

import (
	"log/slog"
	"sync"
)

// Hub, açık WebSocket bağlantılarını kanallara göre tutar ve olayları dağıtır.
//
// NEDEN ELLE YAZILDI (kütüphane değil): burada gereken davranış kısa ama
// kritik kısmı YANLIŞ YAPMAMAK — bir org'un olayının başka bir org'a
// gitmemesi ve yazamayan tek bir istemcinin tüm yayını durdurmaması. Hazır bir
// pub/sub kütüphanesi bu iki soruyu kendi varsayılanlarıyla cevaplardı
// (çoğu "yavaş abone" için bloklar veya sessizce mesaj düşürür).
//
// Hub, WebSocket'i BİLMİYOR: elinde kanal adı ve olay var. Bağlantı ömrü,
// yazma zaman aşımı ve el sıkışma internal/httpapi'nin işi. Bu ayrım sayesinde
// hub gerçek bir soket açmadan test edilebiliyor.
type Hub struct {
	logger *slog.Logger

	mu       sync.Mutex
	channels map[string]map[*Subscription]struct{}
	closed   bool
}

// DefaultSubscriberBuffer, abone başına tampon boyu.
//
// Tampon neden var: olaylar patlamalı geliyor (bir kesinti aynı anda birçok
// monitor'ü etkileyebilir) ve ağa yazma her zaman birkaç milisaniye sürüyor.
// Tampon neden KÜÇÜK: bu bir kuyruk değil, "istemci an itibarıyla ne kadar
// geride kalabilir" sınırı. Büyük bir tampon, yayını takip edemeyen bir
// istemciyi dakikalarca eski veriyle beslerdi — canlı yayın olmaktan çıkardı.
const DefaultSubscriberBuffer = 32

// DropReason, bir aboneliğin neden düşürüldüğünü söyler.
type DropReason string

const (
	// DropSlow: abone tamponu doldu, yani istemci yayına yetişemiyor.
	DropSlow DropReason = "slow"
	// DropShutdown: api kapanıyor.
	DropShutdown DropReason = "shutdown"
)

// Subscription, tek bir bağlantının tek bir kanala aboneliği.
type Subscription struct {
	hub     *Hub
	channel string
	events  chan Event

	// dropped, aboneliği HUB sonlandırdığında kapanır (yavaş istemci veya
	// kapanma). `events` kanalını kapatmak yerine ayrı bir sinyal kullanılıyor:
	// kapalı bir kanala yapılan gönderim panikler, yani yayıncı ile okuyucu
	// arasında yarış doğardı.
	dropped   chan struct{}
	dropOnce  sync.Once
	reasonMu  sync.Mutex
	reasonVal DropReason
}

// NewHub, boş bir hub kurar.
func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}

	return &Hub{
		logger:   logger,
		channels: make(map[string]map[*Subscription]struct{}),
	}
}

// Subscribe, verilen kanala abone olur.
//
// Hub kapanmışsa DA bir abonelik dönüyor — ama hemen düşürülmüş olarak. nil
// dönmek her çağıranın nil kontrolü yazmasını gerektirirdi ve unutulan tek bir
// kontrol kapanma anında panik demekti.
func (h *Hub) Subscribe(channel string) *Subscription {
	sub := &Subscription{
		hub:     h,
		channel: channel,
		events:  make(chan Event, DefaultSubscriberBuffer),
		dropped: make(chan struct{}),
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.closed {
		sub.dropLocked(DropShutdown)

		return sub
	}

	subs, ok := h.channels[channel]
	if !ok {
		subs = make(map[*Subscription]struct{})
		h.channels[channel] = subs
	}

	subs[sub] = struct{}{}

	return sub
}

// Publish, bir kanaldaki tüm abonelere olayı dağıtır ve kaç aboneye
// ulaştığını döner.
//
// BLOKLAMIYOR: gönderim `select`/`default` ile deneniyor. Tamponu dolu olan
// abone BIRAKILIYOR (bağlantısı kapatılacak), çünkü alternatifi tek bir yavaş
// istemcinin yayıncıyı — yani tüm org'un canlı akışını — durdurması olurdu.
// Bırakılan istemci yeniden bağlanıp güncel durumu tazeliyor (6.2).
func (h *Hub) Publish(channel string, event Event) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	var (
		delivered int
		slow      []*Subscription
	)

	for sub := range h.channels[channel] {
		select {
		case sub.events <- event:
			delivered++
		default:
			slow = append(slow, sub)
		}
	}

	for _, sub := range slow {
		h.removeLocked(sub)
		sub.dropLocked(DropSlow)
	}

	if len(slow) > 0 {
		// Sessiz kalmamalı: bu satır ya istemci gerçekten yavaş demek ya da
		// yayın hacmi tasarımın öngördüğünün üstüne çıktı demek.
		h.logger.Warn("yavaş istemci bırakıldı",
			slog.String("channel", channel),
			slog.String("event", event.Type),
			slog.Int("dropped", len(slow)))
	}

	return delivered
}

// Shutdown, tüm abonelikleri düşürür. Bağlantı sahipleri bunu görüp soketi
// düzgün bir kapanış çerçevesiyle kapatıyor — süreç öldüğünde istemcinin
// yarım kalmış bir TCP bağlantısıyla baş başa kalması yerine.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true

	for channel, subs := range h.channels {
		for sub := range subs {
			sub.dropLocked(DropShutdown)
		}

		delete(h.channels, channel)
	}
}

// Subscribers, bir kanaldaki abone sayısını döner (log ve testler için).
func (h *Hub) Subscribers(channel string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.channels[channel])
}

// removeLocked, aboneliği kanal haritasından çıkarır (h.mu tutuluyorken).
func (h *Hub) removeLocked(sub *Subscription) {
	subs, ok := h.channels[sub.channel]
	if !ok {
		return
	}

	delete(subs, sub)

	// Boşalan kanal haritada tutulmuyor: aksi hâlde gelip geçen her org için
	// bir giriş sonsuza kadar kalırdı.
	if len(subs) == 0 {
		delete(h.channels, sub.channel)
	}
}

// Events, olay akışı. Kanal HİÇBİR ZAMAN kapatılmıyor; aboneliğin bittiğini
// Dropped() söylüyor.
func (s *Subscription) Events() <-chan Event { return s.events }

// Dropped, hub aboneliği sonlandırdığında kapanan sinyal.
func (s *Subscription) Dropped() <-chan struct{} { return s.dropped }

// DropReason, aboneliğin neden düşürüldüğünü söyler (Dropped kapandıktan
// sonra anlamlı).
func (s *Subscription) DropReason() DropReason {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()

	return s.reasonVal
}

// Close, aboneliği sonlandırır. Bağlantı sahibi çıkarken çağırıyor; iki kez
// çağrılması güvenli.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()

	s.hub.removeLocked(s)
}

// dropLocked, aboneliği düşürür (hub.mu tutuluyorken çağrılır).
func (s *Subscription) dropLocked(reason DropReason) {
	s.dropOnce.Do(func() {
		s.reasonMu.Lock()
		s.reasonVal = reason
		s.reasonMu.Unlock()

		close(s.dropped)
	})
}
