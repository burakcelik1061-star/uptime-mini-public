// Package logging, iki sürecin de kullandığı yapılandırılmış log kurulumunu
// içerir.
//
// Neden slog: standart kütüphanede (Go 1.21+), yapılandırılmış (JSON) çıktı
// veriyor ve üçüncü parti bir log kütüphanesi eklemeye gerek bırakmıyor.
// Faz 7.4'ün "yapılandırılmış loglama" gereksinimi bu yüzden baştan
// karşılanmış oluyor.
package logging

import (
	"log/slog"
	"os"
)

// New, ortama uygun bir logger üretir.
//
// development'ta insan gözüyle okunabilir metin, diğer ortamlarda JSON:
// log toplayıcılar JSON ister, geliştirici terminali istemez.
func New(env string, service string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}

	var handler slog.Handler
	if env == "development" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Her satırda hangi sürecin konuştuğu yazsın: api ve worker aynı
	// terminale/log toplayıcıya yazıyor.
	return slog.New(handler).With(slog.String("service", service))
}
