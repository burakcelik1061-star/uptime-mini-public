-- +goose Up
-- Alan sözlüğü (bkz. docs/0.1-scope-and-data-model.md §4). Tablolar kendi
-- fazlarında gelecek; bu tipler birden fazla tablo tarafından paylaşıldığı
-- için tek bir migration'da tanımlanıyor.
--
-- Yeni bir değer eklemek ileride ayrı bir migration olur
-- (ALTER TYPE ... ADD VALUE) — enum'lar bilinçli tercih: metin sütununa göre
-- hem daha küçük hem yazım hatasına kapalı.

CREATE TYPE org_role AS ENUM ('owner', 'admin', 'member');

CREATE TYPE monitor_status AS ENUM ('pending', 'up', 'degraded', 'down', 'paused');

CREATE TYPE monitor_method AS ENUM ('GET', 'HEAD', 'POST');

-- Tek bir kontrolün sonucu. 'degraded' ayrı bir değer: "çalışıyor ama yavaş"
-- kontrol ANINDA karara bağlanıp yazılıyor (0.1 §5).
CREATE TYPE check_outcome AS ENUM ('success', 'degraded', 'failure');

-- Başarısız kontrolün sebebi — "hepsi hata" olarak toplanmıyor.
-- 'blocked_url': SSRF koruması (Faz 7.4) isteği hiç göndermeden reddetti.
CREATE TYPE check_error_type AS ENUM (
    'timeout',
    'dns',
    'connection_refused',
    'tls',
    'unexpected_status',
    'blocked_url',
    'unknown'
);

CREATE TYPE incident_status AS ENUM ('investigating', 'identified', 'monitoring', 'resolved');

CREATE TYPE incident_impact AS ENUM ('none', 'minor', 'major', 'critical');

-- +goose Down
DROP TYPE IF EXISTS incident_impact;
DROP TYPE IF EXISTS incident_status;
DROP TYPE IF EXISTS check_error_type;
DROP TYPE IF EXISTS check_outcome;
DROP TYPE IF EXISTS monitor_method;
DROP TYPE IF EXISTS monitor_status;
DROP TYPE IF EXISTS org_role;
