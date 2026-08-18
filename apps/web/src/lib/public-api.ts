import type { PublicStatusPage } from './api-types';

/**
 * Public status sayfasının SUNUCU tarafındaki veri erişimi (Faz 5.1).
 *
 * `lib/api.ts`ten AYRI bir dosya ve bu bilinçli bir karar:
 *
 *   - O istemci, TARAYICIDA çalışıyor (`credentials: 'include'`, bellekteki
 *     access token, 401'de tazeleme). Public sayfanın hiçbirine ihtiyacı yok:
 *     oturum yok, token yok, cookie yok.
 *
 *   - Public sayfa SUNUCUDA render ediliyor (0.2'nin SSR/SEO kararı: bir
 *     status page'in arama motorunda ve link önizlemesinde görünmesi ürünün
 *     kendisi). Yani bu istek Next sunucusundan api'ye gidiyor —
 *     TARAYICIDAN DEĞİL. Sonucu: CORS bu yola hiç girmiyor, `WEB_ORIGINS`
 *     listesinin public sayfayla işi yok.
 *
 * Bu modül YALNIZCA server component'lerden import edilmeli. `server-only`
 * paketi bunu derleme zamanında zorlayabilirdi ama bir bağımlılık daha eklemek
 * yerine kural burada yazılı: aşağıdaki `API_INTERNAL_URL`in NEXT_PUBLIC_
 * öneki yok, yani istemci paketine düşerse zaten `undefined` olur ve hata
 * sessiz kalmaz.
 */

/**
 * API_INTERNAL_URL, Next SUNUCUSUNUN api'ye ulaştığı adres.
 *
 * NEXT_PUBLIC_ öneki YOK: bu değer tarayıcı paketine gömülmemeli, çünkü
 * tarayıcı bu isteği hiç yapmıyor. Konteynerli bir kurulumda api'nin servis
 * adı (http://api:3400) buraya yazılır; local'de varsayılan yeterli — depoyu
 * klonlayan biri hiçbir .env dosyası olmadan çalıştırabilmeli.
 */
const API_INTERNAL_URL =
  process.env.API_INTERNAL_URL ??
  process.env.NEXT_PUBLIC_API_URL ??
  'http://localhost:3400';

/**
 * fetchPublicStatus, slug'a karşılık gelen yayımlanmış sayfayı getirir.
 *
 * Bulunamazsa `null` döner — ATMAZ: "böyle bir sayfa yok" bir hata değil, bir
 * cevap (sunucu da yayımlanmamış sayfa ile olmayan slug'ı ayırt etmiyor).
 * Çağıran bunu `notFound()`a çeviriyor.
 *
 * `cache: 'no-store'`: status page'in tek işi ŞU ANKİ durumu söylemek.
 * Önbelleğe alınmış bir "tüm sistemler çalışıyor", devam eden bir kesinti
 * sırasında verilebilecek en yanlış cevap.
 */
export async function fetchPublicStatus(
  slug: string,
): Promise<PublicStatusPage | null> {
  const response = await fetch(
    `${API_INTERNAL_URL}/v1/public/status/${encodeURIComponent(slug)}`,
    { cache: 'no-store', headers: { Accept: 'application/json' } },
  );

  if (response.status === 404) return null;

  if (!response.ok) {
    // Diğer her şey (500, ağ hatası) gerçek bir arıza: Next'in hata sınırına
    // düşsün, "sayfa yok" gibi görünmesin.
    throw new Error(`status page okunamadı (${response.status})`);
  }

  return (await response.json()) as PublicStatusPage;
}

/**
 * Abonelik işlemlerinin sonucu (6.4).
 *
 * `ok` dışında bir ayrım YOK ve bu bilinçli: abonelik ucu enumeration'a kapalı
 * (sunucu da "zaten kayıtlı" demiyor), onay/iptal uçlarında ise geçersiz ve
 * kullanılmış token aynı cevabı alıyor.
 */
export type SubscriptionResult = 'ok' | 'invalid' | 'error';

/**
 * subscribeToStatusPage, ziyaretçiyi sayfaya abone eder.
 *
 * İSTEK SUNUCUDAN GİDİYOR (Next → api): 5.1'in "ziyaretçi tarayıcısı api'ye
 * hiç istek atmıyor" kararı okuma yolunda olduğu gibi yazma yolunda da
 * korunuyor. Tarayıcı yalnızca Next'e bir form gönderiyor.
 */
export async function subscribeToStatusPage(
  slug: string,
  email: string,
): Promise<SubscriptionResult> {
  return postPublic(
    `/v1/public/status/${encodeURIComponent(slug)}/subscribers`,
    { email },
  );
}

/** confirmSubscription, çift onayın ikinci adımı. */
export async function confirmSubscription(
  token: string,
): Promise<SubscriptionResult> {
  return postPublic('/v1/public/subscriptions/confirm', { token });
}

/** unsubscribeFromStatusPage, abonelikten çıkarır. */
export async function unsubscribeFromStatusPage(
  token: string,
): Promise<SubscriptionResult> {
  return postPublic('/v1/public/subscriptions/unsubscribe', { token });
}

/**
 * visitorForwardedFor, isteği YAPAN ziyaretçinin adresini api'ye taşır.
 *
 * NEDEN GEREKLİ: bu üç uç ziyaretçinin tarayıcısından değil BU SUNUCUDAN
 * çağrılıyor (5.1/6.4'ün kararı), yani api hepsini tek bir adres olarak
 * görüyor ve IP başına hız sınırı sayfa başına sınıra dönüşüyordu — beş
 * isteklik kota tüm ziyaretçilerin abonelik/onay/İPTAL yolunu kapatıyordu.
 *
 * NEDEN OKUMA YOLUNA EKLENMEDİ: `fetchPublicStatus` sayfanın kendi
 * render'ının parçası; ziyaretçinin tetiklediği bir YAZMA değil.
 *
 * GÜVENLİK SÖZLEŞMESİ: api bu başlığı YALNIZCA `TRUSTED_PROXIES` listesindeki
 * bir kaynaktan geldiğinde okuyor (varsayılan: boş, yani hiç). Next gelen
 * başlığı olduğu gibi geçiriyor — dolayısıyla listeye bu sunucuyu yazmak,
 * ancak ÖNÜNDE başlığı temizleyen gerçek bir proxy varsa doğrudur. Aksi
 * hâlde ziyaretçi kendi kimliğini seçebilir (0.4'ün `RealIP` gerekçesi).
 */
async function visitorForwardedFor(): Promise<Record<string, string>> {
  const { headers } = await import('next/headers');
  const incoming = await headers();

  const forwarded = incoming.get('x-forwarded-for');

  return forwarded ? { 'X-Forwarded-For': forwarded } : {};
}

async function postPublic(
  path: string,
  body: unknown,
): Promise<SubscriptionResult> {
  let response: Response;

  try {
    response = await fetch(`${API_INTERNAL_URL}${path}`, {
      method: 'POST',
      cache: 'no-store',
      headers: {
        'Content-Type': 'application/json',
        ...(await visitorForwardedFor()),
      },
      body: JSON.stringify(body),
    });
  } catch {
    return 'error';
  }

  if (response.ok) return 'ok';

  // 404 (token tanınmıyor) ve 422 (adres biçimsiz) kullanıcının
  // düzeltebileceği durumlar; gerisi gerçek arıza.
  if (response.status === 404 || response.status === 422) return 'invalid';

  return 'error';
}
