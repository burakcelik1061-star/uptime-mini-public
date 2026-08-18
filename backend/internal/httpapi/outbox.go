package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/burakcelik1061-star/uptime-mini/backend/internal/notifications"
)

// GELİŞTİRİCİ POSTA KUTUSU (Faz 8.2) — ürün yüzeyi DEĞİL, geliştirme aracı.
//
// KAPI ROUTER'DA, HANDLER'DA DEĞİL. Uç yalnızca APP_ENV açıkça
// development|test iken kaydediliyor (bkz. router.go + config.devToolsEnabled);
// üretimde ROTA HİÇ VAR OLMUYOR ve istek standart 404 zarfını alıyor. Handler
// içinde bir `if isProduction` kontrolü de yazılabilirdi ama o, korumayı
// "kontrolü unutmak mümkün" sınıfına sokardı — 1.4'ün kararı burada da
// geçerli: kontrolü atlamak için ağacın DIŞINA çıkmak gerekmeli.
//
// ORG-SCOPED DEĞİL, sebebi 00018'de yazılı: buradaki mesajların hepsi bir
// organizasyona ait değil (parola sıfırlama org'dan önce geliyor, abonelik
// onayı bir ziyaretçiye ait). Yine de OTURUM İSTİYOR: pano ekranı ve
// oturumsuz bırakmak için hiçbir sebep yok.

// BAĞIMLILIK ARAYÜZ DEĞİL SOMUT TİP (*notifications.Outbox) ve bunun iki
// sebebi var:
//
//  1. GO'NUN TİPLİ-NIL TUZAĞI. Kapı "bağımlılık nil mi" sorusuna dayanıyor;
//     nil bir `*Outbox` bir ARAYÜZ alanına yazıldığında arayüz nil OLMUYOR,
//     yani `deps.Outbox != nil` üretimde de doğru çıkar ve posta kutusu
//     sessizce açılırdı. Somut işaretçide böyle bir boşluk yok.
//  2. Bu adımın şartı: mock'un etrafına İKİNCİ bir soyutlama katmanı
//     yazılmıyor. `Deps.Realtime *realtime.Hub` ve `Deps.Tickets
//     *auth.TicketService` de aynı biçimde somut.
type outboxHandler struct {
	outbox *notifications.Outbox
	logger *slog.Logger
}

// outboxMessageResponse, saklanmış bir gönderimin dışa verilen gösterimi.
type outboxMessageResponse struct {
	ID        string    `json:"id"`
	Recipient string    `json:"recipient"`
	Subject   string    `json:"subject"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// list — GET /v1/dev/outbox
func (h *outboxHandler) list(w http.ResponseWriter, r *http.Request) {
	rows, err := h.outbox.List(r.Context())
	if err != nil {
		writeInternalError(w, r, h.logger, "posta kutusu okunamadı", err)
		return
	}

	messages := make([]outboxMessageResponse, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, outboxMessageResponse{
			ID:        row.ID.String(),
			Recipient: row.Recipient,
			Subject:   row.Subject,
			Body:      row.Body,
			CreatedAt: row.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string][]outboxMessageResponse{"messages": messages})
}
