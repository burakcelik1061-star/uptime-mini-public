package config

import (
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// dotEnvSearchDepth, `.env` aranırken kaç üst dizine bakılacağı.
// `go run ./cmd/api` çalışma dizinini `backend/` yapıyor, dosya ise repo
// kökünde; iki seviye fazlasıyla yeterli.
const dotEnvSearchDepth = 3

// LoadDotEnv, geliştirme kolaylığı için repo kökündeki `.env` dosyasını
// ortama yükler. Dosya yoksa sessizce geçer.
//
// ÖNEMLİ: zaten TANIMLI olan değişkenlerin üzerine YAZMAZ (godotenv'in
// varsayılan davranışı). Yani gerçek ortam değişkenleri her zaman kazanır —
// Docker Compose ve CI kendi değerlerini geçtiğinde `.env` onları
// gölgeleyemez. Bu sıralama bilinçli: dosya en zayıf kaynak.
//
// Süreçlerin main'inden ÇAĞRILIR, config.Load* içinden değil — testler
// çalışırken çalışma dizinine bağlı bir yan etki oluşmasın diye.
func LoadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}

	for i := 0; i <= dotEnvSearchDepth; i++ {
		candidate := filepath.Join(dir, ".env")
		if _, err := os.Stat(candidate); err == nil {
			_ = godotenv.Load(candidate)
			return
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}
