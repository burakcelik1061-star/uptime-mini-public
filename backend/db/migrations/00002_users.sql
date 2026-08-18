-- +goose Up
-- İlk tablo: kullanıcı hesabı (bkz. docs/0.1-scope-and-data-model.md §4).
--
-- `users` (çoğul), `user` DEĞİL: `user` SQL'de ayrılmış bir sözcük ve
-- Postgres'te her kullanımda çift tırnak ister ("user"). Tırnaklı tablo adı
-- her sorguyu ve her hata mesajını kirletirdi; şema genelinde çoğul ad
-- kullanılıyor.
--
-- `SUBSCRIBER` bu tabloya GİRMEZ (0.1 §5): status page abonesinin şifresi ve
-- oturumu yok, kimliği tahmin edilemez token'larla doğrulanıyor.
CREATE TABLE users (
    -- gen_random_uuid() Postgres 13'ten beri çekirdekte; pgcrypto eklentisine
    -- gerek yok.
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- Email uygulama tarafında küçük harfe çevrilip saklanıyor, bu yüzden
    -- UNIQUE kısıtı doğrudan sütunun üzerinde (ayrıca lower(email) ifade
    -- indeksine gerek yok) ve giriş sorgusunun ihtiyaç duyduğu indeks de
    -- bedava geliyor.
    --
    -- Bilinçli olarak `CHECK (email = lower(email))` EKLENMEDİ: Go'nun
    -- strings.ToLower'ı ile Postgres'in lower()'ı her Unicode kod noktasında
    -- birebir aynı davranmak zorunda değil. Ayrışan tek bir karakter, geçerli
    -- bir kaydı 500'e çevirirdi — normalleştirme tek yerde (uygulama), veri
    -- bütünlüğünün son hakemi UNIQUE kısıtı.
    email         text NOT NULL UNIQUE CHECK (char_length(email) BETWEEN 3 AND 254),

    -- Argon2id PHC dizesi: $argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>
    -- Parametreler hash'in İÇİNDE; maliyet ileride artırıldığında eski
    -- kayıtlar doğrulanmaya devam ediyor (bkz. internal/auth/password.go).
    password_hash text NOT NULL,

    name          text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),

    created_at    timestamptz NOT NULL DEFAULT now()
);

-- `updated_at` BİLİNÇLİ OLARAK YOK: bu fazda kullanıcı satırını güncelleyen
-- hiçbir akış yok. Profil/şifre değiştirme geldiğinde kendi migration'ıyla
-- eklenecek — kullanılmayan altyapıyı şimdiden taşımama kararının (Redis,
-- bkz. docs/0.0-constraints.md §2) şema tarafındaki karşılığı.

-- +goose Down
DROP TABLE IF EXISTS users;
