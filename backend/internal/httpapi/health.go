package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Pinger, hazır olma kontrolünün ihtiyaç duyduğu tek davranış.
// Arayüz olması testlerin gerçek bir veritabanı olmadan da her iki yolu
// (sağlıklı/sağlıksız) ölçebilmesi için.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthResponse, /health ucunun gövdesi.
type HealthResponse struct {
	Status        string `json:"status"`
	Service       string `json:"service"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

// ReadyResponse, /ready ucunun gövdesi.
type ReadyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

const readyCheckTimeout = 2 * time.Second

// healthHandler — LIVENESS: süreç ayakta mı?
//
// Bilinçli olarak veritabanına BAKMAZ. Veritabanı düştüğünde de 200 dönmeli;
// yoksa orkestratör, aslında sağlıklı olan ama bağımlılığı düşmüş bir süreci
// gereksiz yere yeniden başlatır (yeniden başlatmak veritabanını geri
// getirmez, sadece kesintiyi uzatır).
func healthHandler(startedAt time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, HealthResponse{
			Status:        "ok",
			Service:       "api",
			UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		})
	}
}

// readyHandler — READINESS: istek karşılayabilir durumda mı?
//
// Veritabanını gerçekten yokluyor. Başarısızsa 503 döner: yük dengeleyici bu
// kopyaya trafik göndermeyi bırakır ama süreç öldürülmez.
func readyHandler(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyCheckTimeout)
		defer cancel()

		if err := db.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, ReadyResponse{
				Status: "degraded",
				Checks: map[string]string{"database": "unreachable"},
			})
			return
		}

		writeJSON(w, http.StatusOK, ReadyResponse{
			Status: "ready",
			Checks: map[string]string{"database": "ok"},
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	// Kodlama hatası burada loglanamaz (logger'ı yok) ve gövde zaten
	// yazılmaya başlandı; Faz 7.4'te merkezi hata yönetimi gelince bu
	// yeniden ele alınacak.
	_ = json.NewEncoder(w).Encode(body)
}
