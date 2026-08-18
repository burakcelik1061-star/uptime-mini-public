package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger, her isteği tek bir yapılandırılmış satırla loglar:
// metot, yol, durum kodu, süre ve istek kimliği.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			// WrapResponseWriter olmadan yazılan status kodunu göremeyiz —
			// http.ResponseWriter onu geri vermiyor.
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			logger.Info("http isteği",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				// Ham TCP adresi — proxy başlıklarına GÜVENİLMİYOR
				// (bkz. router.go'daki RealIP notu).
				slog.String("remote_addr", r.RemoteAddr),
				slog.Int("status", ww.Status()),
				slog.Duration("duration", time.Since(started)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
