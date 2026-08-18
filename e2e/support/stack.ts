/**
 * E2E yığını: veritabanı + api + worker.
 *
 * KARAR (7.2, kullanıcı): api ve worker YEREL SÜREÇ olarak koşuyor,
 * konteynerde değil. Sebep tek bir tuzağı kapatmak: kontrolü yapan worker
 * konteynerde olsaydı, testin host'ta efemer bir portta ayağa kaldırdığı
 * hedef sunucuya `localhost` ile ULAŞAMAZDI — `host.docker.internal` gibi
 * platforma bağlı bir kapı ve Linux'ta `extra_hosts: host-gateway` gerekirdi.
 * Yerel süreçte tuzak kendiliğinden kapanıyor: worker hedefi `127.0.0.1` ile
 * görüyor. Konteyner imajlarının sağlığını CI'daki "Docker imajları" job'ı
 * zaten ölçüyor.
 *
 * Postgres yine COMPOSE'DAN: veritabanı sunucusunu ayrıca kurmak "klonlayan
 * herkes `docker compose up` ile çalıştırabilmeli" sözünün dışına çıkardı.
 */
import { execFile, spawn, type ChildProcess } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);

// `__dirname`, `import.meta.url` DEĞİL: Playwright TypeScript'i CommonJS'e
// çeviriyor ve `import.meta` orada tanımsız (turun ilk denemesinde
// "exports is not defined in ES module scope" ile patladı).
export const repoRoot = join(__dirname, '..', '..');
const backendDir = join(repoRoot, 'backend');
const binDir = join(repoRoot, 'e2e', '.bin');

/**
 * E2E'nin portları geliştirme yığınıyla ÇAKIŞMIYOR (web 3100 / api 3400):
 * bir E2E turu, açık duran `docker compose up -d` + `pnpm dev` ortamını
 * düşürmek zorunda kalmasın. Aynı gerekçe 0.2'de canny-mini için de
 * verilmişti.
 */
export const WEB_PORT = 3101;
export const API_PORT = 3401;
export const WEB_URL = `http://127.0.0.1:${WEB_PORT}`;
export const API_URL = `http://127.0.0.1:${API_PORT}`;

/**
 * AYRI VERİTABANI (kullanıcı kararı): E2E geliştirme verisini ne kirletiyor
 * ne de ona bağlı. Mevcut 12 monitor / 10 kesintilik geliştirme verisi,
 * testin gördüğü listeyi değiştirmemeli.
 *
 * 7.1'in `_test` KORUMASIYLA ÇAKIŞMIYOR: o koruma `internal/testdb`e ait ve
 * yalnızca Go testlerini bağlıyor. Bu harness o paketi HİÇ kullanmıyor —
 * şemayı üretimdeki `cmd/migrate` binary'siyle uyguluyor. Yani koruma
 * gevşetilmedi; iki dünya birbirine hiç değmiyor.
 */
const DB_NAME = 'uptime_mini_e2e';
const PG_USER = process.env.POSTGRES_USER ?? 'uptime';
const PG_PASSWORD = process.env.POSTGRES_PASSWORD ?? 'uptime_dev_password';
const PG_PORT = process.env.POSTGRES_PORT ?? '5433';

export const DATABASE_URL = `postgres://${PG_USER}:${PG_PASSWORD}@127.0.0.1:${PG_PORT}/${DB_NAME}?sslmode=disable`;

/**
 * İmzalama sırları SABİT: api bu turda yeniden başlamıyor, ama geçici anahtar
 * üretimi bir UYARI logluyor ve E2E'nin logu gürültüsüz kalsın.
 */
const JWT_ACCESS_SECRET = 'e2e-access-secret-0123456789abcdefghij';
const REFRESH_TOKEN_SECRET = 'e2e-refresh-secret-0123456789abcdefghij';

const processes: ChildProcess[] = [];

/** exe, Windows'ta `.exe` uzantısını ekler. */
function exe(name: string): string {
  return process.platform === 'win32' ? `${name}.exe` : name;
}

/** compose, `docker compose` komutunu repo kökünde çalıştırır. */
async function compose(...args: string[]): Promise<void> {
  await execFileAsync('docker', ['compose', ...args], { cwd: repoRoot });
}

