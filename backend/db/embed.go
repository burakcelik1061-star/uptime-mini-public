// Package db, migration dosyalarını ikili dosyanın İÇİNE gömer.
//
// Neden gömülü: `cmd/migrate` binary'si tek başına çalışabilsin — Docker
// imajına ayrıca SQL dosyası kopyalamak, imaj ile migration'ların sürümünün
// ayrışması riskini doğurur. Gömülü olunca "hangi migration'lar bu binary'de
// var" sorusunun tek cevabı var.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS

// MigrationsDir, gömülü dosya sistemindeki dizin adı.
const MigrationsDir = "migrations"
