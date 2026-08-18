-- Bu dosyadaki her sorgu, sqlc tarafından tipli bir Go fonksiyonuna
-- dönüştürülür (bkz. backend/sqlc.yaml). Sorgu ile şema uyuşmazsa hata
-- çalışma zamanında değil `sqlc generate` sırasında çıkar.
--
-- `SELECT *` yerine sütunlar TEK TEK yazılıyor: yeni bir sütun eklendiğinde
-- üretilen struct'ın sessizce büyümesini değil, bu dosyanın bilinçli olarak
-- güncellenmesini istiyoruz.

-- name: CreateUser :one
INSERT INTO users (email, password_hash, name)
VALUES ($1, $2, $3)
RETURNING id, email, password_hash, name, created_at;

-- name: GetUserByEmail :one
-- Email uygulama tarafında normalleştirilmiş (küçük harf + kırpılmış) olarak
-- saklanıyor; çağıran taraf aynı normalleştirmeyi uygulamak zorunda.
SELECT id, email, password_hash, name, created_at
FROM users
WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, password_hash, name, created_at
FROM users
WHERE id = $1;