/**
 * psql, komutu postgres KONTEYNERİNİN İÇİNDE çalıştırır.
 *
 * Host'ta `psql` kurulu olmasını beklemiyoruz: "hiçbir ek kurulum gerekmesin"
 * (GÜNCEL DURUM §3) E2E için de geçerli.
 *
 * `-h 127.0.0.1` ŞART, unix soketi DEĞİL — ve sebebi CI'da yaşandı: postgres
 * imajı ilk kurulumda (initdb) `listen_addresses=''` ile GEÇİCİ bir sunucu
 * ayağa kaldırıyor; o sunucu YALNIZCA unix soketinden erişilebiliyor. Soket
 * üzerinden yoklayan bir hazırlık kontrolü o geçici sunucuya bağlanıp
 * "hazır" diyor, hemen ardından geçici sunucu kapanıyor ve asıl komut
 * "No such file or directory" ile düşüyor. TCP ise init bitene kadar hiç
 * açılmıyor — yani doğru soruyu soruyor.
 */
async function psql(database: string, sql: string): Promise<void> {
  await execFileAsync(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      '-e',
      `PGPASSWORD=${PG_PASSWORD}`,
      'postgres',
      'psql',
      '-h',
      '127.0.0.1',
      '-U',
      PG_USER,
      '-d',
      database,
      '-v',
      'ON_ERROR_STOP=1',
      '-c',
      sql,
    ],
    { cwd: repoRoot },
  );
}

/**
 * resetDatabase, E2E veritabanını SIFIRDAN üretir.
 *
 * Her koşu TEMİZ başlamak zorunda (kullanıcı şartı): E2E'nin ilk adımı kayıt
 * olmak, yani önceki koşudan kalan bir kullanıcı ikinci koşuyu "e-posta
 * zaten kayıtlı" ile düşürürdü. TRUNCATE değil DROP: şema da her koşuda
 * migration'lardan yeniden doğsun.
 *
 * `WITH (FORCE)`: önceki koşudan kalmış bir bağlantı DROP'u bloke etmesin
 * (Postgres 13+).
 */
export async function resetDatabase(): Promise<void> {
  // `--wait`: compose'un kendi healthcheck'i (pg_isready) geçene kadar
  // bekliyor. Aşağıdaki döngü yine de duruyor — healthcheck soket üzerinden
  // yokluyor, yani yukarıdaki init yarışını tek başına kapatmıyor.
  await compose('up', '-d', '--wait', 'postgres');

  // Postgres TCP üzerinden sorgu kabul edene kadar bekle.
  await waitFor('postgres', async () => {
    try {
      await psql('postgres', 'SELECT 1');
      return true;
    } catch {
      return false;
    }
  });

  await psql('postgres', `DROP DATABASE IF EXISTS ${DB_NAME} WITH (FORCE)`);
  await psql('postgres', `CREATE DATABASE ${DB_NAME}`);
}

/**
 * buildBinaries, api/worker/migrate binary'lerini derler.
 *
 * `go run` KULLANILMIYOR: `go run` derleyip ÇOCUK bir süreç başlatıyor ve
 * onu öldürmek gerçek uygulamayı öldürmüyor (özellikle Windows'ta) — turdan
 * sonra ayakta kalan bir worker, bir sonraki koşunun veritabanına yazardı.
 */
export async function buildBinaries(): Promise<void> {
  mkdirSync(binDir, { recursive: true });

  for (const name of ['api', 'worker', 'migrate']) {
    await execFileAsync(
      'go',
      ['build', '-o', join(binDir, exe(name)), `./cmd/${name}`],
      { cwd: backendDir },
    );
  }
}

/**
 * migrate, şemayı uygular.
 *
 * Migration'lar üretimdeki binary'nin İÇİNDE gömülü (`go:embed`, 0.3): şemanın
 * ikinci bir kopyası (SQL dump, elle CREATE TABLE) çıkarılmıyor — çıksaydı
 * E2E, uygulamanın gerçekten kullandığından farklı bir şemaya karşı koşardı.
 */
export async function migrate(): Promise<void> {
  await execFileAsync(join(binDir, exe('migrate')), ['up'], {
    cwd: backendDir,
    env: { ...process.env, DATABASE_URL, APP_ENV: 'test' },
  });
}

