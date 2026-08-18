/**
 * ALT ALAN ADI → KİRACI ÇÖZÜMÜ (Faz 5.4).
 *
 * Bu projedeki "çok kiracılılık" sorusunun son parçası: bir isteğin hangi
 * organizasyona ait olduğunu, oturum olmadan, YALNIZCA ADRESTEN anlamak.
 *
 * Çözüm Next MIDDLEWARE'inde (0.2'nin stack kararında yazdığı gibi) ve
 * REWRITE ile: `acme.localhost:3100/` isteği sunucuda `/status/acme` olarak
 * çalışıyor. Yönlendirme (redirect) DEĞİL — ziyaretçinin adres çubuğunda
 * alt alan adı kalmalı, yoksa "kendi alan adım" hissi kaybolurdu ve her
 * ziyaret bir fazladan tur atardı.
 *
 * PATH TABANLI ERİŞİM DE ÇALIŞMAYA DEVAM EDİYOR (`/status/acme`): ikisi aynı
 * rotaya düştüğü için ikinci bir sayfa, ikinci bir veri yolu yok.
 */

/**
 * Ayrılmış etiketler: alt alan adı gibi görünen ama kiracı OLMAYAN adlar.
 *
 * `www` bir organizasyon olamaz (slug'ı alınsa bile ana siteyle karışırdı).
 * Liste kısa kalmalı — her yeni ad, kullanıcının alamayacağı bir slug demek.
 */
const RESERVED_LABELS = new Set(['www']);

/**
 * Slug biçimi — veritabanındaki CHECK ile AYNI (migration 00004/00014).
 *
 * Aynı olması şart: buradaki kural daha gevşek olsaydı, geçerli slug olamayacak
 * bir etiket sunucuya kadar gelip boş bir sorgu çalıştırırdı; daha sıkı
 * olsaydı geçerli bir sayfa alt alan adından erişilemezdi.
 */
const SLUG_PATTERN = /^[a-z0-9]+(-[a-z0-9]+)*$/;

/**
 * ROOT_DOMAIN, kiracı alt alan adlarının asıldığı ana alan adı.
 *
 * Local'de `localhost` — tarayıcılar `*.localhost` adreslerini kendiliğinden
 * 127.0.0.1'e çözüyor, yani hosts dosyasına dokunmadan `acme.localhost:3100`
 * çalışıyor (bkz. README).
 *
 * `NEXT_PUBLIC_` öneki YOK: bu değer yalnızca middleware'de, yani sunucuda
 * okunuyor.
 */
export const ROOT_DOMAIN = (
  process.env.STATUS_PAGE_ROOT_DOMAIN ?? 'localhost'
).toLowerCase();

/**
 * tenantSlugFromHost, `Host` başlığından kiracı slug'ını çıkarır.
 *
 * `null` = bu adres bir kiracı alt alan adı değil (ana alan adı, `www`, çok
 * seviyeli bir ad ya da slug biçimine uymayan bir etiket). O durumda istek
 * olduğu gibi devam ediyor — yani pano, giriş ekranı ve path tabanlı status
 * sayfası ana alan adında çalışmaya devam ediyor.
 *
 * Saf fonksiyon: girdisi bir dize, çıktısı bir dize. Port ayrıştırması da
 * burada, çünkü `Host` başlığı `acme.localhost:3100` biçiminde geliyor.
 */
export function tenantSlugFromHost(host: string | null): string | null {
  if (!host) return null;

  // IPv6 adresleri köşeli parantezle geliyor ([::1]:3100); kiracı adresi
  // olamazlar.
  if (host.startsWith('[')) return null;

  const hostname = host.split(':')[0].toLowerCase();

  if (hostname === ROOT_DOMAIN) return null;
  if (!hostname.endsWith(`.${ROOT_DOMAIN}`)) return null;

  const label = hostname.slice(0, -(ROOT_DOMAIN.length + 1));

  // Çok seviyeli adlar (a.b.localhost) kiracı sayılmıyor: slug tek bir
  // etikettir ve "hangi seviyeyi okuyacağız" sorusunun sessiz bir cevabı
  // olmamalı.
  if (label === '' || label.includes('.')) return null;

  if (RESERVED_LABELS.has(label)) return null;
  if (!SLUG_PATTERN.test(label)) return null;

  return label;
}
