-- +goose Up
-- Çok kiracılılığın (multi-tenancy) temeli: monitor, incident, status page —
-- Faz 2'den itibaren gelen her şey bir organizasyona ait olacak.
CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    name       text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),

    -- URL'de görünen ad. GLOBAL benzersiz: Faz 5.4'te public status page
    -- alt alan adına (slug.example.com) taşındığında isim alanı zaten
    -- küresel olacak; benzersizliği sonradan daraltmak, o güne kadar
    -- oluşmuş çakışan kayıtları taşımak demek olurdu.
    --
    -- Biçim veritabanında da zorlanıyor: uygulama slug'ı üretiyor ama
    -- kısıt son hakem. Küçük harf/rakam ve aralarında tek tire.
    slug       text NOT NULL UNIQUE
                    CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$')
                    CHECK (char_length(slug) BETWEEN 1 AND 63),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Kullanıcı ↔ organizasyon: çoktan çoğa, rolüyle birlikte.
-- Bir kullanıcı birden fazla organizasyona üye olabiliyor (0.1 §2).
CREATE TABLE organization_members (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    org_id    uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id   uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- 00001'de tanımlanan enum: owner | admin | member.
    role      org_role NOT NULL,

    joined_at timestamptz NOT NULL DEFAULT now(),

    -- Aynı kullanıcı bir organizasyona iki kez üye olamaz. Rol değişimi
    -- yeni satır değil UPDATE.
    UNIQUE (org_id, user_id)
);

-- "Bu kullanıcının organizasyonları" sorgusunun indeksi. (org_id yönü,
-- yukarıdaki UNIQUE kısıtının indeksinden zaten karşılanıyor: bileşik
-- indeksin ilk sütunu org_id.)
CREATE INDEX organization_members_user_id_idx ON organization_members (user_id);

-- +goose Down
DROP TABLE IF EXISTS organization_members;
DROP TABLE IF EXISTS organizations;
