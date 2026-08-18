-- +goose Up
-- PAROLA SIFIRLAMA (Faz 8.4).
--
-- NEDEN VAR: 1.1 parola sıfırlamayı "dış e-posta servisi yok" gerekçesiyle
-- MVP dışına almıştı (README'nin "hesap tarafı" sınırlarında da yazılı).
-- Sonucu somut: parolasını unutan kullanıcı KALICI OLARAK kilitliydi —
-- hesabını geri almanın hiçbir yolu yoktu.
CREATE TABLE password_reset_tokens (
    id      uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- TOKEN HAM SAKLANMIYOR (1.2'nin refresh token, 6.4'ün onay ve 8.3'ün
    -- davet token'ı kararlarının aynısı): veritabanı dökümünü ele geçiren
    -- biri, bekleyen sıfırlama isteklerini kullanma yeteneğini HAZIR
    -- bulmasın. Ham değer yalnızca e-postadaki linkte var.
    --
    -- Bu kararın bir sonucu var ve 8.4'ün tasarımını belirliyor: worker
    -- hash'ten linki ÜRETEMEZ, yani e-postayı 6.3'ün gönderim job'ına
    -- devretmek mümkün değil. Gönderim api sürecinde kalmak zorunda
    -- (bkz. internal/passwordreset).
    token_hash text NOT NULL UNIQUE,

    -- KISA ÖMÜRLÜ: bu token tek başına HESAP DEVRALMA yetkisi veriyor —
    -- davetten (8.3, 7 gün) çok daha dar olmalı. Bir saat, e-postayı görüp
    -- yeni parola yazmak için yeterli.
    expires_at timestamptz NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Token ile arama: UNIQUE kısıtının indeksi zaten karşılıyor.
-- Kullanıcı bazında silme (başarılı sıfırlamada TÜM token'lar düşüyor) ve
-- `users` cascade'i bu indeksten geçiyor.
CREATE INDEX password_reset_tokens_user_id_idx ON password_reset_tokens (user_id);

-- TEK KULLANIMLIK "used_at" İLE DEĞİL SİLEREK: başarılı bir sıfırlamada
-- kullanıcının TÜM sıfırlama token'ları siliniyor (yalnızca kullanılan
-- değil). Sebep somut — saldırgan da bir sıfırlama isteyip token'ını
-- beklemede tutabilir; meşru kullanıcı parolasını değiştirdiğinde o token
-- da ölmeli. "Kullanıldı" bayrağı, ölmesi gereken satırları veritabanında
-- tutmaktan başka bir şey kazandırmazdı.

-- +goose Down
DROP TABLE IF EXISTS password_reset_tokens;
