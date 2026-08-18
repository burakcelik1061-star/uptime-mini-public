-- +goose Up
-- Refresh token'lar (Faz 1.2). Access token kısa ömürlü ve DOĞRULANIRKEN
-- veritabanına bakılmıyor; uzun oturum ve İPTAL edilebilirlik bu tablodan
-- geliyor.
--
-- Tasarımın üç dayanağı:
--   1. Token'ın kendisi DEĞİL, HMAC'i saklanıyor (aşağıya bkz.).
--   2. Her kullanımda rotasyon: kullanılan satır `used_at` ile işaretleniyor,
--      yerine yenisi yazılıyor.
--   3. Aynı `session_id` bir oturum AİLESİ: kullanılmış bir token tekrar
--      sunulduğunda (çalınmış olabilir) ailenin tamamı iptal ediliyor.
CREATE TABLE refresh_tokens (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Rotasyon zincirinin kimliği. Yeni token AYNI session_id ile yazılıyor;
    -- böylece "bu oturumu kapat" tek bir UPDATE.
    session_id uuid NOT NULL,

    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- Ham token DEĞİL, anahtarlı özeti (HMAC-SHA256). Veritabanı dökümü ele
    -- geçse bile satırlardan kullanılabilir token üretilemiyor: özet tek
    -- yönlü ve anahtar (REFRESH_TOKEN_SECRET) veritabanında değil.
    --
    -- Parolalardaki gibi Argon2 KULLANILMIYOR: token 32 byte'lık rastgele
    -- bir değer, sözlük saldırısına konu olacak düşük entropili bir sır
    -- değil. Yavaş bir özet burada güvenlik katmazdı, her istek yenilemeyi
    -- pahalılaştırırdı.
    token_hash bytea NOT NULL UNIQUE,

    issued_at  timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,

    -- Rotasyon: bir kez kullanıldığında dolar. İkinci kez sunulması
    -- "yeniden kullanım" (reuse) demektir.
    used_at    timestamptz,

    -- Logout veya reuse tespiti.
    revoked_at timestamptz
);

-- Aile iptali (reuse/logout) bu indeksle çalışıyor.
CREATE INDEX refresh_tokens_session_id_idx ON refresh_tokens (session_id);

-- `users` satırı silindiğinde Postgres bu tabloyu tarar; indeks olmadan
-- cascade silme tam tarama yapardı.
CREATE INDEX refresh_tokens_user_id_idx ON refresh_tokens (user_id);

-- Süresi geçmiş satırların budanması için (temizlik işi Faz 3'te worker'a
-- verilecek).
CREATE INDEX refresh_tokens_expires_at_idx ON refresh_tokens (expires_at);

-- +goose Down
DROP TABLE IF EXISTS refresh_tokens;
