package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// uniqueViolation, Postgres'in "unique constraint ihlali" SQLSTATE kodu.
const uniqueViolation = "23505"

// IsUniqueViolation, hatanın belirli bir unique kısıtın ihlali olup
// olmadığını söyler.
//
// Neden "önce SELECT ile bak, yoksa INSERT et" yapılmıyor: iki istek aynı
// anda geldiğinde ikisi de "yok" görüp ikisi de INSERT eder — yarış açık.
// Tek doğru hakem veritabanının kısıtı; kod da kısıtın verdiği hatayı
// çeviriyor.
//
// constraint boş verilirse kısıt adına bakılmaz.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return false
	}

	return constraint == "" || pgErr.ConstraintName == constraint
}