/** start, bir binary'yi başlatır ve kapanışta öldürülmek üzere kaydeder. */
function start(name: string, env: Record<string, string>): ChildProcess {
  const child = spawn(join(binDir, exe(name)), [], {
    cwd: backendDir,
    env: { ...process.env, ...env },
    stdio: ['ignore', 'pipe', 'pipe'],
  });

  // Loglar sessizce yutulmuyor: E2E kırıldığında sebebi çoğu zaman api ya da
  // worker logunda. Playwright'ın raporunda görünsün diye stderr'e yazılıyor.
  child.stdout?.on('data', (chunk: Buffer) => {
    process.stderr.write(`[${name}] ${chunk.toString()}`);
  });
  child.stderr?.on('data', (chunk: Buffer) => {
    process.stderr.write(`[${name}] ${chunk.toString()}`);
  });

  child.on('exit', (code) => {
    if (code !== 0 && code !== null) {
      process.stderr.write(`[${name}] süreç ${code} ile çıktı\n`);
    }
  });

  processes.push(child);

  return child;
}

/** startApi, api sürecini başlatır ve /health cevap verene kadar bekler. */
export async function startApi(): Promise<void> {
  start('api', {
    APP_ENV: 'test',
    API_PORT: String(API_PORT),
    DATABASE_URL,
    // Manuel kontrol ucu da aynı kapıdan geçiyor (7.4).
    CHECKER_ALLOW_PRIVATE_TARGETS: 'true',
    // Tarayıcı web'i 127.0.0.1 üzerinden açıyor; CORS listesi TAM eşleşme
    // aradığı için (2.1) iki yazım da listede.
    WEB_ORIGINS: `http://127.0.0.1:${WEB_PORT},http://localhost:${WEB_PORT}`,
    JWT_ACCESS_SECRET,
    REFRESH_TOKEN_SECRET,
    PUBLIC_BASE_URL: WEB_URL,
  });

  await waitFor('api', async () => {
    try {
      const response = await fetch(`${API_URL}/health`);
      return response.ok;
    } catch {
      return false;
    }
  });
}

/**
 * startWorker, worker sürecini başlatır.
 *
 * `WORKER_SCHEDULER_PERIOD=1s`: tur aralığı kısaltılıyor (varsayılan 5s).
 * Monitor'ün KENDİ aralığı (en az 30 sn, 2.1'in kuralı) değişmiyor — yani
 * "ilk kontrol hemen" davranışı hızlanıyor, ürün kuralları değil.
 */
export async function startWorker(): Promise<void> {
  start('worker', {
    APP_ENV: 'test',
    DATABASE_URL,
    // SSRF KORUMASININ KAPISI (7.4): E2E'nin hedefi `127.0.0.1`de, yani
    // üretim politikası onu `blocked_url` ile reddederdi. Kapı yalnızca
    // APP_ENV=development|test iken açılabiliyor — bu yığın `test`.
    CHECKER_ALLOW_PRIVATE_TARGETS: 'true',
    WORKER_SCHEDULER_PERIOD: '1s',
    WORKER_HEARTBEAT_PERIOD: '2s',
    // Toplulaştırma/budama E2E'nin konusu değil; turda gereksiz iş yapmasın.
    WORKER_ROLLUP_PERIOD: '1h',
    WORKER_RETENTION_PERIOD: '1h',
  });

  // worker'ın HTTP portu yok; canlılığı kalp atışı dosyasından okunuyor (3.1).
  await waitFor('worker', async () => {
    try {
      await execFileAsync(join(binDir, exe('worker')), ['healthcheck'], {
        cwd: backendDir,
        env: { ...process.env, APP_ENV: 'test', WORKER_HEARTBEAT_PERIOD: '2s' },
      });
      return true;
    } catch {
      return false;
    }
  });
}

/** stopAll, başlatılan tüm süreçleri durdurur. */
export function stopAll(): void {
  for (const child of processes) {
    child.kill();
  }
  processes.length = 0;
}

/** waitFor, koşul sağlanana kadar (ya da süre dolana kadar) bekler. */
async function waitFor(
  what: string,
  check: () => Promise<boolean>,
  timeoutMs = 60_000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;

  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 250));
  }

  throw new Error(`${what} ${timeoutMs} ms içinde hazır olmadı`);
}
