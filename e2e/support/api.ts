/**
 * Testin api'yi DOĞRUDAN çağırdığı yerler.
 *
 * NEDEN ARAYÜZDEN DEĞİL: EŞİK ALANLARI FORMDA YOK. `MonitorForm` ad/URL/metot
 * + aralık, timeout, degraded eşiği ve beklenen status aralığını gösteriyor;
 * `failureThreshold` ürünün varsayılanına (3) bırakılmış. Bunun için arayüz
 * UYDURMAK, E2E uğruna ürüne ekran eklemek olurdu — test ürünü değiştirmiyor,
 * ürünün gerçek yüzeyini kullanıyor.
 *
 * KAYIT 8.1'DEN BERİ ARAYÜZDEN YAPILIYOR. 7.2 bu dosyaya "kayıt ekranı yok
 * (2.1'in kullanıcı kararı), E2E'nin ilk adımı uçtan gitmek zorunda" diye
 * yazmıştı; 8.1 o ertelemeyi kapattı, yani gerekçe artık geçerli değil ve
 * ürünün ana akışının ilk adımı ("yabancı biri kaydolabiliyor") gerçekten
 * TARAYICIDAN ölçülüyor. Buradaki `logIn` yalnızca api-only çağrılara
 * (eşik) token bulmak için: arayüz access token'ı bilinçli olarak yalnızca
 * bellekte tutuyor (2.1), yani testin onu sayfadan okuması mümkün değil.
 */
import { API_URL } from './stack';

type Json = Record<string, unknown>;

async function call(
  method: string,
  path: string,
  body?: Json,
  token?: string,
): Promise<Json> {
  const response = await fetch(`${API_URL}${path}`, {
    method,
    headers: {
      ...(body ? { 'content-type': 'application/json' } : {}),
      ...(token ? { authorization: `Bearer ${token}` } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });

  const text = await response.text();

  if (!response.ok) {
    throw new Error(`${method} ${path} → ${response.status}: ${text}`);
  }

  return text ? (JSON.parse(text) as Json) : {};
}

export type Credentials = {
  email: string;
  password: string;
  name: string;
};

/**
 * newCredentials, bu koşuya ait benzersiz bir hesap üretir (henüz KAYDETMEZ).
 *
 * Adres her koşuda farklı: veritabanı her koşuda DROP+CREATE ediliyor (7.2)
 * ama testi tek başına elle çalıştıran biri aynı adrese ikinci kez kaydolup
 * 409 almasın.
 */
export function newCredentials(): Credentials {
  return {
    email: `e2e-${Date.now()}-${Math.floor(Math.random() * 10_000)}@example.test`,
    password: 'correct-horse-battery',
    name: 'E2E Kullanıcı',
  };
}

/** logIn, api'den bir access token alır (yalnızca api-only çağrılar için). */
export async function logIn(account: Credentials): Promise<string> {
  const payload = (await call('POST', '/v1/auth/login', {
    email: account.email,
    password: account.password,
  })) as { accessToken: string };

  return payload.accessToken;
}

/** firstOrgID, kullanıcının organizasyonunun kimliğini döner. */
export async function firstOrgID(token: string): Promise<string> {
  const payload = (await call('GET', '/v1/orgs', undefined, token)) as {
    organizations: { id: string }[];
  };

  if (payload.organizations.length === 0) {
    throw new Error('kullanıcının organizasyonu yok');
  }

  return payload.organizations[0].id;
}

/** monitorIDByName, ada göre monitor kimliğini bulur. */
export async function monitorIDByName(
  token: string,
  orgID: string,
  name: string,
): Promise<string> {
  const payload = (await call(
    'GET',
    `/v1/orgs/${orgID}/monitors`,
    undefined,
    token,
  )) as { monitors: { id: string; name: string }[] };

  const monitor = payload.monitors.find((row) => row.name === name);
  if (!monitor) {
    throw new Error(`"${name}" adlı monitor bulunamadı`);
  }

  return monitor.id;
}

/**
 * setFailureThreshold, kesinti eşiğini değiştirir.
 *
 * E2E bunu 1'e çekiyor: eşik monitor bazında (3.3, alt sınır 1) ve aralığın
 * alt sınırı 30 saniye (2.1). Varsayılan 3 eşikle kesintiyi görmek 90+ saniye
 * beklemek demekti. Ürün kuralı esnetilmiyor — YAPILANDIRMA kullanılıyor.
 */
export async function setFailureThreshold(
  token: string,
  orgID: string,
  monitorID: string,
  threshold: number,
): Promise<void> {
  await call(
    'PATCH',
    `/v1/orgs/${orgID}/monitors/${monitorID}`,
    { failureThreshold: threshold, checkIntervalSeconds: 30 },
    token,
  );
}
